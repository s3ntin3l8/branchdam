package pipeline

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

func provenanceSource(t *testing.T, database *db.DB, nodeID int64) (source, hash string) {
	t.Helper()
	p, err := database.Reader.GetFullHashProvenance(context.Background(), nodeID)
	if err != nil {
		t.Fatalf("GetFullHashProvenance(%d): %v", nodeID, err)
	}
	return p.Source, p.FullHash
}

// TestCommitRecordsServerHashProvenance backs #505 on the scanner side:
// every full_hash a scan computes is recorded as server-computed -- on a
// new node, on a moved (rebased) node, and on a touched node that had no
// hash yet. A touched node whose stored hash a device asserted, and which
// this pass recomputed to the same value, becomes server-computed too.
func TestCommitRecordsServerHashProvenance(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	loc := seedLocation(t, database, "TIER3_MASTER_ARCHIVE", false)
	now := time.Now()
	h := func(c string) string { return strings.Repeat(c, 64) }

	// New node.
	if _, err := Commit(ctx, database, loc, []Result{
		{Path: "/a/new.mov", FileName: "new.mov", FileExt: "mov", Size: 1, ModTime: now, FastHash: "aaaaaaaaaaaaaaaa", FullHash: h("a")},
	}, 0); err != nil {
		t.Fatal(err)
	}
	newNode := mustGetLiveNode(t, database, "/a/new.mov")
	if src, fh := provenanceSource(t, database, newNode.ID); src != db.HashSourceServer || fh != h("a") {
		t.Errorf("new node provenance = %s/%s, want SERVER/%s", src, fh, h("a"))
	}

	// Moved node: indexed without a hash, gone MISSING, found elsewhere
	// with one.
	if _, err := Commit(ctx, database, loc, []Result{
		{Path: "/a/old.mov", FileName: "old.mov", FileExt: "mov", Size: 1, ModTime: now, FastHash: "bbbbbbbbbbbbbbbb"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	moved := mustGetLiveNode(t, database, "/a/old.mov")
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error { return q.MarkNodeMissing(ctx, moved.ID) }); err != nil {
		t.Fatal(err)
	}
	if _, err := Commit(ctx, database, loc, []Result{
		{Path: "/a/moved.mov", FileName: "moved.mov", FileExt: "mov", Size: 1, ModTime: now, FastHash: "bbbbbbbbbbbbbbbb", FullHash: h("b")},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if src, fh := provenanceSource(t, database, moved.ID); src != db.HashSourceServer || fh != h("b") {
		t.Errorf("moved node provenance = %s/%s, want SERVER/%s", src, fh, h("b"))
	}

	// Touched node whose hash a device asserted, recomputed to the same
	// value by this pass.
	if _, err := Commit(ctx, database, loc, []Result{
		{Path: "/a/agent.mov", FileName: "agent.mov", FileExt: "mov", Size: 1, ModTime: now, FastHash: "cccccccccccccccc"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	agentNode := mustGetLiveNode(t, database, "/a/agent.mov")
	asserted := h("c")
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.UpdateMediaNodeFullHash(ctx, sqlcgen.UpdateMediaNodeFullHashParams{ID: agentNode.ID, FullHash: &asserted}); err != nil {
			return err
		}
		return db.RecordFullHashSource(ctx, q, agentNode.ID, &asserted, db.HashSourceAgent)
	}); err != nil {
		t.Fatal(err)
	}
	// A pass that did not compute a full hash changes nothing.
	if _, err := Commit(ctx, database, loc, []Result{
		{Path: "/a/agent.mov", FileName: "agent.mov", FileExt: "mov", Size: 1, ModTime: now, FastHash: "cccccccccccccccc"},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if src, _ := provenanceSource(t, database, agentNode.ID); src != db.HashSourceAgent {
		t.Fatalf("touch without a computed hash changed provenance to %s", src)
	}
	if _, err := Commit(ctx, database, loc, []Result{
		{Path: "/a/agent.mov", FileName: "agent.mov", FileExt: "mov", Size: 1, ModTime: now, FastHash: "cccccccccccccccc", FullHash: asserted},
	}, 0); err != nil {
		t.Fatal(err)
	}
	if src, _ := provenanceSource(t, database, agentNode.ID); src != db.HashSourceServer {
		t.Errorf("touched node recomputed to the asserted value: provenance = %s, want SERVER", src)
	}
}
