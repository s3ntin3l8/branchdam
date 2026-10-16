package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
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

// newSearchTestServer wires a server with attribution + an admin group so
// both /api/v1/assets and the admin-only /api/v1/users answer adminReq.
func newSearchTestServer(t *testing.T) (*Server, *attributionusers.Service) {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "search.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	usersSvc := attributionusers.NewService(database)
	if _, err := usersSvc.EnsureSystemUser(context.Background()); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	store, err := settings.NewStore(context.Background(), database,
		config.Config{Authz: config.Authz{Groups: []string{"dam-admins"}}}, settingsTestKey(t), nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}
	srv := New(Deps{
		Settings: store, DB: database, Hub: sse.New(), Version: "test",
		Attribution: usersSvc,
		Audit:       audit.NewService(database, usersSvc),

		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})
	return srv, usersSvc
}

type searchNode struct {
	name      string
	camera    string
	lifecycle string
	captured  sql.NullInt64
	createdAt int64
}

func seedSearchNodes(t *testing.T, srv *Server, nodes []searchNode) {
	t.Helper()
	var locID int64
	if err := srv.db.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		r, e := q.UpsertStorageLocation(context.Background(), sqlcgen.UpsertStorageLocationParams{
			Name: "search-loc", RootPath: "/tmp/search-loc", Tier: "TIER1_LOCAL_SCRATCH",
		})
		locID = r.ID
		return e
	}); err != nil {
		t.Fatalf("seed storage location: %v", err)
	}
	for i, n := range nodes {
		lifecycle := n.lifecycle
		if lifecycle == "" {
			lifecycle = "ACTIVE"
		}
		var camera sql.NullString
		if n.camera != "" {
			camera = sql.NullString{String: n.camera, Valid: true}
		}
		createdAt := n.createdAt
		if createdAt == 0 {
			createdAt = 1_700_000_000 + int64(i)
		}
		path := "/tmp/search-loc/" + n.name
		if _, err := srv.db.ExecInTx(context.Background(),
			`INSERT INTO media_nodes (node_uuid, storage_location_id, file_path, file_name, file_ext, size_bytes, mtime_unix,
			   indexing_status, graph_status, lifecycle_state, camera_model, captured_at_unix, created_at)
			 VALUES (?, ?, ?, ?, ?, 1024, unixepoch(), 'INDEXED_FULL', 'UNLINKED', ?, ?, ?, ?)`,
			fmt.Sprintf("uuid-search-%d", i), locID, path, n.name, filepath.Ext(n.name), lifecycle, camera, n.captured, createdAt); err != nil {
			t.Fatalf("seed media node %s: %v", n.name, err)
		}
	}
}

type assetListBody struct {
	Assets []struct {
		FileName string `json:"fileName"`
	} `json:"assets"`
	Total int64 `json:"total"`
}

func getAssets(t *testing.T, srv *Server, query url.Values) (int, assetListBody) {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodGet, "/api/v1/assets?"+query.Encode(), nil))
	var body assetListBody
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v body=%s", err, rr.Body.String())
		}
	}
	return rr.Code, body
}

func assetNames(b assetListBody) []string {
	out := make([]string, len(b.Assets))
	for i, a := range b.Assets {
		out[i] = a.FileName
	}
	return out
}

// TestHandleListAssets_QSearchesFileNameServerSide: ?q= narrows the list
// (and its total) to a case-insensitive file_name substring across the
// whole table, keeps % and _ literal, and on the unfiltered path still
// hides TRASHED rows.
func TestHandleListAssets_QSearchesFileNameServerSide(t *testing.T) {
	srv, _ := newSearchTestServer(t)
	seedSearchNodes(t, srv, []searchNode{
		{name: "DSC_0001.ARW", camera: "Sony"},
		{name: "dsc_0002.jpg", camera: "Canon"},
		{name: "IMG_1000.jpg", camera: "Sony"},
		{name: "DSC_TRASHED.jpg", lifecycle: "TRASHED", camera: "Sony"},
		{name: "100%_crop.png"},
		{name: "1000_crop.png"},
	})

	code, body := getAssets(t, srv, url.Values{"q": {"dsc"}})
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	if body.Total != 2 || len(body.Assets) != 2 {
		t.Fatalf("q=dsc: total=%d names=%v, want the two live DSC rows", body.Total, assetNames(body))
	}
	for _, n := range assetNames(body) {
		if !strings.Contains(strings.ToLower(n), "dsc") || n == "DSC_TRASHED.jpg" {
			t.Errorf("q=dsc returned %q", n)
		}
	}

	// q composes with a structural filter (the filtered path).
	_, body = getAssets(t, srv, url.Values{"q": {"DSC"}, "cameraModel": {"Sony"}})
	if got := assetNames(body); body.Total != 2 || len(got) != 2 {
		t.Fatalf("q=DSC&cameraModel=Sony: total=%d names=%v, want DSC_0001.ARW + DSC_TRASHED.jpg", body.Total, got)
	}

	// % is literal, not a LIKE wildcard.
	_, body = getAssets(t, srv, url.Values{"q": {"100%"}})
	if got := assetNames(body); body.Total != 1 || len(got) != 1 || got[0] != "100%_crop.png" {
		t.Fatalf("q=100%%: total=%d names=%v, want only 100%%_crop.png", body.Total, got)
	}

	// Blank q is no filter.
	_, body = getAssets(t, srv, url.Values{"q": {"   "}})
	if body.Total != 5 {
		t.Fatalf("blank q: total=%d, want 5 live rows", body.Total)
	}
}

