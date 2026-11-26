package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"

	"github.com/s3ntin3l8/branchdam/internal/clock"
)

// oauthAllowedAlgs is the only signature set /mcp will verify. HMAC
// algorithms are absent on purpose: an HS256 token is signed with a
// client secret this server does not hold, and Authentik falls back to
// that when a provider has no signing key. "none" is absent too.
var oauthAllowedAlgs = []jose.SignatureAlgorithm{
	jose.EdDSA,
	jose.RS256, jose.RS384, jose.RS512,
	jose.ES256, jose.ES384, jose.ES512,
	jose.PS256, jose.PS384, jose.PS512,
}

const (
	oauthKeyTTL          = 5 * time.Minute
	oauthRefreshCooldown = 30 * time.Second
	// oauthKeyStale is how long a previously fetched key set may still
	// verify tokens when the issuer cannot be reached. A successful
	// refresh drops a removed key immediately. A failed refresh does
	// not, for at most this long.
	oauthKeyStale  = time.Hour
	oauthBodyLimit = 1 << 20
)

// OAuthIdentity is a verified access token. Claims still carry the
// raw payload so scope mapping can read whatever the operator named.
type OAuthIdentity struct {
	Subject string
	Claims  map[string]any
}

// ProtectedResourceMetadata is the RFC 9728 document served at
// /.well-known/oauth-protected-resource. It names the authorization
// server and nothing else.
type ProtectedResourceMetadata struct {
	Resource               string   `json:"resource"`
	AuthorizationServers   []string `json:"authorization_servers"`
	ScopesSupported        []string `json:"scopes_supported"`
	BearerMethodsSupported []string `json:"bearer_methods_supported"`
}

// OAuthValidator checks audience-bound JWTs against an issuer's JWKS.
// The issuer URL is operator config, not a value taken from the token.
type OAuthValidator struct {
	issuer      string
	audience    string
	metadataURL string
	scopeClaims map[string]map[string][]string
	client      *http.Client

	mu        sync.Mutex
	fetchWait *sync.Cond
	fetching  bool
	keys      jose.JSONWebKeySet
	keysAt    time.Time
}

// NewOAuthValidator rejects an issuer or audience that is not an
// absolute http(s) URL. http is accepted so tests can use a local
// JWKS; production issuers are https.
func NewOAuthValidator(issuer, audience string, scopeClaims map[string]map[string][]string) (*OAuthValidator, error) {
	issuer = strings.TrimSpace(issuer)
	audience = strings.TrimSpace(audience)
	meta, err := OAuthMetadataURL(audience)
	if err != nil {
		return nil, err
	}
	iss, err := url.Parse(issuer)
	if err != nil || !httpURL(iss) {
		return nil, errors.New("oauth: issuer must be an absolute http(s) url")
	}
	v := &OAuthValidator{
		issuer:      issuer,
		audience:    audience,
		metadataURL: meta,
		scopeClaims: scopeClaims,
		client: &http.Client{
			Timeout: 5 * time.Second,
			// A redirect would let the issuer point sameHost at itself
			// and then hop to an attacker's key set. Discovery and JWKS
			// are fetched only at the URLs we already checked.
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("oauth: redirects are not followed")
			},
		},
	}
	v.fetchWait = sync.NewCond(&v.mu)
	return v, nil
}

func (v *OAuthValidator) Issuer() string      { return v.issuer }
func (v *OAuthValidator) Audience() string    { return v.audience }
func (v *OAuthValidator) MetadataURL() string { return v.metadataURL }

func (v *OAuthValidator) ScopeClaims() map[string]map[string][]string {
	return v.scopeClaims
}

func (v *OAuthValidator) Metadata() ProtectedResourceMetadata {
	return ProtectedResourceMetadata{
		Resource:               v.audience,
		AuthorizationServers:   []string{v.issuer},
		ScopesSupported:        []string{"mcp:read", "mcp:review", "mcp:enrich"},
		BearerMethodsSupported: []string{"header"},
	}
}

// OAuthMetadataURL is the absolute protected-resource metadata URL for
// an audience. The audience is the MCP resource identifier
// (https://dam.example.com/mcp); the document lives at the origin.
func OAuthMetadataURL(audience string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(audience))
	if err != nil || !httpURL(u) {
		return "", errors.New("oauth: audience must be an absolute http(s) url")
	}
	meta := u.Scheme + "://" + u.Host + "/.well-known/oauth-protected-resource"
	if strings.ContainsAny(meta, "\"\\\n\r ") {
		return "", errors.New("oauth: audience is not safe to put in a header")
	}
	return meta, nil
}

