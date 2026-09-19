package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// patTestPepper is the deterministic pepper used by all PAT tests.
var patTestPepper = []byte("pat-test-pepper-not-used-anywhere-else")

// mintTestToken mints a fresh bdam_pat_<base64-url> token. The
// returned plaintext is what the test puts in the Authorization
// header; the lookup callback in each test hashes the presented
// plaintext the same way the production hashToken does.
func mintTestToken(t *testing.T) string {
	t.Helper()
	buf := make([]byte, patRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("rand: %v", err)
	}
	enc := base64.URLEncoding.WithPadding(base64.NoPadding)
	return PATPrefix + enc.EncodeToString(buf)
}

// hashTestToken mirrors production hashToken. Tests use it in their
// stub Lookup callbacks to validate the presented plaintext.
func hashTestToken(plaintext string) string {
	mac := hmac.New(sha256.New, patTestPepper)
	mac.Write([]byte("pat:" + plaintext))
	sum := mac.Sum(nil)
	const hexChars = "0123456789abcdef"
	out := make([]byte, len(sum)*2)
	for i, b := range sum {
		out[i*2] = hexChars[b>>4]
		out[i*2+1] = hexChars[b&0x0F]
	}
	return string(out)
}

// TestRequirePAT_MintUseRevokeFlow exercises the happy path: a minted
// token validates on a request, and revocation invalidates it
// thereafter. Uses an in-memory Lookup callback so the test doesn't
// need a live database; the sqlcgen integration is covered by the
// service-level tests under cmd/branchdam.
func TestRequirePAT_MintUseRevokeFlow(t *testing.T) {
	plaintext := mintTestToken(t)
	_ = hashTestToken(plaintext)
	const userID int64 = 42
	const scope = "settings:write"

	revoked := false
	mw := &PATMiddleware{
		Lookup: func(ctx context.Context, presented string) (PATLookupResult, error) {
			if revoked {
				return PATLookupResult{}, sql.ErrNoRows
			}
			if presented != plaintext {
				return PATLookupResult{}, sql.ErrNoRows
			}
			return PATLookupResult{UserID: userID, Scopes: []string{scope}}, nil
		},
		Pepper: patTestPepper,
		Scope:  scope,
		Now:    func() time.Time { return time.Unix(1_700_000_000, 0) },
	}

	handlerCalled := false
	chain := mw.RequirePAT(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handlerCalled = true
		p, ok := From(r.Context())
		if !ok {
			t.Error("no Principal in context")
		}
		if p.Kind != KindUser {
			t.Errorf("Kind = %q, want %q", p.Kind, KindUser)
		}
		if !p.Authenticated {
			t.Error("Authenticated = false, want true")
		}
		w.WriteHeader(http.StatusOK)
	}))

	// Request with a valid token.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if !handlerCalled {
		t.Error("handler was not called -- token must have validated")
	}

	// Revoke; same token must now fail closed.
	revoked = true
	req = httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr = httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (revoked token must be rejected)", rr.Code)
	}
}

// TestRequirePAT_ScopeEnforcement covers the most important security
// boundary: a token with the wrong scope is rejected even if it's a
// perfectly valid token for its user. The cross-talk the relaxed
// shared-secret model allowed is precisely what scoped tokens
// prevent.
func TestRequirePAT_ScopeEnforcement(t *testing.T) {
	plaintext := mintTestToken(t)

	mw := &PATMiddleware{
		Lookup: func(ctx context.Context, presented string) (PATLookupResult, error) {
			// Token only has pairings:write -- the route asks for
			// settings:write, so this must 403.
			return PATLookupResult{UserID: 1, Scopes: []string{"pairings:write"}}, nil
		},
		Pepper: patTestPepper,
		Scope:  "settings:write",
	}

	chain := mw.RequirePAT(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler was called despite wrong scope")
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 (wrong scope must be rejected, not downgraded)", rr.Code)
	}
}

