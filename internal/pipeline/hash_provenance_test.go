package pipeline

import (
	"context"
	"database/sql"
	"errors"
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

// TestContradictingFullHashStopsAuthorisingPrune is the regression guard for
// the stale-hash data-loss path.
//
// fast_hash samples only three 2 MiB windows (internal/hashing), so a
// same-size in-place rewrite of a Tier-3 master can leave the fast hash
// bit-identical while the bytes are entirely different. The scanner then
// reaches the "touched" branch with a freshly computed BLAKE3 that DISAGREES
// with the stored one.
//
// If that disagreement is ignored, the node keeps its old full_hash and its
// SERVER provenance even though the bytes on disk are different. The prune
// gate treats a server-verified hash as the authority for "this Tier-3
// master still holds what it held", so it would go on to authorise deleting
// the Tier-1 cache that still holds the ORIGINAL bytes.
//
// The disagreement must therefore be treated as a version collision: the old
// row is archived -- so it stops vouching, and stops being a prunable
// ancestor -- and a successor is inserted carrying the hash that matches the
// bytes actually on disk.
func TestContradictingFullHashStopsAuthorisingPrune(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	archiveID := seedLocation(t, database, "TIER3_MASTER_ARCHIVE", false)
	cacheID := seedLocation(t, database, "TIER1_LOCAL_SCRATCH", false)
	now := time.Now()
	h := func(c string) string { return strings.Repeat(c, 64) }

	// The Tier-3 master, hashed by the server.
	if _, err := Commit(ctx, database, archiveID, []Result{
		{Path: "/archive/master.mov", FileName: "master.mov", FileExt: "mov", Size: 4096, ModTime: now, FastHash: "aaaaaaaaaaaaaaaa", FullHash: h("a")},
	}, 0); err != nil {
		t.Fatal(err)
	}
	master := mustGetLiveNode(t, database, "/archive/master.mov")
	if src, fh := provenanceSource(t, database, master.ID); src != db.HashSourceServer || fh != h("a") {
		t.Fatalf("master provenance = %s/%s, want SERVER/%s", src, fh, h("a"))
	}

	// The Tier-1 cache that holds the ORIGINAL bytes, linked to the master.
	if _, err := Commit(ctx, database, cacheID, []Result{
		{Path: "/cache/master_proxy.mov", FileName: "master_proxy.mov", FileExt: "mov", Size: 4096, ModTime: now, FastHash: "dddddddddddddddd", FullHash: h("d")},
	}, 0); err != nil {
		t.Fatal(err)
	}
	cache := mustGetLiveNode(t, database, "/cache/master_proxy.mov")
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID:     master.ID,
			TargetNodeID:     cache.ID,
			RelationshipType: "DERIVED_FROM",
			Confidence:       0.9,
			Tier:             1,
			Resolver:         "test-fixture",
			EvidenceJson:     "{}",
			ReviewState:      "AUTO_ACCEPTED",
		})
		return err
	}); err != nil {
		t.Fatal(err)
	}

	// Baseline: the master IS a verified Tier-3 ancestor of the cache, so the
	// prune gate would use it. Without this the final assertion below would
	// pass for the wrong reason.
	ancestors, err := database.Reader.ListVerifiedTier3Ancestors(ctx, cache.ID)
	if err != nil {
		t.Fatalf("ListVerifiedTier3Ancestors: %v", err)
	}
	if len(ancestors) != 1 || ancestors[0].ID != master.ID {
		t.Fatalf("ancestors = %+v, want exactly the master (%d)", ancestors, master.ID)
	}

	// The rewrite: same size, same fast hash (it landed outside the sampled
	// windows), different bytes -- so a different full hash.
	later := now.Add(time.Minute)
	stats, err := Commit(ctx, database, archiveID, []Result{
		{Path: "/archive/master.mov", FileName: "master.mov", FileExt: "mov", Size: 4096, ModTime: later, FastHash: "aaaaaaaaaaaaaaaa", FullHash: h("b")},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats.VersionCollisions != 1 {
		t.Fatalf("stats = %+v, want VersionCollisions=1", stats)
	}
	if stats.Touched != 0 {
		t.Errorf("stats.Touched = %d, want 0 -- the bytes changed, this is not a touch", stats.Touched)
	}

	// The live node at the path is a NEW row carrying the hash that matches
	// the bytes actually on disk.
	live := mustGetLiveNode(t, database, "/archive/master.mov")
	if live.ID == master.ID {
		t.Fatal("the live node at the path is still the old node -- the stale hash is still vouching")
	}
	if live.FullHash == nil || *live.FullHash != h("b") {
		t.Errorf("successor full_hash = %v, want %s", live.FullHash, h("b"))
	}
	if src, fh := provenanceSource(t, database, live.ID); src != db.HashSourceServer || fh != h("b") {
		t.Errorf("successor provenance = %s/%s, want SERVER/%s", src, fh, h("b"))
	}

	// The old row is archived, not deleted.
	archived, err := database.Reader.GetMediaNodeByID(ctx, master.ID)
	if err != nil {
		t.Fatalf("old node not found after archiving: %v", err)
	}
	if archived.LifecycleState != "ARCHIVED" {
		t.Errorf("old node lifecycle_state = %q, want ARCHIVED", archived.LifecycleState)
	}
	if !archived.SupersededBy.Valid || archived.SupersededBy.Int64 != live.ID {
		t.Errorf("old node superseded_by = %v, want %d", archived.SupersededBy, live.ID)
	}

	// The load-bearing assertion: the old node is no longer a verified
	// Tier-3 ancestor, so the prune gate can no longer authorise deleting
	// the Tier-1 cache that still holds the ORIGINAL bytes.
	ancestors, err = database.Reader.ListVerifiedTier3Ancestors(ctx, cache.ID)
	if err != nil {
		t.Fatalf("ListVerifiedTier3Ancestors: %v", err)
	}
	if len(ancestors) != 0 {
		t.Errorf("ancestors = %+v, want none -- the archived master must not authorise a purge", ancestors)
	}
}

// TestContradictingAgentAssertedHashKeepsTheNodeIdentity is the counterpart
// of TestContradictingFullHashStopsAuthorisingPrune: the stored hash here is
// one the SERVER never vouched for.
//
// A paired device asserted it, so it carries no authority. internal/prune
// honours a Tier-3 master's hash only while a SERVER provenance row matches
// it, so an AGENT-only hash authorises no purge and there is no stale vouch
// to withdraw. A disagreement with the hash this pass computed is therefore
// not evidence of a content change -- it may only be a wrong assertion.
//
// Treating it as one would archive the row on the strength of a value the
// server never verified: the node's creator attribution and every edge on it
// would be stranded on the archived row, and the internal/rehash MISMATCH
// repair that exists for exactly this condition would be pre-empted. The node
// must keep its identity instead, and adopt the hash the server computed from
// the bytes it just read.
func TestContradictingAgentAssertedHashKeepsTheNodeIdentity(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	archiveID := seedLocation(t, database, "TIER3_MASTER_ARCHIVE", false)
	now := time.Now()
	h := func(c string) string { return strings.Repeat(c, 64) }

	// The master, hashed by the server.
	if _, err := Commit(ctx, database, archiveID, []Result{
		{Path: "/archive/master.mov", FileName: "master.mov", FileExt: "mov", Size: 4096, ModTime: now, FastHash: "aaaaaaaaaaaaaaaa", FullHash: h("a")},
	}, 0); err != nil {
		t.Fatal(err)
	}
	master := mustGetLiveNode(t, database, "/archive/master.mov")

	// A paired device asserts a hash of its own for the node.
	asserted := h("z")
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.UpdateMediaNodeFullHash(ctx, sqlcgen.UpdateMediaNodeFullHashParams{ID: master.ID, FullHash: &asserted}); err != nil {
			return err
		}
		return db.RecordFullHashSource(ctx, q, master.ID, &asserted, db.HashSourceAgent)
	}); err != nil {
		t.Fatal(err)
	}
	if src, fh := provenanceSource(t, database, master.ID); src != db.HashSourceAgent || fh != asserted {
		t.Fatalf("provenance = %s/%s, want AGENT/%s", src, fh, asserted)
	}

	// The next pass reads the bytes and computes a different hash. The fast
	// hash is unchanged, so this is the "touched" branch.
	later := now.Add(time.Minute)
	stats, err := Commit(ctx, database, archiveID, []Result{
		{Path: "/archive/master.mov", FileName: "master.mov", FileExt: "mov", Size: 4096, ModTime: later, FastHash: "aaaaaaaaaaaaaaaa", FullHash: h("b")},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats.VersionCollisions != 0 {
		t.Fatalf("stats = %+v, want VersionCollisions=0 -- an unverified assertion is not a content change", stats)
	}
	if stats.Touched != 1 {
		t.Errorf("stats.Touched = %d, want 1", stats.Touched)
	}

	// The node keeps its identity: the same row, the same node_uuid.
	live := mustGetLiveNode(t, database, "/archive/master.mov")
	if live.ID != master.ID {
		t.Fatalf("live node id = %d, want %d -- the node was versioned on an unverified assertion", live.ID, master.ID)
	}
	if live.NodeUuid != master.NodeUuid {
		t.Errorf("live node uuid = %q, want %q", live.NodeUuid, master.NodeUuid)
	}

	// The bad assertion is cleared: the server's own hash is stored, and the
	// provenance records who computed it.
	if live.FullHash == nil || *live.FullHash != h("b") {
		t.Errorf("full_hash = %v, want %s", live.FullHash, h("b"))
	}
	if src, fh := provenanceSource(t, database, master.ID); src != db.HashSourceServer || fh != h("b") {
		t.Errorf("provenance = %s/%s, want SERVER/%s", src, fh, h("b"))
	}
}

// TestContradictingFullHashWithDuplicateContentArchivesSuperseded covers the
// sibling case: the rewritten content is already indexed at another live
// path, so no successor can be inserted -- the unique index
// ux_media_nodes_live_full_hash forbids two live nodes carrying one hash.
//
// The superseded row must still be archived. Leaving it live would leave it
// vouching, with its SERVER provenance, for bytes that are no longer at that
// path, which is the same hole the version collision closes.
func TestContradictingFullHashWithDuplicateContentArchivesSuperseded(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	archiveID := seedLocation(t, database, "TIER3_MASTER_ARCHIVE", false)
	now := time.Now()
	h := func(c string) string { return strings.Repeat(c, 64) }

	// The master, plus the other live node that already holds the content
	// the master is about to be rewritten with.
	if _, err := Commit(ctx, database, archiveID, []Result{
		{Path: "/archive/master.mov", FileName: "master.mov", FileExt: "mov", Size: 4096, ModTime: now, FastHash: "aaaaaaaaaaaaaaaa", FullHash: h("a")},
		{Path: "/archive/other.mov", FileName: "other.mov", FileExt: "mov", Size: 4096, ModTime: now, FastHash: "bbbbbbbbbbbbbbbb", FullHash: h("b")},
	}, 0); err != nil {
		t.Fatal(err)
	}
	master := mustGetLiveNode(t, database, "/archive/master.mov")

	// The rewrite: same fast hash (it landed outside the sampled windows),
	// content identical to the other node.
	later := now.Add(time.Minute)
	stats, err := Commit(ctx, database, archiveID, []Result{
		{Path: "/archive/master.mov", FileName: "master.mov", FileExt: "mov", Size: 4096, ModTime: later, FastHash: "aaaaaaaaaaaaaaaa", FullHash: h("b")},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats.VersionCollisions != 1 {
		t.Fatalf("stats = %+v, want VersionCollisions=1", stats)
	}
	if stats.Duplicates != 1 {
		t.Errorf("stats.Duplicates = %d, want 1", stats.Duplicates)
	}

	// The superseded row is archived: it must stop vouching for bytes that
	// are no longer at this path.
	archived, err := database.Reader.GetMediaNodeByID(ctx, master.ID)
	if err != nil {
		t.Fatalf("old node not found: %v", err)
	}
	if archived.LifecycleState != "ARCHIVED" {
		t.Errorf("old node lifecycle_state = %q, want ARCHIVED", archived.LifecycleState)
	}

	// And no second live node may carry that hash.
	if _, err := database.Reader.GetLiveNodeByPath(ctx, "/archive/master.mov"); err == nil {
		t.Error("a live node was inserted at the path even though another live node already holds that content")
	}
}

// TestDisprovedAgentHashIsClearedWhenTheContentIsAlreadyIndexed covers the
// one case the AGENT-hash correction does not reach: the freshly computed
// hash already belongs to ANOTHER live node, so the unique index
// ux_media_nodes_live_full_hash forbids storing it here.
//
// The stored value is still disproved -- the server read the bytes itself --
// so it must not be kept. Leaving it would (a) make GetMediaNodeByFullHash
// match this node for the stale hash, so a later file whose TRUE hash is
// that one is silently declined and never indexed, with nothing in the log
// beyond a duplicate counter; and (b) never self-heal, because every later
// pass computes the same contradicting hash and takes the same skip.
//
// The node therefore loses the hash (and the provenance row that asserted
// it) but keeps its identity, its node_uuid and its attribution.
func TestDisprovedAgentHashIsClearedWhenTheContentIsAlreadyIndexed(t *testing.T) {
	database := openTestDB(t)
	ctx := context.Background()
	archiveID := seedLocation(t, database, "TIER3_MASTER_ARCHIVE", false)
	now := time.Now()
	h := func(c string) string { return strings.Repeat(c, 64) }

	// The master (hash A), and the other live node that already holds the
	// content the master is about to be rewritten with (hash B).
	if _, err := Commit(ctx, database, archiveID, []Result{
		{Path: "/archive/master.mov", FileName: "master.mov", FileExt: "mov", Size: 4096, ModTime: now, FastHash: "aaaaaaaaaaaaaaaa", FullHash: h("a")},
		{Path: "/archive/copy.mov", FileName: "copy.mov", FileExt: "mov", Size: 4096, ModTime: now, FastHash: "bbbbbbbbbbbbbbbb", FullHash: h("b")},
	}, 0); err != nil {
		t.Fatal(err)
	}
	master := mustGetLiveNode(t, database, "/archive/master.mov")

	// A paired device asserts a hash of its own for the master.
	asserted := h("z")
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.UpdateMediaNodeFullHash(ctx, sqlcgen.UpdateMediaNodeFullHashParams{ID: master.ID, FullHash: &asserted}); err != nil {
			return err
		}
		return db.RecordFullHashSource(ctx, q, master.ID, &asserted, db.HashSourceAgent)
	}); err != nil {
		t.Fatal(err)
	}
	if src, fh := provenanceSource(t, database, master.ID); src != db.HashSourceAgent || fh != asserted {
		t.Fatalf("provenance = %s/%s, want AGENT/%s", src, fh, asserted)
	}

	// The next pass reads the bytes and computes hash B -- the content the
	// OTHER live node already holds. The fast hash is unchanged, so this is
	// the "touched" branch.
	later := now.Add(time.Minute)
	stats, err := Commit(ctx, database, archiveID, []Result{
		{Path: "/archive/master.mov", FileName: "master.mov", FileExt: "mov", Size: 4096, ModTime: later, FastHash: "aaaaaaaaaaaaaaaa", FullHash: h("b")},
	}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Touched != 1 {
		t.Errorf("stats.Touched = %d, want 1", stats.Touched)
	}
	if stats.Duplicates != 1 {
		t.Errorf("stats.Duplicates = %d, want 1", stats.Duplicates)
	}
	if stats.VersionCollisions != 0 {
		t.Fatalf("stats = %+v, want VersionCollisions=0 -- an unverified assertion is not a content change", stats)
	}

	// The node keeps its identity: the same row, the same node_uuid.
	live := mustGetLiveNode(t, database, "/archive/master.mov")
	if live.ID != master.ID {
		t.Fatalf("live node id = %d, want %d -- the node was versioned on an unverified assertion", live.ID, master.ID)
	}
	if live.NodeUuid != master.NodeUuid {
		t.Errorf("live node uuid = %q, want %q", live.NodeUuid, master.NodeUuid)
	}

	// The disproved hash is gone: not stored, and no longer asserted by a
	// provenance row.
	if live.FullHash != nil {
		t.Errorf("full_hash = %q, want NULL -- the server disproved it and cannot store the correct one", *live.FullHash)
	}
	if live.IndexingStatus != "INDEXED_SHALLOW" {
		t.Errorf("indexing_status = %q, want INDEXED_SHALLOW -- the node holds no full hash any more", live.IndexingStatus)
	}
	if _, err := database.Reader.GetFullHashProvenance(ctx, master.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("provenance row = %v, want none -- the disproved hash must not be asserted any more", err)
	}

	// And the stale hash no longer shadows its own content: a file whose
	// TRUE hash is the disproved one is indexed normally instead of being
	// declined as a duplicate of this node.
	if _, err := Commit(ctx, database, archiveID, []Result{
		{Path: "/archive/z.mov", FileName: "z.mov", FileExt: "mov", Size: 4096, ModTime: later, FastHash: "cccccccccccccccc", FullHash: asserted},
	}, 0); err != nil {
		t.Fatal(err)
	}
	shadowed := mustGetLiveNode(t, database, "/archive/z.mov")
	if shadowed.FullHash == nil || *shadowed.FullHash != asserted {
		t.Errorf("full_hash = %v, want %s -- the stale hash still shadows its own content", shadowed.FullHash, asserted)
	}
}