func httpURL(u *url.URL) bool {
	return u != nil && u.Host != "" && (u.Scheme == "https" || u.Scheme == "http")
}

// Validate verifies raw as a signed JWT for this resource. A bad
// signature, the wrong issuer or audience, a missing subject, or an
// expired token is an error. The caller maps that to 401.
func (v *OAuthValidator) Validate(ctx context.Context, raw string, now time.Time) (OAuthIdentity, error) {
	tok, err := jwt.ParseSigned(raw, oauthAllowedAlgs)
	if err != nil {
		return OAuthIdentity{}, fmt.Errorf("oauth: parse token: %w", err)
	}
	ident, err := v.claims(ctx, tok, false)
	if err != nil && errors.Is(err, jose.ErrJWKSKidNotFound) {
		ident, err = v.claims(ctx, tok, true)
	}
	if err != nil {
		return OAuthIdentity{}, err
	}
	if err := ident.registered.ValidateWithLeeway(jwt.Expected{
		Issuer:      v.issuer,
		AnyAudience: jwt.Audience{v.audience},
		Time:        now,
	}, time.Minute); err != nil {
		return OAuthIdentity{}, fmt.Errorf("oauth: %w", err)
	}
	if ident.registered.Expiry == nil {
		return OAuthIdentity{}, errors.New("oauth: token has no exp")
	}
	if ident.registered.Subject == "" {
		return OAuthIdentity{}, errors.New("oauth: token has no sub")
	}
	return OAuthIdentity{Subject: ident.registered.Subject, Claims: ident.extra}, nil
}

type verifiedClaims struct {
	registered jwt.Claims
	extra      map[string]any
}

func (v *OAuthValidator) claims(ctx context.Context, tok *jwt.JSONWebToken, force bool) (verifiedClaims, error) {
	set, err := v.keySet(ctx, force)
	if err != nil {
		return verifiedClaims{}, err
	}
	var registered jwt.Claims
	extra := map[string]any{}
	if err := tok.Claims(set, &registered, &extra); err != nil {
		return verifiedClaims{}, fmt.Errorf("oauth: verify signature: %w", err)
	}
	return verifiedClaims{registered: registered, extra: extra}, nil
}

func (v *OAuthValidator) keySet(ctx context.Context, force bool) (jose.JSONWebKeySet, error) {
	v.mu.Lock()
	for {
		age := time.Duration(0)
		if !v.keysAt.IsZero() {
			age = clock.Real().Sub(v.keysAt)
		}
		fresh := !v.keysAt.IsZero() && age < oauthKeyTTL
		if !force && fresh {
			set := v.keys
			v.mu.Unlock()
			return set, nil
		}
		// An unknown kid may refresh, but not more than once per cooldown,
		// and never for a bad signature. The HTTP fetch is not under mu.
		if force && !v.keysAt.IsZero() && age < oauthRefreshCooldown {
			set := v.keys
			v.mu.Unlock()
			return set, nil
		}
		if v.fetching {
			v.fetchWait.Wait()
			force = false
			continue
		}
		v.fetching = true
		v.mu.Unlock()
		set, err := v.fetchKeys(ctx)
		v.mu.Lock()
		v.fetching = false
		if err != nil {
			staleOK := !v.keysAt.IsZero() && clock.Real().Sub(v.keysAt) < oauthKeyStale
			v.fetchWait.Broadcast()
			if staleOK {
				cached := v.keys
				v.mu.Unlock()
				return cached, nil
			}
			v.mu.Unlock()
			return jose.JSONWebKeySet{}, err
		}
		v.keys = set
		v.keysAt = clock.Real()
		v.fetchWait.Broadcast()
		v.mu.Unlock()
		return set, nil
	}
}

func (v *OAuthValidator) fetchKeys(ctx context.Context) (jose.JSONWebKeySet, error) {
	disc := strings.TrimRight(v.issuer, "/") + "/.well-known/openid-configuration"
	var doc struct {
		Issuer string `json:"issuer"`
		JWKS   string `json:"jwks_uri"`
	}
	if err := v.getJSON(ctx, disc, &doc); err != nil {
		return jose.JSONWebKeySet{}, err
	}
	if doc.Issuer != v.issuer {
		return jose.JSONWebKeySet{}, errors.New("oauth: discovery issuer does not match configured issuer")
	}
	if !sameHost(v.issuer, doc.JWKS) {
		return jose.JSONWebKeySet{}, errors.New("oauth: jwks_uri host does not match issuer")
	}
	var set jose.JSONWebKeySet
	if err := v.getJSON(ctx, doc.JWKS, &set); err != nil {
		return jose.JSONWebKeySet{}, err
	}
	if len(set.Keys) == 0 {
		return jose.JSONWebKeySet{}, errors.New("oauth: jwks is empty")
	}
	return set, nil
}

