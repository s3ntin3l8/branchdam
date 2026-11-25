package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// A broad match must not scan the FTS hit set again for every media node.
// The former correlated predicate/rank took tens of seconds at this size.
func TestBroadFTSSearchIsBounded(t *testing.T) {
	database := openTestDB(t)
	if _, err := database.writer.Exec(`INSERT INTO storage_locations(id,name,root_path,tier)
 VALUES(1,'archive','/archive','TIER1_LOCAL_SCRATCH');
 WITH RECURSIVE seed(id) AS (SELECT 1 UNION ALL SELECT id+1 FROM seed WHERE id<8000)
 INSERT INTO media_nodes(id,node_uuid,storage_location_id,file_path,file_name)
 SELECT id, 'uuid-'||id, 1, '/archive/clip_'||id||'.jpg', 'clip_'||id||'.jpg' FROM seed;`); err != nil {
		t.Fatal(err)
	}
	name := sql.NullString{String: "clip", Valid: true}
	fts := sql.NullString{String: `"clip"*`, Valid: true}
	active := sql.NullString{String: "ACTIVE", Valid: true}
	checks := []struct {
		name string
		run  func(context.Context) (int64, error)
	}{
		{"count", func(ctx context.Context) (int64, error) {
			return database.Reader.CountMediaNodesSearch(ctx, sqlcgen.CountMediaNodesSearchParams{NameQuery: name, FtsQuery: fts})
		}},
		{"filtered count", func(ctx context.Context) (int64, error) {
			return database.Reader.CountMediaNodesFiltered(ctx, sqlcgen.CountMediaNodesFilteredParams{NameQuery: name, FtsQuery: fts, LifecycleState: active})
		}},
		{"list", func(ctx context.Context) (int64, error) {
			rows, err := database.Reader.ListMediaNodes(ctx, sqlcgen.ListMediaNodesParams{NameQuery: name, FtsQuery: fts, Limit: 50})
			if err == nil && (len(rows) != 50 || rows[0].ID != 8000 || rows[49].ID != 7951) {
				t.Errorf("unstable broad-match page: %v", rows)
			}
			return int64(len(rows)), err
		}},
		{"filtered list", func(ctx context.Context) (int64, error) {
			rows, err := database.Reader.ListMediaNodesFiltered(ctx, sqlcgen.ListMediaNodesFilteredParams{NameQuery: name, FtsQuery: fts, LifecycleState: active, Limit: 50})
			if err == nil && (len(rows) != 50 || rows[0].ID != 8000 || rows[49].ID != 7951) {
				t.Errorf("unstable filtered broad-match page: %v", rows)
			}
			return int64(len(rows)), err
		}},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			// Generous headroom for race/coverage and loaded CI; the regression is
			// quadratic, whereas the joined hit set completes in milliseconds.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			start := time.Now()
			count, err := check.run(ctx)
			t.Logf("broad FTS search: %s", time.Since(start))
			if err != nil {
				t.Fatal(err)
			}
			want := int64(8000)
			if check.name == "list" || check.name == "filtered list" {
				want = 50
			}
			if count != want {
				t.Fatalf("got %d, want %d", count, want)
			}
		})
	}
}
