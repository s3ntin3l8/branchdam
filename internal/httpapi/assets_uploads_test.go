package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	attributionusers "github.com/s3ntin3l8/branchdam/internal/users"
)

// seedMediaNodeWithUploader inserts a minimal media_nodes row with the
// given uploaded_by_user_id. Used by the "My uploads" filter test.
func seedMediaNodeWithUploader(t *testing.T, srv *Server, filePath, storageLocName string, uploadedBy sql.NullInt64) int64 {
	t.Helper()
	// Find or create the storage location id.
	locs, err := srv.db.Reader.ListStorageLocations(context.Background())
	if err != nil {
		t.Fatalf("list storage locations: %v", err)
	}
	var locID int64
	for _, l := range locs {
		if l.Name == storageLocName {
			locID = l.ID
			break
		}
	}
	if locID == 0 {
		err := srv.db.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			r, e := q.UpsertStorageLocation(context.Background(), sqlcgen.UpsertStorageLocationParams{
				Name: storageLocName, RootPath: "/tmp/" + storageLocName, Tier: "TIER1_LOCAL_SCRATCH",
			})
			locID = r.ID
			return e
		})
		if err != nil {
			t.Fatalf("seed storage location: %v", err)
		}
	}
	// Now insert the media_node.
	res, err := srv.db.ExecInTx(context.Background(),
		`INSERT INTO media_nodes (node_uuid, storage_location_id, file_path, file_name, file_ext, size_bytes, mtime_unix, indexing_status, graph_status, lifecycle_state, uploaded_by_user_id) VALUES (?, ?, ?, ?, ?, ?, unixepoch(), 'INDEXED_FULL', 'UNLINKED', 'ACTIVE', ?)`,
		"uuid-"+filePath, locID, filePath, filepath.Base(filePath), filepath.Ext(filePath), 1024, uploadedBy)
	if err != nil {
		t.Fatalf("seed media node: %v", err)
	}
	id, _ := res.LastInsertId()
	return id
}

// TestHandleListAssets_UploadedByUserIdFilter: the new
// uploadedByUserId query parameter narrows the list to assets that
// carry the given attribution FK. Seeds three nodes (alice, bob, NULL)
// and verifies the filter returns exactly the expected rows.
func TestHandleListAssets_UploadedByUserIdFilter(t *testing.T) {
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "uploads-filter.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	usersSvc := attributionusers.NewService(database)
	if _, err := usersSvc.EnsureSystemUser(context.Background()); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	base := config.Config{Authz: config.Authz{Groups: []string{"dam-admins"}}}
	store, err := settings.NewStore(context.Background(), database, base, settingsTestKey(t), nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}
	srv := New(Deps{
		Settings: store, DB: database, Hub: sse.New(), Version: "test",
		Attribution: usersSvc,
		Audit:       audit.NewService(database, usersSvc),
	})

	// Provision alice + bob via ResolveOrCreate (real attribution rows).
	alice, err := usersSvc.ResolveOrCreate(context.Background(), auth.Principal{
		Kind: auth.KindUser, Name: "alice", ExternalUID: "alice-uid", Authenticated: true,
	})
	if err != nil {
		t.Fatalf("alice: %v", err)
	}
	bob, err := usersSvc.ResolveOrCreate(context.Background(), auth.Principal{
		Kind: auth.KindUser, Name: "bob", ExternalUID: "bob-uid", Authenticated: true,
	})
	if err != nil {
		t.Fatalf("bob: %v", err)
	}

	// Seed four media nodes: alice x2, bob x1, unattributed x1.
	seedMediaNodeWithUploader(t, srv, "/data/alice/a.jpg", "uploads-loc", sql.NullInt64{Int64: alice.ID, Valid: true})
	seedMediaNodeWithUploader(t, srv, "/data/alice/b.jpg", "uploads-loc", sql.NullInt64{Int64: alice.ID, Valid: true})
	seedMediaNodeWithUploader(t, srv, "/data/bob/c.jpg", "uploads-loc", sql.NullInt64{Int64: bob.ID, Valid: true})
	seedMediaNodeWithUploader(t, srv, "/data/system/d.jpg", "uploads-loc", sql.NullInt64{Valid: false})

	type assetsResponse struct {
		Assets []struct {
			FilePath         string `json:"filePath"`
			UploadedByUserID *int64 `json:"uploadedByUserId,omitempty"`
		} `json:"assets"`
		Total int64 `json:"total"`
	}
	decode := func(body []byte) assetsResponse {
		var r assetsResponse
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, string(body))
		}
		return r
	}

	// Filter by alice: should return exactly 2 alice rows.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodGet,
		"/api/v1/assets?uploadedByUserId="+strconv.FormatInt(alice.ID, 10), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("alice filter: status = %d, body=%s", rr.Code, rr.Body.String())
	}
	got := decode(rr.Body.Bytes())
	if got.Total != 2 {
		t.Errorf("alice total = %d, want 2", got.Total)
	}
	for _, a := range got.Assets {
		if a.UploadedByUserID == nil || *a.UploadedByUserID != alice.ID {
			t.Errorf("alice filter leaked non-alice row: %s (uploadedBy=%v)", a.FilePath, a.UploadedByUserID)
		}
	}

	// Filter by bob: should return 1 row.
	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, adminReq(http.MethodGet,
		"/api/v1/assets?uploadedByUserId="+strconv.FormatInt(bob.ID, 10), nil))
	if rr2.Code != http.StatusOK {
		t.Fatalf("bob filter: status = %d, body=%s", rr2.Code, rr2.Body.String())
	}
	got2 := decode(rr2.Body.Bytes())
	if got2.Total != 1 {
		t.Errorf("bob total = %d, want 1", got2.Total)
	}
	if got2.Assets[0].FilePath != "/data/bob/c.jpg" {
		t.Errorf("bob row = %s, want /data/bob/c.jpg", got2.Assets[0].FilePath)
	}

	// No filter: should return all 4 rows.
	rr3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr3, adminReq(http.MethodGet, "/api/v1/assets", nil))
	if rr3.Code != http.StatusOK {
		t.Fatalf("no filter: status = %d, body=%s", rr3.Code, rr3.Body.String())
	}
	got3 := decode(rr3.Body.Bytes())
	if got3.Total != 4 {
		t.Errorf("total = %d, want 4", got3.Total)
	}
}
