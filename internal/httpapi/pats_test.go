package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// patTestPepper is deterministic; tests never share it with pairing
// tests' pepper because the two services are constructed independently
// in production too (both read cfg.Pairing.Pepper, but each test wires
// its own).
var patTestPepper = []byte("httpapi-pat-test-pepper-0123456789ab")

// newPATTestServer wires a Server with a real SQLite DB, a PAT service,
// and an agent API key -- enough to exercise the full Handler() chain:
// PAT middleware -> authzHandler (RequireAdmin) -> handlers.
func newPATTestServer(t *testing.T) (*Server, *db.DB, *auth.PATService, int64) {
	t.Helper()
	root := t.TempDir()
	database, err := db.Open(context.Background(), filepath.Join(root, "pats.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	// A users row must exist before Mint: user_pats.user_id is a
	// RESTRICT FK. EnsureBootstrapUser + PromoteUserToAdmin is the same
	// sequence the boot sequence runs -- the promotion matters: the
	// PAT lookup query joins users and filters on is_admin=1, so an
	// un-promoted owner's token 401s (authority is live-checked, not
	// frozen at mint).
	var userID int64
	require.NoError(t, database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		uid, err := q.EnsureBootstrapUser(context.Background())
		if err != nil {
			return err
		}
		if err := q.PromoteUserToAdmin(context.Background(), uid); err != nil {
			return err
		}
		userID = uid
		return nil
	}))

	patSvc := auth.NewPATService(database, patTestPepper)
	srv := New(Deps{
		Config:  &config.Config{Agent: config.Agent{}},
		DB:      database,
		Version: "test",
		PAT:     patSvc,

		AgentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})
	return srv, database, patSvc, userID
}

