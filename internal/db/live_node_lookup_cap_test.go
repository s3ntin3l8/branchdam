package db

import (
	"context"
	"database/sql"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/pressly/goose/v3"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// TestLiveNodeLookupsAreBounded pins that the two live-node lookups
// honour their row cap.
//
// ListLiveNodesByFileName and ListLiveNodesByDocumentID both used to
// return every matching live row with no cap, and neither had an index
// (see migration_00044_test.go for the index half). The cap has to keep
// the consumer's semantics intact: the project-sidecar resolver's
// filename fallback only uses a match when it is EXACTLY one node, so
// truncating a group of three to two must still leave "more than one"
// and be skipped as ambiguous -- it must never turn an ambiguous match
// into a wrong single one.
func TestLiveNodeLookupsAreBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "live-node-lookup-cap.db")
	writerDB := openRawWriter(t, path)
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)
	if err := goose.SetDialect("sqlite3"); err != nil {
		t.Fatal(err)
	}
	if err := goose.Up(writerDB, migrationsDir); err != nil {
		t.Fatalf("migrate up: %v", err)
	}

	if _, err := writerDB.Exec(
		`INSERT INTO storage_locations (id, name, root_path, tier) VALUES (1, 'media', '/media', 'TIER1_LOCAL_SCRATCH')`,
	); err != nil {
		t.Fatalf("seed storage location: %v", err)
	}
	for i := 1; i <= 3; i++ {
		stmt := `INSERT INTO media_nodes (id, node_uuid, storage_location_id, file_path, file_name, document_id, lifecycle_state, indexing_status, full_hash, updated_at) VALUES (` +
			strconv.Itoa(i) +
			`, '018f0000-0000-7000-8000-00000000000` + strconv.Itoa(i) +
			`', 1, '/media/` + strconv.Itoa(i) + `.jpg', 'shared.jpg', 'doc-shared', 'ACTIVE', 'INDEXED_FULL', '` +
			strings.Repeat(string(rune('a'+i)), 64) + `', 1000)`
		if _, err := writerDB.Exec(stmt); err != nil {
			t.Fatalf("seed media node %d: %v", i, err)
		}
	}

	q := sqlcgen.New(writerDB)
	ctx := context.Background()

	rows, err := q.ListLiveNodesByFileName(ctx, sqlcgen.ListLiveNodesByFileNameParams{
		FileName: "shared.jpg",
		Limit:    500,
	})
	if err != nil {
		t.Fatalf("ListLiveNodesByFileName: %v", err)
	}
	if len(rows) != 3 {
		t.Errorf("ListLiveNodesByFileName returned %d rows, want 3 (the cap must not truncate a small group)", len(rows))
	}

	rows, err = q.ListLiveNodesByFileName(ctx, sqlcgen.ListLiveNodesByFileNameParams{
		FileName: "shared.jpg",
		Limit:    2,
	})
	if err != nil {
		t.Fatalf("ListLiveNodesByFileName (capped): %v", err)
	}
	if len(rows) != 2 {
		t.Errorf("ListLiveNodesByFileName with a cap of 2 returned %d rows, want 2", len(rows))
	}
	// A truncated group is still "more than one", so the consumer still
	// treats it as an ambiguous match and skips it.
	if len(rows) == 1 {
		t.Error("a truncated group collapsed to a single match, which would turn an ambiguous reference into a wrong one")
	}

	docID := sql.NullString{String: "doc-shared", Valid: true}
	docRows, err := q.ListLiveNodesByDocumentID(ctx, sqlcgen.ListLiveNodesByDocumentIDParams{
		DocumentID: docID,
		Limit:      500,
	})
	if err != nil {
		t.Fatalf("ListLiveNodesByDocumentID: %v", err)
	}
	if len(docRows) != 3 {
		t.Errorf("ListLiveNodesByDocumentID returned %d rows, want 3", len(docRows))
	}

	docRows, err = q.ListLiveNodesByDocumentID(ctx, sqlcgen.ListLiveNodesByDocumentIDParams{
		DocumentID: docID,
		Limit:      1,
	})
	if err != nil {
		t.Fatalf("ListLiveNodesByDocumentID (capped): %v", err)
	}
	if len(docRows) != 1 {
		t.Errorf("ListLiveNodesByDocumentID with a cap of 1 returned %d rows, want 1", len(docRows))
	}
}
