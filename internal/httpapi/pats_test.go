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
	// RESTRICT FK. EnsureBootstrapUser is the same query the boot
	// sequence uses.
	var userID int64
	require.NoError(t, database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		uid, err := q.EnsureBootstrapUser(context.Background())
		if err != nil {
			return err
		}
		userID = uid
		return nil
	}))

	patSvc := auth.NewPATService(database, patTestPepper)
	srv := New(Deps{
		Config:  &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:      database,
		Version: "test",
		PAT:     patSvc,
	})
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