func sameHost(issuer, raw string) bool {
	a, errA := url.Parse(issuer)
	b, errB := url.Parse(raw)
	if errA != nil || errB != nil || !httpURL(a) || !httpURL(b) {
		return false
	}
	return strings.EqualFold(a.Host, b.Host) && a.Scheme == b.Scheme
}

func (v *OAuthValidator) getJSON(ctx context.Context, rawURL string, dest any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	resp, err := v.client.Do(req)
	if resp != nil && resp.Body != nil {
		defer func() { _ = resp.Body.Close() }()
	}
	if err != nil {
		return fmt.Errorf("oauth: get %s: %w", rawURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("oauth: get %s: status %d", rawURL, resp.StatusCode)
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, oauthBodyLimit)).Decode(dest); err != nil {
		return fmt.Errorf("oauth: decode %s: %w", rawURL, err)
	}
	return nil
}

// MapOAuthScopes turns configured claim values into mcp:read,
// mcp:review and mcp:enrich. Any other mapped string is dropped, so
// the config cannot grant admin or a wildcard. A token's own scope
// claim is not trusted unless the operator mapped it.
func MapOAuthScopes(claims map[string]any, mapping map[string]map[string][]string) []string {
	if len(mapping) == 0 || len(claims) == 0 {
		return nil
	}
	seen := map[string]struct{}{}
	var out []string
	for claim, values := range mapping {
		raw, ok := claims[claim]
		if !ok {
			continue
		}
		for _, value := range oauthClaimStrings(raw) {
			for _, scope := range values[value] {
				if scope != "mcp:read" && scope != "mcp:review" && scope != "mcp:enrich" {
					continue
				}
				if _, ok := seen[scope]; ok {
					continue
				}
				seen[scope] = struct{}{}
				out = append(out, scope)
			}
		}
	}
	return out
}

func oauthClaimStrings(raw any) []string {
	switch v := raw.(type) {
	case string:
		if v == "" {
			return nil
		}
		return []string{v}
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			s, ok := item.(string)
			if ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// ContextWithOAuth attaches an OAuth principal and the scopes
// HasScope reads. The same context key as a PAT is deliberate: tool
// checks call HasScope and do not grow a second path.
func ContextWithOAuth(ctx context.Context, p Principal, scopes []string, isAdmin bool) context.Context {
	copied := append([]string(nil), scopes...)
	ctx = withPrincipal(ctx, p)
	ctx = withPATScopes(ctx, copied)
	return WithLocalUserView(ctx, LocalUserView{
		UserID:      p.UserID,
		IsAdmin:     isAdmin,
		MFAVerified: true,
	})
}

// OAuthUnauthorized writes the 401 a client uses to discover the
// authorization server. invalidToken sets error=invalid_token; a
// missing credential leaves it off so the client starts discovery
// instead of treating the challenge as a rejected token.
func OAuthUnauthorized(w http.ResponseWriter, metadataURL string, invalidToken bool) {
	challenge := `Bearer resource_metadata="` + metadataURL + `"`
	if invalidToken {
		challenge = `Bearer error="invalid_token", resource_metadata="` + metadataURL + `"`
	}
	w.Header().Set("WWW-Authenticate", challenge)
	http.Error(w, "authentication required", http.StatusUnauthorized)
}

// OAuthForbidden writes 403. insufficientScope is a valid token whose
// claims map to no mcp:* grant. Otherwise the subject is not an
// existing forward-auth user, and this server does not create one.
func OAuthForbidden(w http.ResponseWriter, metadataURL string, insufficientScope bool) {
	if insufficientScope {
		w.Header().Set("WWW-Authenticate", `Bearer error="insufficient_scope", scope="mcp:read mcp:review mcp:enrich", resource_metadata="`+metadataURL+`"`)
		http.Error(w, "forbidden: mcp scope required", http.StatusForbidden)
		return
	}
	w.Header().Set("WWW-Authenticate", `Bearer resource_metadata="`+metadataURL+`"`)
	http.Error(w, "forbidden: user is not provisioned", http.StatusForbidden)
}
