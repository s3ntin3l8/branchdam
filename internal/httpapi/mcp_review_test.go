package httpapi

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"testing"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/auth/session"
	"github.com/s3ntin3l8/branchdam/internal/auth/users"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	attributionusers "github.com/s3ntin3l8/branchdam/internal/users"
)

func newMCPReviewServer(t *testing.T) (*Server, *db.DB, *auth.PATService, *audit.Service) {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "mcp-review.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	attr := attributionusers.NewService(database)
	if _, err := attr.EnsureSystemUser(context.Background()); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	base := config.Config{
		MCP:   config.MCP{Enabled: true},
		Authz: config.Authz{Groups: []string{"dam-admins"}},
		Auth:  config.Auth{Mode: string(auth.AuthModeBoth)},
		HTTP:  config.HTTP{TrustedProxies: []string{"192.0.2.1/32"}},
	}
	store, err := settings.NewStore(context.Background(), database, base, settingsTestKey(t), nil)
	require.NoError(t, err)

	userSvc := users.NewService(database, usersCookieTestBase64, users.ServiceOptions{
		CookieKey: usersCookieTestKey,
	})
	sessionMw := session.New(userSvc, session.Config{CookieName: "branchdam_session"})
	patSvc := auth.NewPATService(database, patTestPepper)
	auditSvc := audit.NewService(database, attr)
	srv := New(Deps{
		Config: &base, Settings: store, DB: database, Hub: sse.New(), Version: "test",
		Attribution: attr, Audit: auditSvc, PAT: patSvc,
		LocalAuth: &LocalAuthDeps{
			Users: userSvc, SessionMw: sessionMw, AuthMode: auth.AuthModeBoth,
		},
		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey),
	})
	return srv, database, patSvc, auditSvc
}

func TestMCPReviewTools(t *testing.T) {
	srv, database, patSvc, auditSvc := newMCPReviewServer(t)
	ctx := context.Background()
	locID := seedAuditLocation(t, database)
	seed := reviewEdgeSeed{tier: 2, resolver: "stem", confidence: 0.7}
	mcpEdge, _ := seedReviewEdge(t, database, locID, seed)
	restEdge, _ := seedReviewEdge(t, database, locID, seed)

	_, bobID := mintSessionForUser(t, database, srv.localAuth.users, "bob", false, "local")
	readToken, _, err := patSvc.Mint(ctx, bobID, "read", []string{"mcp:read"}, 0)
	require.NoError(t, err)
	reviewToken, reviewPAT, err := patSvc.Mint(ctx, bobID, "review", []string{"mcp:review"}, 0)
	require.NoError(t, err)

	handler := srv.Handler()
	readSession := dialMCP(t, handler, readToken)
	denied := callMCP(t, readSession, "confirm_edge", map[string]any{"id": mcpEdge})
	require.True(t, denied.IsError)
	assert.Contains(t, mcpText(denied), "mcp:review")
	unchanged, err := database.Reader.GetMediaEdge(ctx, mcpEdge)
	require.NoError(t, err)
	assert.Equal(t, "NEEDS_REVIEW", unchanged.ReviewState)
	assert.False(t, unchanged.ReviewSource.Valid)

	reviewSession := dialMCP(t, handler, reviewToken)
	confirmed := callMCP(t, reviewSession, "confirm_edge", map[string]any{"id": mcpEdge})
	require.False(t, confirmed.IsError, mcpText(confirmed))
	assert.NotContains(t, mcpText(confirmed), "stem")

	edge, err := database.Reader.GetMediaEdge(ctx, mcpEdge)
	require.NoError(t, err)
	assert.Equal(t, "CONFIRMED", edge.ReviewState)
	require.True(t, edge.ReviewSource.Valid)
	assert.Equal(t, "assistant", edge.ReviewSource.String)

	entries, _, err := auditSvc.ListActivity(ctx, audit.Filter{Event: audit.EventEdgeReviewConfirmed}, 20, 0)
	require.NoError(t, err)
	var patID int64
	var found bool
	for _, e := range entries {
		if e.ResourceID.Valid && e.ResourceID.String == strconv.FormatInt(mcpEdge, 10) {
			found = true
			require.True(t, e.PatID.Valid, "MCP confirm audit row must carry pat_id: %+v", e)
			patID = e.PatID.Int64
		}
	}
	require.True(t, found, "confirm audit row missing: %+v", entries)
	assert.Equal(t, reviewPAT.ID, patID)

	rr := serve(srv, adminReq(http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/confirm", restEdge), nil))
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	rest, err := database.Reader.GetMediaEdge(ctx, restEdge)
	require.NoError(t, err)
	assert.Equal(t, "CONFIRMED", rest.ReviewState)
	require.True(t, rest.ReviewSource.Valid)
	assert.Equal(t, "human", rest.ReviewSource.String)

	reset := callMCP(t, reviewSession, "reset_edge_review", map[string]any{"id": mcpEdge})
	require.False(t, reset.IsError, mcpText(reset))
	cleared, err := database.Reader.GetMediaEdge(ctx, mcpEdge)
	require.NoError(t, err)
	assert.Equal(t, "NEEDS_REVIEW", cleared.ReviewState)
	assert.False(t, cleared.ReviewSource.Valid)

	prompts, err := reviewSession.ListPrompts(ctx, nil)
	require.NoError(t, err)
	names := make([]string, 0, len(prompts.Prompts))
	for _, p := range prompts.Prompts {
		names = append(names, p.Name)
	}
	assert.ElementsMatch(t, []string{"triage-review-queue", "explain-asset"}, names)

	explain, err := reviewSession.GetPrompt(ctx, &sdkmcp.GetPromptParams{
		Name:      "explain-asset",
		Arguments: map[string]string{"id": "42"},
	})
	require.NoError(t, err)
	require.NotEmpty(t, explain.Messages)
	text, ok := explain.Messages[0].Content.(*sdkmcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, text.Text, "42")
	assert.Contains(t, text.Text, "untrusted")
	assert.Contains(t, text.Text, "agrees")

	triage, err := reviewSession.GetPrompt(ctx, &sdkmcp.GetPromptParams{Name: "triage-review-queue"})
	require.NoError(t, err)
	triageText, ok := triage.Messages[0].Content.(*sdkmcp.TextContent)
	require.True(t, ok)
	assert.Contains(t, triageText.Text, "untrusted")
	assert.Contains(t, triageText.Text, "agrees")
	assert.Contains(t, triageText.Text, "explicit edge id")
}
