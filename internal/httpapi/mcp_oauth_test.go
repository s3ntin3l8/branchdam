package httpapi

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/auth/session"
	"github.com/s3ntin3l8/branchdam/internal/auth/users"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	attributionusers "github.com/s3ntin3l8/branchdam/internal/users"
)

func TestMCPOAuthResourceServer(t *testing.T) {
	key := oauthTestKey(t)
	const kid = "k1"
	const audience = "https://dam.example.com/mcp"
	issuer, closeIssuer := oauthTestIssuer(t, kid, key)
	t.Cleanup(closeIssuer)

	ctx := context.Background()
	database, err := db.Open(ctx, t.TempDir()+"/oauth.db")
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	attr := attributionusers.NewService(database)
	_, err = attr.EnsureSystemUser(ctx)
	require.NoError(t, err)

	cfg := config.Config{
		MCP: config.MCP{
			Enabled: true,
			OAuth: config.MCPOAuth{
				Issuer:   issuer,
				Audience: audience,
				ScopeClaims: map[string]map[string][]string{
					"groups": {
						"readers":   {"mcp:read"},
						"reviewers": {"mcp:review"},
					},
				},
			},
		},
		Auth: config.Auth{Mode: string(auth.AuthModeBoth)},
	}
	userSvc := users.NewService(database, usersCookieTestBase64, users.ServiceOptions{CookieKey: usersCookieTestKey})
	patSvc := auth.NewPATService(database, patTestPepper)
	auditSvc := audit.NewService(database, attr)
	srv := New(Deps{
		Config: &cfg, DB: database, Hub: sse.New(), Version: "test",
		Attribution: attr, Audit: auditSvc, PAT: patSvc,
		LocalAuth: &LocalAuthDeps{
			Users: userSvc, SessionMw: session.New(userSvc, session.Config{CookieName: "branchdam_session"}),
			AuthMode: auth.AuthModeBoth,
		},
	})
	handler := srv.Handler()

	_, err = database.ExecInTx(ctx,
		"INSERT INTO users (username, email, is_admin, source, created_at, created_by, auth_provider, external_uid) VALUES ('alice', 'alice@example.com', 0, 'forward-link', ?1, 'test', 'authentik', 'uid-alice')",
		time.Now().Unix(),
	)
	require.NoError(t, err)

	t.Run("metadata is public", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil))
		require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
		var doc auth.ProtectedResourceMetadata
		require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &doc))
		assert.Equal(t, audience, doc.Resource)
		require.Equal(t, []string{issuer}, doc.AuthorizationServers)
		assert.NotContains(t, rr.Body.String(), "secret")
		assert.Empty(t, rr.Header().Get("WWW-Authenticate"))
	})

	t.Run("missing token challenges", func(t *testing.T) {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, "/mcp", nil))
		require.Equal(t, http.StatusUnauthorized, rr.Code)
		assert.Contains(t, rr.Header().Get("WWW-Authenticate"), `resource_metadata="https://dam.example.com/.well-known/oauth-protected-resource"`)
		assert.NotContains(t, rr.Header().Get("WWW-Authenticate"), "invalid_token")
	})

	reject := func(t *testing.T, name, raw string) {
		t.Helper()
		t.Run(name, func(t *testing.T) {
			rr := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			req.Header.Set("Authorization", "Bearer "+raw)
			handler.ServeHTTP(rr, req)
			require.Equal(t, http.StatusUnauthorized, rr.Code, rr.Body.String())
			assert.Contains(t, rr.Header().Get("WWW-Authenticate"), "invalid_token")
			assert.Contains(t, rr.Header().Get("WWW-Authenticate"), "resource_metadata=")
		})
	}
	now := time.Now()
	reject(t, "wrong audience", oauthSign(t, key, kid, map[string]any{
		"iss": issuer, "sub": "uid-alice", "aud": "https://evil.example/mcp", "exp": now.Add(time.Hour).Unix(),
		"groups": []string{"readers"},
	}))
	reject(t, "expired", oauthSign(t, key, kid, map[string]any{
		"iss": issuer, "sub": "uid-alice", "aud": audience, "exp": now.Add(-2 * time.Hour).Unix(),
		"groups": []string{"readers"},
	}))
	other := oauthTestKey(t)
	reject(t, "bad signature", oauthSign(t, other, kid, map[string]any{
		"iss": issuer, "sub": "uid-alice", "aud": audience, "exp": now.Add(time.Hour).Unix(),
		"groups": []string{"readers"},
	}))

	t.Run("no mcp group is 403", func(t *testing.T) {
		raw := oauthSign(t, key, kid, map[string]any{
			"iss": issuer, "sub": "uid-alice", "aud": audience, "exp": now.Add(time.Hour).Unix(),
			"groups": []string{"visitors"},
		})
		rr := postBearer(handler, raw)
		require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Header().Get("WWW-Authenticate"), "insufficient_scope")
	})

	t.Run("unknown subject is not provisioned", func(t *testing.T) {
		raw := oauthSign(t, key, kid, map[string]any{
			"iss": issuer, "sub": "uid-ghost", "aud": audience, "exp": now.Add(time.Hour).Unix(),
			"groups": []string{"readers"},
		})
		rr := postBearer(handler, raw)
		require.Equal(t, http.StatusForbidden, rr.Code, rr.Body.String())
		assert.Contains(t, rr.Body.String(), "not provisioned")
		_, err := database.Reader.GetAttributionUserByExternalUID(ctx, sqlcgen.GetAttributionUserByExternalUIDParams{
			AuthProvider: "authentik", ExternalUid: "uid-ghost",
		})
		require.ErrorIs(t, err, sql.ErrNoRows)
		_, err = database.Reader.GetAttributionUserByExternalUID(ctx, sqlcgen.GetAttributionUserByExternalUIDParams{
			AuthProvider: "oauth", ExternalUid: "uid-ghost",
		})
		require.ErrorIs(t, err, sql.ErrNoRows)
	})

	t.Run("valid token lists tools", func(t *testing.T) {
		raw := oauthSign(t, key, kid, map[string]any{
			"iss": issuer, "sub": "uid-alice", "aud": audience, "exp": now.Add(time.Hour).Unix(),
			"groups": []string{"readers"},
		})
		session := dialMCP(t, handler, raw)
		listed, err := session.ListTools(ctx, nil)
		require.NoError(t, err)
		require.NotEmpty(t, listed.Tools)
	})

	t.Run("review audit is oauth", func(t *testing.T) {
		locID := seedAuditLocation(t, database)
		edgeID, _ := seedReviewEdge(t, database, locID, reviewEdgeSeed{tier: 2, resolver: "stem", confidence: 0.7})
		raw := oauthSign(t, key, kid, map[string]any{
			"iss": issuer, "sub": "uid-alice", "aud": audience, "exp": now.Add(time.Hour).Unix(),
			"groups": []string{"reviewers"},
		})
		session := dialMCP(t, handler, raw)
		confirmed := callMCP(t, session, "confirm_edge", map[string]any{"id": edgeID})
		require.False(t, confirmed.IsError, mcpText(confirmed))
		edge, err := database.Reader.GetMediaEdge(ctx, edgeID)
		require.NoError(t, err)
		assert.Equal(t, "assistant", edge.ReviewSource.String)
		entries, _, err := auditSvc.ListActivity(ctx, audit.Filter{Event: audit.EventEdgeReviewConfirmed}, 20, 0)
		require.NoError(t, err)
		var found bool
		for _, e := range entries {
			if e.ResourceID.Valid && e.ResourceID.String == strconv.FormatInt(edgeID, 10) {
				found = true
				require.True(t, e.AuthProvider.Valid)
				assert.Equal(t, "oauth", e.AuthProvider.String)
				assert.False(t, e.PatID.Valid)
			}
		}
		require.True(t, found)
	})

	t.Run("pat still works", func(t *testing.T) {
		_, bobID := mintSessionForUser(t, database, srv.localAuth.users, "bob", false, "local")
		token, _, err := patSvc.Mint(ctx, bobID, "read", []string{"mcp:read"}, 0)
		require.NoError(t, err)
		session := dialMCP(t, handler, token)
		listed, err := session.ListTools(ctx, nil)
		require.NoError(t, err)
		require.NotEmpty(t, listed.Tools)
	})
}

func postBearer(handler http.Handler, raw string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer "+raw)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	return rr
}

func oauthTestKey(t *testing.T) *ecdsa.PrivateKey {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	return key
}

func oauthTestIssuer(t *testing.T, kid string, key *ecdsa.PrivateKey) (string, func()) {
	t.Helper()
	pub := jose.JSONWebKey{Key: &key.PublicKey, KeyID: kid, Use: "sig", Algorithm: string(jose.ES256)}
	set := jose.JSONWebKeySet{Keys: []jose.JSONWebKey{pub}}
	var iss string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/.well-known/openid-configuration":
			_ = json.NewEncoder(w).Encode(map[string]string{"issuer": iss, "jwks_uri": iss + "/jwks"})
		case "/jwks":
			_ = json.NewEncoder(w).Encode(set)
		default:
			http.NotFound(w, r)
		}
	}))
	iss = srv.URL
	return iss, srv.Close
}

func oauthSign(t *testing.T, key *ecdsa.PrivateKey, kid string, claims map[string]any) string {
	t.Helper()
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", kid)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.ES256, Key: key}, opts)
	require.NoError(t, err)
	raw, err := jwt.Signed(signer).Claims(claims).Serialize()
	require.NoError(t, err)
	return raw
}
