package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

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

// newAuditAttributionServer is newReviewTestServer plus a PAT service and
// a local session stack in auth mode "both". Forward-auth headers are
// trusted from httptest's RemoteAddr (192.0.2.1) so the same Handler()
// can confirm an edge as a PAT, as forward-auth, and as a local session.
func newAuditAttributionServer(t *testing.T) (*Server, *db.DB, *auth.PATService, *users.Service) {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "audit-attr.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	attr := attributionusers.NewService(database)
	if _, err := attr.EnsureSystemUser(context.Background()); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	base := config.Config{
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
		Settings: store, DB: database, Hub: sse.New(), Version: "test",
		Attribution: attr, Audit: auditSvc, PAT: patSvc,
		LocalAuth: &LocalAuthDeps{
			Users: userSvc, SessionMw: sessionMw, AuthMode: auth.AuthModeBoth,
		},
		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey),
	})
	return srv, database, patSvc, userSvc
}

// TestAuditActivity_RecordsCredential is the acceptance test for recording
// which credential confirmed an edge. An admin PAT, a forward-auth request
// and a local session each confirm a different edge; the activity log
// distinguishes them.
func TestAuditActivity_RecordsCredential(t *testing.T) {
	srv, database, patSvc, userSvc := newAuditAttributionServer(t)
	ctx := context.Background()
	locID := seedAuditLocation(t, database)
	seed := reviewEdgeSeed{tier: 2, resolver: "stem", confidence: 0.7}
	patEdge, _ := seedReviewEdge(t, database, locID, seed)
	fwdEdge, _ := seedReviewEdge(t, database, locID, seed)
	localEdge, _ := seedReviewEdge(t, database, locID, seed)

	cookie, adminID := mintSessionForUser(t, database, userSvc, "localadmin", true, "local")
	token, minted, err := patSvc.Mint(ctx, adminID, "claude-desktop", []string{"*"}, 0)
	require.NoError(t, err)

	patReq := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/confirm", patEdge), nil)
	patReq.Header.Set("Authorization", "Bearer "+token)
	if rr := serve(srv, patReq); rr.Code != http.StatusOK {
		t.Fatalf("pat confirm: %d %s", rr.Code, rr.Body.String())
	}
	if rr := serve(srv, adminReq(http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/confirm", fwdEdge), nil)); rr.Code != http.StatusOK {
		t.Fatalf("forward-auth confirm: %d %s", rr.Code, rr.Body.String())
	}
	if rr := serve(srv, withSessionCookie(http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/confirm", localEdge), cookie, nil)); rr.Code != http.StatusOK {
		t.Fatalf("local confirm: %d %s", rr.Code, rr.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/api/v1/audit?type=activity&limit=50", nil)
	get.Header.Set("Authorization", "Bearer "+token)
	rr := serve(srv, get)
	if rr.Code != http.StatusOK {
		t.Fatalf("audit: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Entries []map[string]any `json:"entries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v body=%s", err, rr.Body.String())
	}
	byEdge := map[string]map[string]any{}
	for _, e := range body.Entries {
		if e["event"] == audit.EventEdgeReviewConfirmed {
			id, _ := e["resourceId"].(string)
			byEdge[id] = e
		}
	}
	patRow := byEdge[strconv.FormatInt(patEdge, 10)]
	fwdRow := byEdge[strconv.FormatInt(fwdEdge, 10)]
	localRow := byEdge[strconv.FormatInt(localEdge, 10)]
	if patRow == nil || fwdRow == nil || localRow == nil {
		t.Fatalf("confirm rows missing: %+v", byEdge)
	}
	if patRow["authProvider"] != "pat" || patRow["patName"] != "claude-desktop" {
		t.Errorf("pat entry = %v", patRow)
	}
	if got, ok := patRow["patId"].(float64); !ok || int64(got) != minted.ID {
		t.Errorf("patId = %v (%T), want %d", patRow["patId"], patRow["patId"], minted.ID)
	}
	if _, ok := fwdRow["patId"]; !ok || fwdRow["patId"] != nil || fwdRow["authProvider"] != "authentik" {
		t.Errorf("forward-auth entry = %v, want authProvider authentik and patId null", fwdRow)
	}
	if fwdRow["patName"] != nil {
		t.Errorf("forward-auth patName = %v, want absent", fwdRow["patName"])
	}
	if _, ok := localRow["patId"]; !ok || localRow["patId"] != nil || localRow["authProvider"] != "local" {
		t.Errorf("local entry = %v, want authProvider local and patId null", localRow)
	}
}
