package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/sse"
)

// TestListJobs_ReportsLocationAndTimestamps pins the jobs-table fields
// the Ingest Jobs page shows: which storage location a job scanned, when
// it started and (once done) when it finished.
func TestListJobs_ReportsLocationAndTimestamps(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	var locID, doneID, runningID int64
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "jobs-loc", RootPath: t.TempDir(), Tier: "TIER2_EXPORTS",
		})
		if err != nil {
			return err
		}
		locID = loc.ID
		done, err := q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{
			StorageLocationID: sql.NullInt64{Int64: locID, Valid: true}, Kind: "FULL_SCAN",
		})
		if err != nil {
			return err
		}
		doneID = done.ID
		if err := q.CompleteScanJob(ctx, done.ID); err != nil {
			return err
		}
		running, err := q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{
			StorageLocationID: sql.NullInt64{Int64: locID, Valid: true}, Kind: "INCREMENTAL",
		})
		runningID = running.ID
		return err
	}); err != nil {
		t.Fatalf("seed scan jobs: %v", err)
	}

	for _, path := range []string{"/api/v1/jobs", "/api/v1/progress"} {
		rr := doJSON(t, srv.Handler(), http.MethodGet, path, nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("GET %s: status %d, body %s", path, rr.Code, rr.Body.String())
		}
		var body struct {
			Jobs []struct {
				ID                int64  `json:"id"`
				StorageLocationID *int64 `json:"storageLocationId"`
				StartedAt         int64  `json:"startedAt"`
				UpdatedAt         int64  `json:"updatedAt"`
				FinishedAt        *int64 `json:"finishedAt"`
			} `json:"jobs"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		if len(body.Jobs) != 2 {
			t.Fatalf("GET %s: %d jobs, want 2", path, len(body.Jobs))
		}
		for _, j := range body.Jobs {
			if j.StorageLocationID == nil || *j.StorageLocationID != locID {
				t.Errorf("%s job %d: storageLocationId = %v, want %d", path, j.ID, j.StorageLocationID, locID)
			}
			if j.StartedAt == 0 || j.UpdatedAt == 0 {
				t.Errorf("%s job %d: startedAt=%d updatedAt=%d, want both set", path, j.ID, j.StartedAt, j.UpdatedAt)
			}
			switch j.ID {
			case doneID:
				if j.FinishedAt == nil || *j.FinishedAt < j.StartedAt {
					t.Errorf("%s completed job: finishedAt = %v, want >= startedAt %d", path, j.FinishedAt, j.StartedAt)
				}
			case runningID:
				if j.FinishedAt != nil {
					t.Errorf("%s running job: finishedAt = %d, want absent", path, *j.FinishedAt)
				}
			}
		}
	}
}

// TestHandleAudit_LoginStreamAppliesFilters: the audit log page's
// filters used to be silently ignored on the login stream (the query took
// no filters), so a filtered view showed every login. Event maps to the
// outcome, resourceId to the source, actor to user_id, and the time
// window to created_at; any resourceType but login_source matches
// nothing.
func TestHandleAudit_LoginStreamAppliesFilters(t *testing.T) {
	srv, _, usersSvc := newAuditHandlerServer(t)
	ctx := context.Background()
	system, err := usersSvc.EnsureSystemUser(ctx)
	if err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	systemID := system.ID
	seed := []struct {
		userID  any
		outcome string
		at      int64
	}{
		{nil, "bad-password", 1000},
		{systemID, "ok", 2000},
		{nil, "bad-password", 3000},
	}
	for _, r := range seed {
		if _, err := srv.db.ExecInTx(ctx,
			"INSERT INTO login_audit (user_id, username_presented, source, outcome, ip, user_agent, created_at) VALUES (?1, 'x', 'local', ?2, '127.0.0.1', 'test', ?3)",
			r.userID, r.outcome, r.at); err != nil {
			t.Fatalf("seed login_audit: %v", err)
		}
	}
	admin := auth.WithPrincipal(ctx, adminPrincipal())

	cases := []struct {
		name string
		in   AuditInput
		want int64
	}{
		{"unfiltered", AuditInput{}, 3},
		{"event", AuditInput{Event: "bad-password"}, 2},
		{"actor", AuditInput{ActorUserID: systemID}, 1},
		{"since", AuditInput{SinceUnix: 2000}, 2},
		{"until (exclusive)", AuditInput{UntilUnix: 2000}, 1},
		{"window and event", AuditInput{SinceUnix: 1500, UntilUnix: 3500, Event: "bad-password"}, 1},
		{"resourceId", AuditInput{ResourceID: "forward-noop"}, 0},
		{"login resourceType", AuditInput{ResourceType: "login_source"}, 3},
		{"other resourceType", AuditInput{ResourceType: "asset"}, 0},
	}
	for _, tc := range cases {
		in := tc.in
		in.Type, in.Limit = "login", 50
		out, err := srv.handleAudit(admin, &in)
		if err != nil {
			t.Fatalf("%s: handleAudit: %v", tc.name, err)
		}
		if out.Body.Total != tc.want || int64(len(out.Body.Entries)) != tc.want {
			t.Errorf("%s: total=%d entries=%d, want %d", tc.name, out.Body.Total, len(out.Body.Entries), tc.want)
		}
	}
}

// TestStorageHealth_PendingRestartClearsAfterRestart: the "pending
// restart" signal used to be "has any override", so it never cleared --
// an override persists across the restart that applies it. pendingRestart
// now compares the current overrides with the ones the process booted
// with.
func TestStorageHealth_PendingRestartClearsAfterRestart(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "pending-restart.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	base := config.Config{
		StorageLocations: []config.StorageLocation{
			{Name: "Tier 1", RootPath: "/data/tier1", Tier: "TIER1_LOCAL_SCRATCH", Watch: true},
		},
	}
	boot := func() *Server {
		store, err := settings.NewStore(context.Background(), database, base, settingsTestKey(t), nil)
		if err != nil {
			t.Fatalf("settings.NewStore: %v", err)
		}
		return New(Deps{Settings: store, DB: database, Hub: sse.New(), Version: "test", agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})
	}
	type loc struct {
		OverriddenFields []string `json:"overriddenFields"`
		PendingRestart   bool     `json:"pendingRestart"`
	}
	health := func(srv *Server) loc {
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/storage-health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/storage-health: %d %s", rec.Code, rec.Body.String())
		}
		var body struct {
			Locations []loc `json:"locations"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || len(body.Locations) != 1 {
			t.Fatalf("decode storage-health: %v (%s)", err, rec.Body.String())
		}
		return body.Locations[0]
	}
	apply := func(set map[string]any, unset []string) {
		if err := settings.ApplyStorageLocationOverride(context.Background(), database, "/data/tier1", set, unset, "tester"); err != nil {
			t.Fatalf("ApplyStorageLocationOverride: %v", err)
		}
	}

	srv := boot()
	seedTestStorageLocation(t, srv, "Tier 1", "/data/tier1", "TIER1_LOCAL_SCRATCH", false)
	if got := health(srv); got.PendingRestart {
		t.Fatalf("fresh boot, no overrides: pendingRestart = true")
	}

	// Setting an override to the value config.yaml already has changes
	// nothing a restart would apply.
	apply(map[string]any{"watch": true}, nil)
	if got := health(srv); got.PendingRestart || len(got.OverriddenFields) != 1 {
		t.Fatalf("no-op override: got %+v, want overridden but not pending", got)
	}

	apply(map[string]any{"sweep": true}, nil)
	if got := health(srv); !got.PendingRestart {
		t.Fatalf("after an override edit: pendingRestart = false, want true")
	}

	// "Restart": a new store snapshots the overrides it boots with.
	srv = boot()
	got := health(srv)
	if got.PendingRestart {
		t.Fatalf("after restart: pendingRestart = true, want false")
	}
	if len(got.OverriddenFields) == 0 {
		t.Fatalf("after restart: overriddenFields empty, the override itself must persist")
	}

	// Removing the override is again a change the next restart applies.
	apply(nil, []string{"sweep"})
	if got := health(srv); !got.PendingRestart {
		t.Fatalf("after unsetting an applied override: pendingRestart = false, want true")
	}
}
