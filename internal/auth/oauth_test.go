package auth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

func testOAuthIssuer(t *testing.T, kid string, key *ecdsa.PrivateKey) (issuer string, closeFn func()) {
	t.Helper()
	pub := jose.JSONWebKey{Key: &key.PublicKey, KeyID: kid, Use: "sig", Algorithm: string(jose.ES256)}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{pub}}
	var iss string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{
				"issuer":   iss,
				"jwks_uri": iss + "/jwks",
			})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(set)
		default:
			http.NotFound(w, r)
		}
	}))
	iss = srv.URL
	return iss, srv.Close
}

func signOAuth(t *testing.T, key *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, opts)
	if err != nil {
		t.Fatalf("signer: %v", err)
	}
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	if err != nil {
		t.Fatalf("serialize: %v", err)
	}
	return raw
}

func newTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	return key
}

func TestOAuthValidatorAcceptsAudienceBoundToken(t *testing.T) {
	key := newTestKey(t)
	issuer, done := testOAuthIssuer(t, "k1", key)
	defer done()
	const audience = "https://dam.example.com/mcp"
	v, err := NewOAuthValidator(issuer, audience, nil)
	if err != nil {
		t.Fatalf("NewOAuthValidator: %v", err)
	}
	raw := signOAuth(t, key, "k1", map[string]any{
		"iss": issuer, "sub": "uid-alice", "aud": audience,
		"exp":    time.Now().Add(time.Hour).Unix(),
		"groups": []string{"reviewers"},
	})
	ident, err := v.Validate(context.Background(), raw, time.Now())
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if ident.Subject != "uid-alice" {
		t.Errorf("sub = %q", ident.Subject)
	}
	scopes := MapOAuthScopes(ident.Claims, map[string]map[string][]string{
		"groups": {"reviewers": {"mcp:review", "admin", "*"}},
	})
	if len(scopes) != 1 || scopes[0] != "mcp:review" {
		t.Errorf("scopes = %v, want [mcp:review] with admin and wildcard dropped", scopes)
	}
}

func TestOAuthValidatorRejectsBadTokens(t *testing.T) {
	key := newTestKey(t)
	other := newTestKey(t)
	issuer, done := testOAuthIssuer(t, "k1", key)
	defer done()
	const audience = "https://dam.example.com/mcp"
	v, err := NewOAuthValidator(issuer, audience, nil)
	if err != nil {
		t.Fatalf("NewOAuthValidator: %v", err)
	}
	now := time.Now()
	cases := []struct {
		name   string
		claims map[string]any
		key    *ecdsa.PrivateKey
		kid    string
	}{
		{"wrong audience", map[string]any{"iss": issuer, "sub": "uid", "aud": "https://evil.example/mcp", "exp": now.Add(time.Hour).Unix()}, key, "k1"},
		{"expired", map[string]any{"iss": issuer, "sub": "uid", "aud": audience, "exp": now.Add(-2 * time.Hour).Unix()}, key, "k1"},
		{"no exp", map[string]any{"iss": issuer, "sub": "uid", "aud": audience}, key, "k1"},
		{"bad signature", map[string]any{"iss": issuer, "sub": "uid", "aud": audience, "exp": now.Add(time.Hour).Unix()}, other, "k1"},
		{"hmac is not pinned", nil, nil, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var raw string
			if tc.name == "hmac is not pinned" {
				secret := []byte("client-secret-not-a-public-key!!")
				opts := (&jose.SignerOptions{}).WithType("JWT")
				signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.HS256, Key: secret}, opts)
				if err != nil {
					t.Fatalf("hmac signer: %v", err)
				}
				raw, err = jwt.Signed(signer).Claims(map[string]any{
					"iss": issuer, "sub": "uid", "aud": audience, "exp": now.Add(time.Hour).Unix(),
				}).Serialize()
				if err != nil {
					t.Fatalf("serialize: %v", err)
				}
			} else {
				raw = signOAuth(t, tc.key, tc.kid, tc.claims)
			}
			if _, err := v.Validate(context.Background(), raw, now); err == nil {
				t.Fatal("Validate succeeded, want error")
			}
		})
	}
}

func TestOAuthMetadataURL(t *testing.T) {
	got, err := OAuthMetadataURL("https://dam.example.com/mcp")
	if err != nil {
		t.Fatal(err)
	}
	if got != "https://dam.example.com/.well-known/oauth-protected-resource" {
		t.Errorf("metadata = %q", got)
	}
	if _, err := NewOAuthValidator("not a url", "https://dam.example.com/mcp", nil); err == nil {
		t.Fatal("bad issuer accepted")
	}
}
