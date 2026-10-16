package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	attributionusers "github.com/s3ntin3l8/branchdam/internal/users"
)

// TestPAT_AttributesToTokenOwner pins the audit finding that a
// PAT-authenticated request provisioned a phantom ('authentik',
// 'pat:<hash prefix>') users row -- an admin under the default empty
// authz.groups -- and attributed pairings, uploads, scans and actor_audit
// rows to it instead of the token's owner. With attribution and audit
// wired, a PAT request must resolve to the owner's existing users.id,
// carry the owner's username, and create no users row at all.
func TestPAT_AttributesToTokenOwner(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "pat-attr.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	var ownerID int64
	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		uid, err := q.EnsureBootstrapUser(ctx)
		if err != nil {
			return err
		}
		ownerID = uid
		return q.PromoteUserToAdmin(ctx, uid)
	}))
	owner, err := database.Reader.GetAttributionUserByID(ctx, ownerID)
	require.NoError(t, err)

	usersSvc := attributionusers.NewService(database)
	_, err = usersSvc.EnsureSystemUser(ctx)
	require.NoError(t, err)
	patSvc := auth.NewPATService(database, patTestPepper)
	srv := New(Deps{
		Config:      &config.Config{Agent: config.Agent{}},
		DB:          database,
		Version:     "test",
		PAT:         patSvc,
		Attribution: usersSvc,
		Audit:       audit.NewService(database, usersSvc),

		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})
	handler := srv.Handler()

	token, _, err := patSvc.Mint(ctx, ownerID, "ops", []string{"*"}, 0)
	require.NoError(t, err)

	listUsers := func() []sqlcgen.ListAttributionUsersRow {
		rows, err := database.Reader.ListAttributionUsers(ctx, sqlcgen.ListAttributionUsersParams{Limit: 100})
		require.NoError(t, err)
		return rows
	}
	before := listUsers()

	// /api/v1/me resolves attribution through ResolveOrCreate.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("Authorization", "Bearer "+token)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "GET /me body: %s", rr.Body.String())
	var me struct {
		Name              string `json:"name"`
		AttributionUserID int64  `json:"attributionUserId"`
		IsAdmin           bool   `json:"isAdmin"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &me))
	assert.Equal(t, ownerID, me.AttributionUserID, "a PAT request must be attributed to the token owner")
	assert.Equal(t, owner.Username, me.Name, "the PAT principal must carry the owner's username")
	assert.True(t, me.IsAdmin)

	// A write route records actor_audit through audit.resolveActor.
	req = httptest.NewRequest(http.MethodPost, "/api/v1/users/me/pats", strings.NewReader(`{"name":"second","scopes":["*"]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, "POST mint body: %s", rr.Body.String())

	auditRows, err := database.Reader.ListActorAudit(ctx, sqlcgen.ListActorAuditParams{Limit: 10, Event: audit.EventPATMinted})
	require.NoError(t, err)
	require.Len(t, auditRows, 1)
	assert.Equal(t, ownerID, auditRows[0].ActorUserID.Int64, "actor_audit must name the token owner")
	assert.Equal(t, owner.Username, auditRows[0].ActorName)

	after := listUsers()
	assert.Len(t, after, len(before), "a PAT request must not provision any users row")
	for _, u := range after {
		assert.False(t, strings.HasPrefix(u.ExternalUid, "pat:"), "phantom PAT user provisioned: %+v", u)
	}
}

// TestResolveActorUserID_PATPrincipalUsesOwner covers the resolver every
// attribution write path (web upload, scan, pairing) goes through.
func TestResolveActorUserID_PATPrincipalUsesOwner(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "pat-resolve.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	var ownerID int64
	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		ownerID, err = q.EnsureBootstrapUser(ctx)
		return err
	}))
	usersSvc := attributionusers.NewService(database)

	pctx := auth.WithPrincipal(ctx, auth.Principal{
		Kind:          auth.KindUser,
		Name:          "ansible-bootstrap",
		ExternalUID:   "pat:deadbeef",
		AuthProvider:  auth.AuthProviderPAT,
		UserID:        ownerID,
		Authenticated: true,
	})
	assert.Equal(t, ownerID, resolveActorUserID(pctx, usersSvc, nil))

	_, err = database.Reader.GetAttributionUserByExternalUID(ctx, sqlcgen.GetAttributionUserByExternalUIDParams{
		AuthProvider: attributionusers.AuthProviderAuthentik,
		ExternalUid:  "pat:deadbeef",
	})
	assert.Error(t, err, "no phantom ('authentik', 'pat:...') row may exist")
}