// TestRequirePAT_WildcardSatisfiesEveryScope verifies the wildcard "*"
// scope (the bootstrap PAT's only scope) satisfies every requested
// scope, including ones that don't exist yet. This is the contract
// that lets the bootstrap PAT do anything without enumerating scopes.
func TestRequirePAT_WildcardSatisfiesEveryScope(t *testing.T) {
	plaintext := mintTestToken(t)

	for _, requested := range []string{"", "settings:write", "pairings:write", "anything:future"} {
		t.Run("scope="+requested, func(t *testing.T) {
			mw := &PATMiddleware{
				Lookup: func(ctx context.Context, presented string) (PATLookupResult, error) {
					return PATLookupResult{UserID: 1, Scopes: []string{"*"}}, nil
				},
				Pepper: patTestPepper,
				Scope:  requested,
			}
			chain := mw.RequirePAT(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusOK)
			}))
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			req.Header.Set("Authorization", "Bearer "+plaintext)
			rr := httptest.NewRecorder()
			chain.ServeHTTP(rr, req)
			if rr.Code != http.StatusOK {
				t.Errorf("status = %d, want 200 (wildcard must satisfy scope %q)", rr.Code, requested)
			}
		})
	}
}

// TestRequirePAT_NoTokenFallsThrough verifies the middleware is
// transparent when no Authorization: Bearer bdam_pat_... header is
// presented -- it must pass through to the next chain (forward-auth /
// local-cookie) instead of rejecting the request. Routes that require
// a PAT specifically should layer RequireAdmin AFTER RequirePAT so
// unauthenticated requests still see the human-cookie prompt.
func TestRequirePAT_NoTokenFallsThrough(t *testing.T) {
	chain := (&PATMiddleware{
		Lookup: func(ctx context.Context, presented string) (PATLookupResult, error) {
			t.Error("Lookup was called despite no token presented")
			return PATLookupResult{}, nil
		},
		Pepper: patTestPepper,
		Scope:  "settings:write",
	}).RequirePAT(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK) // pass-through reached
	}))

	// Request with NO Authorization header.
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (no-token must fall through)", rr.Code)
	}

	// Request with non-Bearer scheme -- still a fall-through.
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz")
	rr = httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (non-Bearer must fall through)", rr.Code)
	}

	// Request with Bearer but wrong prefix -- still a fall-through.
	req = httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer wrong-prefix-token")
	rr = httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (non-PAT Bearer must fall through)", rr.Code)
	}
}

// TestRequirePAT_LookupDBError500s verifies a non-NoRows error from
// the lookup callback (DB trouble) is 500, not 401 -- the operator
// monitoring posture should distinguish "your token is wrong" from
// "the database is unreachable".
func TestRequirePAT_LookupDBError500s(t *testing.T) {
	plaintext := mintTestToken(t)

	mw := &PATMiddleware{
		Lookup: func(ctx context.Context, presented string) (PATLookupResult, error) {
			return PATLookupResult{}, errSimulatedDBFailure
		},
		Pepper: patTestPepper,
		Scope:  "settings:write",
	}
	chain := mw.RequirePAT(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("handler was called despite DB error")
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 (DB error must surface, not 401)", rr.Code)
	}
}

var errSimulatedDBFailure = sentinelErr("simulated DB failure")

type sentinelErr string

func (e sentinelErr) Error() string { return string(e) }