// TestHandleListAssets_SortWhitelist: ?sort= orders the whole result set
// server-side, captured_at NULLs sort last both ways, and a value outside
// the whitelist is rejected.
func TestHandleListAssets_SortWhitelist(t *testing.T) {
	srv, _ := newSearchTestServer(t)
	seedSearchNodes(t, srv, []searchNode{
		{name: "b.jpg", captured: sql.NullInt64{Int64: 300, Valid: true}, createdAt: 30},
		{name: "C.jpg", createdAt: 10},
		{name: "a.jpg", captured: sql.NullInt64{Int64: 100, Valid: true}, createdAt: 20},
	})

	cases := []struct {
		sort string
		want []string
	}{
		{"name_asc", []string{"a.jpg", "b.jpg", "C.jpg"}},
		{"name_desc", []string{"C.jpg", "b.jpg", "a.jpg"}},
		{"created_asc", []string{"C.jpg", "a.jpg", "b.jpg"}},
		{"created_desc", []string{"b.jpg", "a.jpg", "C.jpg"}},
		{"captured_asc", []string{"a.jpg", "b.jpg", "C.jpg"}},
		{"captured_desc", []string{"b.jpg", "a.jpg", "C.jpg"}},
		{"newest", []string{"a.jpg", "C.jpg", "b.jpg"}},
	}
	for _, tc := range cases {
		t.Run(tc.sort, func(t *testing.T) {
			// limit=2 + offset=1 proves the ORDER BY runs before paging.
			_, full := getAssets(t, srv, url.Values{"sort": {tc.sort}})
			if got := assetNames(full); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("sort=%s: got %v, want %v", tc.sort, got, tc.want)
			}
			_, page := getAssets(t, srv, url.Values{"sort": {tc.sort}, "limit": {"1"}, "offset": {"1"}})
			if got := assetNames(page); len(got) != 1 || got[0] != tc.want[1] {
				t.Fatalf("sort=%s page 2: got %v, want [%s]", tc.sort, got, tc.want[1])
			}
			// Same order on the filtered path.
			_, filtered := getAssets(t, srv, url.Values{"sort": {tc.sort}, "lifecycleState": {"ACTIVE"}})
			if got := assetNames(filtered); strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("sort=%s filtered: got %v, want %v", tc.sort, got, tc.want)
			}
		})
	}

	if code, _ := getAssets(t, srv, url.Values{"sort": {"file_path; DROP TABLE media_nodes"}}); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown sort: status = %d, want 422", code)
	}
}

type userListBody struct {
	Users []struct {
		ID       int64  `json:"id"`
		Username string `json:"username"`
	} `json:"users"`
	Total int64 `json:"total"`
}

func getUsers(t *testing.T, srv *Server, query url.Values) userListBody {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodGet, "/api/v1/users?"+query.Encode(), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /users?%s: status = %d body=%s", query.Encode(), rr.Code, rr.Body.String())
	}
	var body userListBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return body
}

// TestHandleListUsers_QAndIDs: ?q= searches username/email over the whole
// table (not the loaded page) with a matching total, and ?ids= resolves
// specific users regardless of which page they would fall on.
func TestHandleListUsers_QAndIDs(t *testing.T) {
	srv, usersSvc := newSearchTestServer(t)
	var lastID int64
	for i := 0; i < 30; i++ {
		u, err := usersSvc.ResolveOrCreate(context.Background(), auth.Principal{
			Kind: auth.KindUser, Name: fmt.Sprintf("user%02d", i), ExternalUID: fmt.Sprintf("uid-%d", i),
			Email: fmt.Sprintf("person%02d@example.com", i), Authenticated: true,
		})
		if err != nil {
			t.Fatalf("ResolveOrCreate: %v", err)
		}
		lastID = u.ID
	}

	// user29 sits beyond a limit=5 first page, but q finds it.
	body := getUsers(t, srv, url.Values{"q": {"USER29"}, "limit": {"5"}})
	if body.Total != 1 || len(body.Users) != 1 || body.Users[0].Username != "user29" {
		t.Fatalf("q=USER29: %+v", body)
	}
	// Email matches too; total is the filtered count, not the table size.
	body = getUsers(t, srv, url.Values{"q": {"person1"}, "limit": {"5"}})
	if body.Total != 10 || len(body.Users) != 5 {
		t.Fatalf("q=person1: total=%d len=%d, want total 10, 5 on the page", body.Total, len(body.Users))
	}

	// ids= returns exactly the requested rows.
	body = getUsers(t, srv, url.Values{"ids": {strconv.FormatInt(lastID, 10) + ",999999"}})
	if body.Total != 1 || len(body.Users) != 1 || body.Users[0].ID != lastID {
		t.Fatalf("ids=%d: %+v", lastID, body)
	}

	// Unfiltered still pages the whole table (30 users + system).
	body = getUsers(t, srv, url.Values{"limit": {"5"}})
	if body.Total != 31 || len(body.Users) != 5 {
		t.Fatalf("unfiltered: total=%d len=%d", body.Total, len(body.Users))
	}
}