// TestPAT_EndpointsViaBearerToken exercises the wiring added with the
// PAT service: mint via the service, then list/mint/revoke through the
// HTTP API authenticated by the Bearer token alone (no session cookie,
// no forward-auth headers).
func TestPAT_EndpointsViaBearerToken(t *testing.T) {
	srv, _, patSvc, userID := newPATTestServer(t)
	ctx := context.Background()
	handler := srv.Handler()

	// Mint a wildcard admin PAT directly through the service -- this
	// stands in for the bootstrap PAT an operator would have on disk.
	token, _, err := patSvc.Mint(ctx, userID, "bootstrap", []string{"*"}, 0)
	require.NoError(t, err)
	require.True(t, strings.HasPrefix(token, auth.PATPrefix))

	// GET /api/v1/users/me/pats with the Bearer token.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me/pats", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "GET list body: %s", rr.Body.String())
	var decoded struct {
		PATs []map[string]any `json:"pats"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &decoded))
	require.Len(t, decoded.PATs, 1)
	assert.Equal(t, "bootstrap", decoded.PATs[0]["name"])
	assert.NotEmpty(t, decoded.PATs[0]["hashedKeyPrefix"])
	_, leaksPlaintext := decoded.PATs[0]["plaintext"]
	assert.False(t, leaksPlaintext, "list response must never include the plaintext token")

	// POST /api/v1/users/me/pats -- mint a second PAT over the API.
	mintBody := `{"name":"ansible","scopes":["*"]}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/users/me/pats", strings.NewReader(mintBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "POST mint body: %s", rr.Body.String())
	var minted struct {
		Plaintext string `json:"plaintext"`
		ID        int64  `json:"id"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &minted))
	require.True(t, strings.HasPrefix(minted.Plaintext, auth.PATPrefix), "minted plaintext must carry the prefix")

	// Revoke the minted PAT -- must 200.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/users/me/pats/"+strconv.FormatInt(minted.ID, 10)+"/revoke", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "POST revoke body: %s", rr.Body.String())

	// The revoked token must no longer authenticate: a list with it
	// returns 401.
	req = httptest.NewRequest(http.MethodGet, "/api/v1/users/me/pats", nil)
	req.Header.Set("Authorization", "Bearer "+minted.Plaintext)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code, "revoked token must fail closed")
}

// TestPAT_AgentPathNotBypassed pins the security property of the
// wiring: a PAT header on an agent route must NOT short-circuit
// AgentChain. Without the agent API key the request is rejected by the
// agent chain -- a 200 here would mean the PAT bypass reached an agent
// handler.
func TestPAT_AgentPathNotBypassed(t *testing.T) {
	srv, _, patSvc, userID := newPATTestServer(t)
	token, _, err := patSvc.Mint(context.Background(), userID, "bootstrap", []string{"*"}, 0)
	require.NoError(t, err)

	handler := srv.Handler()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	require.NotEqual(t, http.StatusOK, rr.Code,
		"PAT header on agent route must not reach the agent handler with a user principal")
}

// TestPAT_NoTokenFallsThroughToExistingChains verifies requests without
// a PAT header still get the unauthenticated-browser treatment (403
// from RequireAdmin on the gated write path, not a PAT 401).
func TestPAT_NoTokenFallsThroughToExistingChains(t *testing.T) {
	srv, _, _, _ := newPATTestServer(t)
	handler := srv.Handler()

	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me/pats", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	// No identity at all: RequireAdmin reports "authentication
	// required" (403), NOT 401-from-PAT -- the PAT chain only speaks
	// when a token was presented.
	assert.Equal(t, http.StatusForbidden, rr.Code)
}

// TestPAT_ScopeEnforcementPerRouteGroup pins the review finding that
// the PAT middleware was wired with an empty scope (scopes_json
// restricted nothing). A narrowly-scoped token must pass only its own
// route group: "pairings:write" gets through the pairings group but
// is 403'd at the PAT-management group, and vice versa for a
// "pats:write" token.
func TestPAT_ScopeEnforcementPerRouteGroup(t *testing.T) {
	srv, _, patSvc, userID := newPATTestServer(t)
	ctx := context.Background()
	handler := srv.Handler()

	pairingsToken, _, err := patSvc.Mint(ctx, userID, "pairings-only", []string{"pairings:write"}, 0)
	require.NoError(t, err)
	patsToken, _, err := patSvc.Mint(ctx, userID, "pats-only", []string{"pats:write"}, 0)
	require.NoError(t, err)

	// pairings-scoped token on the PAT-management group -> 403.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me/pats", nil)
	req.Header.Set("Authorization", "Bearer "+pairingsToken)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code, "pairings:write token must not reach the PAT group")

	// pats-scoped token on the PAT-management group -> passes the
	// scope gate and the RequireAdmin gate (200).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/users/me/pats", nil)
	req.Header.Set("Authorization", "Bearer "+patsToken)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code, "pats:write token must pass its own group")

	// pats-scoped token on the pairings group -> 403. (The pairing
	// service is nil in this test server, so a scope-passing request
	// would 503 from pairingSvc(), not 403 -- 403 proves the scope
	// gate rejected it.)
	req = httptest.NewRequest(http.MethodGet, "/api/v1/companion/pairings", nil)
	req.Header.Set("Authorization", "Bearer "+patsToken)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code, "pats:write token must not reach the pairings group")

	// pairings-scoped token on the pairings group -> passes the scope
	// gate (503 = pairing service not configured, i.e. past auth).
	req = httptest.NewRequest(http.MethodGet, "/api/v1/companion/pairings", nil)
	req.Header.Set("Authorization", "Bearer "+pairingsToken)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusServiceUnavailable, rr.Code, "pairings:write token must pass its own group")
}

// TestPAT_RevokeUnknownID404 pins the review finding that Revoke
// reported ok:true for ids that matched no live row of the caller --
// a silent false success that would leave an operator believing a
// token was dead. Today the handler must 404.
func TestPAT_RevokeUnknownID404(t *testing.T) {
	srv, _, patSvc, userID := newPATTestServer(t)
	token, _, err := patSvc.Mint(context.Background(), userID, "bootstrap", []string{"*"}, 0)
	require.NoError(t, err)

	handler := srv.Handler()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/pats/999999/revoke", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusNotFound, rr.Code, "revoking an unknown id must 404, body: %s", rr.Body.String())
}

// TestPAT_DemotedOwnerFailsClosed pins the live authority check: the
// lookup query joins users and filters on is_admin=1, so demoting the
// owner invalidates every live token of theirs -- the token minted
// while admin must 401 after the demotion, exactly like a revoked
// token.
func TestPAT_DemotedOwnerFailsClosed(t *testing.T) {
	srv, database, patSvc, userID := newPATTestServer(t)
	ctx := context.Background()
	token, _, err := patSvc.Mint(ctx, userID, "bootstrap", []string{"*"}, 0)
	require.NoError(t, err)

	handler := srv.Handler()
	req := httptest.NewRequest(http.MethodGet, "/api/v1/users/me/pats", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "token must authenticate while the owner is admin")

	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.DemoteUserFromAdmin(ctx, userID)
	}))

	req = httptest.NewRequest(http.MethodGet, "/api/v1/users/me/pats", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusUnauthorized, rr.Code,
		"demoted owner's token must fail closed (authority is live-checked, not frozen at mint)")
}

// TestPAT_MintScopeCap pins the round-3 finding that scopes acted as
// route gates but not as a blast-radius cap: without this, a
// ["pats:write"] token could mint itself a ["*"] token, making every
// scoped grant a full-admin credential in disguise. A PAT caller may
// only mint scopes it itself carries (unless it is the wildcard).
func TestPAT_MintScopeCap(t *testing.T) {
	srv, _, patSvc, userID := newPATTestServer(t)
	ctx := context.Background()
	handler := srv.Handler()

	patsToken, _, err := patSvc.Mint(ctx, userID, "pats-only", []string{"pats:write"}, 0)
	require.NoError(t, err)

	// Minting a wildcard with a pats:write token -> 403.
	mintBody := `{"name":"escalation","scopes":["*"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/pats", strings.NewReader(mintBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+patsToken)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code,
		"pats:write token must not mint a wildcard token, body: %s", rr.Body.String())

	// Minting within its own scope -> 200.
	mintBody = `{"name":"sibling","scopes":["pats:write"]}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/users/me/pats", strings.NewReader(mintBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+patsToken)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code,
		"pats:write token may mint another pats:write token, body: %s", rr.Body.String())

	// A foreign scope (not the wildcard, not one it carries) -> 403.
	mintBody = `{"name":"escalation-2","scopes":["pairings:write"]}`
	req = httptest.NewRequest(http.MethodPost, "/api/v1/users/me/pats", strings.NewReader(mintBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+patsToken)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusForbidden, rr.Code,
		"pats:write token must not mint a foreign scope, body: %s", rr.Body.String())
}

// TestPAT_MintExpiresAtPast: an expiry in the past mints a token that
// 401s on first use -- the silent-dead-credential shape. Reject with
// 400 at the boundary.
func TestPAT_MintExpiresAtPast(t *testing.T) {
	srv, _, patSvc, userID := newPATTestServer(t)
	token, _, err := patSvc.Mint(context.Background(), userID, "bootstrap", []string{"*"}, 0)
	require.NoError(t, err)

	handler := srv.Handler()
	mintBody := `{"name":"dead-on-arrival","scopes":["*"],"expiresAt":1}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/pats", strings.NewReader(mintBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"expiresAt in the past must 400, body: %s", rr.Body.String())
}

// TestPAT_MintUnknownScope400 pins the round-4 allowlist: a scope no
// route group consults would mint a token that authenticates but
// passes no scope gate. The handler must 400 with the allowed set.
func TestPAT_MintUnknownScope400(t *testing.T) {
	srv, _, patSvc, userID := newPATTestServer(t)
	token, _, err := patSvc.Mint(context.Background(), userID, "bootstrap", []string{"*"}, 0)
	require.NoError(t, err)

	handler := srv.Handler()
	mintBody := `{"name":"dead-scope","scopes":["settings:write"]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/pats", strings.NewReader(mintBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"unknown scope must 400, body: %s", rr.Body.String())
}

// TestPAT_MintEmptyScopes400: an empty scope list mints a token that
// 403s on every route group -- the silent-dead-credential shape.
// 400 at the boundary.
func TestPAT_MintEmptyScopes400(t *testing.T) {
	srv, _, patSvc, userID := newPATTestServer(t)
	token, _, err := patSvc.Mint(context.Background(), userID, "bootstrap", []string{"*"}, 0)
	require.NoError(t, err)

	handler := srv.Handler()
	mintBody := `{"name":"no-scopes","scopes":[]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users/me/pats", strings.NewReader(mintBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusBadRequest, rr.Code,
		"empty scope list must 400, body: %s", rr.Body.String())
}