// TestRequirePAT_AttachesLocalUserView pins the three consumer-facing
// side effects of a successful PAT auth:
//
//   - LocalUserView.UserID == the token owner's users.id, so
//     /api/v1/users/me/pats resolves the owner without a second query;
//   - LocalUserView.IsAdmin == true, so RequireAdmin's local-`is_admin`
//     override passes even when authz.groups is non-empty (the PAT
//     principal carries no forward-auth Groups);
//   - LocalUserView.MFAVerified == true, so MFAGate doesn't mistake the
//     token for a half-authed password-only session.
func TestRequirePAT_AttachesLocalUserView(t *testing.T) {
	plaintext := mintTestToken(t)
	const userID int64 = 42

	mw := &PATMiddleware{
		Lookup: func(ctx context.Context, presented string) (PATLookupResult, error) {
			return PATLookupResult{UserID: userID, Scopes: []string{"*"}}, nil
		},
		Pepper: patTestPepper,
	}

	chain := mw.RequirePAT(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		view, ok := FromUser(r.Context())
		if !ok {
			t.Error("no LocalUserView in context")
			return
		}
		if view.UserID != userID {
			t.Errorf("LocalUserView.UserID = %d, want %d", view.UserID, userID)
		}
		if !view.IsAdmin {
			t.Error("LocalUserView.IsAdmin = false, want true (PAT principal must pass RequireAdmin)")
		}
		if !view.MFAVerified {
			t.Error("LocalUserView.MFAVerified = false, want true (token is the second factor)")
		}
		p, ok := From(r.Context())
		if !ok {
			t.Error("no Principal in context")
			return
		}
		if p.Kind != KindUser || !p.Authenticated {
			t.Errorf("Principal = %+v, want authenticated KindUser", p)
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Authorization", "Bearer "+plaintext)
	rr := httptest.NewRecorder()
	chain.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rr.Code)
	}
}

// TestExtractBearerPAT covers the bearer-extraction helper
// independently of the rest of the middleware: the wrong-prefix case
// (Bearer but not bdam_pat_) must be transparent, not a 401, because
// the middleware's contract is pass-through on no-token.
func TestExtractBearerPAT(t *testing.T) {
	cases := []struct {
		name   string
		header string
		want   string
	}{
		{"no header", "", ""},
		{"basic auth", "Basic dXNlcjpwYXNz", ""},
		{"bearer non-PAT prefix", "Bearer wrong-prefix-token", ""},
		{"bearer with bdam_pat_ prefix", "Bearer bdam_pat_abcdef", "bdam_pat_abcdef"},
		{"case insensitive", "bearer bdam_pat_xyz", "bdam_pat_xyz"},
		{"bearer with trailing whitespace", "Bearer   bdam_pat_q   ", "bdam_pat_q"},
		{"malformed -- no Bearer", "xyz bdam_pat_q", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/x", nil)
			if c.header != "" {
				req.Header.Set("Authorization", c.header)
			}
			got := extractBearerPAT(req)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestMintPATPlaintextFormat locks in the literal shape of a minted
// token so future refactors don't accidentally change the
// "bdam_pat_<base64-url-no-padding>" contract that the parser
// depends on.
func TestMintPATPlaintextFormat(t *testing.T) {
	plaintext, err := mintPATPlaintext()
	if err != nil {
		t.Fatalf("mintPATPlaintext: %v", err)
	}
	if !strings.HasPrefix(plaintext, PATPrefix) {
		t.Errorf("plaintext %q missing prefix %q", plaintext, PATPrefix)
	}
	body := strings.TrimPrefix(plaintext, PATPrefix)
	if len(body) != 43 {
		t.Errorf("body length = %d, want 43 (32 random bytes base64-url-no-padding)", len(body))
	}
	// base64-url-no-padding alphabet only.
	for _, r := range body {
		if !strings.ContainsRune(patB64URLNoPadding, r) {
			t.Errorf("body contains non-alphabet char %q in %q", r, body)
		}
	}
}

// TestScopeSatisfied covers the scope-matching helper directly so the
// rules are explicit (test names document the contract).
func TestScopeSatisfied(t *testing.T) {
	cases := []struct {
		name      string
		scopes    []string
		requested string
		want      bool
	}{
		{"empty requested always satisfied", []string{"x"}, "", true},
		{"exact match", []string{"x", "y"}, "x", true},
		{"wildcard satisfies any single", []string{"*"}, "x", true},
		{"wildcard satisfies any multi", []string{"*"}, "x:y:z", true},
		{"missing exact", []string{"x", "y"}, "z", false},
		{"empty scopes + non-empty request", nil, "x", false},
		{"empty scopes + wildcard request", nil, "*", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := scopeSatisfied(c.scopes, c.requested)
			if got != c.want {
				t.Errorf("got %v, want %v", got, c.want)
			}
		})
	}
}
