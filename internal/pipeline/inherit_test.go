package pipeline

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

func TestPickWinningParent(t *testing.T) {
	t.Run("excludes unconfirmed review states", func(t *testing.T) {
		edges := []sqlcgen.MediaEdge{
			{ID: 1, ReviewState: "NEEDS_REVIEW", Tier: 1, RelationshipType: "DERIVED_FROM", Confidence: 0.95},
			{ID: 2, ReviewState: "REJECTED", Tier: 1, RelationshipType: "DERIVED_FROM", Confidence: 0.99},
		}
		if got := PickWinningParent(edges); got != nil {
			t.Errorf("PickWinningParent = %+v, want nil", got)
		}
	})

	t.Run("excludes tier 3 heuristics unconditionally", func(t *testing.T) {
		edges := []sqlcgen.MediaEdge{
			{ID: 1, ReviewState: "AUTO_ACCEPTED", Tier: 3, RelationshipType: "DERIVED_FROM", Confidence: 0.99},
			{ID: 2, ReviewState: "CONFIRMED", Tier: 3, RelationshipType: "FINAL_EXPORT", Confidence: 0.99},
		}
		if got := PickWinningParent(edges); got != nil {
			t.Errorf("PickWinningParent = %+v, want nil (Tier-3 must be excluded)", got)
		}
	})

	t.Run("excludes non-ancestry relationship types", func(t *testing.T) {
		edges := []sqlcgen.MediaEdge{
			{ID: 1, ReviewState: "AUTO_ACCEPTED", Tier: 1, RelationshipType: "DUPLICATE_OF", Confidence: 0.99},
			{ID: 2, ReviewState: "CONFIRMED", Tier: 1, RelationshipType: "PROJECT_SIDECAR", Confidence: 0.99},
		}
		if got := PickWinningParent(edges); got != nil {
			t.Errorf("PickWinningParent = %+v, want nil (non-ancestry must be excluded)", got)
		}
	})

	t.Run("picks highest confidence tier 1 or 2", func(t *testing.T) {
		edges := []sqlcgen.MediaEdge{
			{ID: 1, ReviewState: "AUTO_ACCEPTED", Tier: 2, RelationshipType: "DERIVED_FROM", Confidence: 0.85},
			{ID: 2, ReviewState: "AUTO_ACCEPTED", Tier: 1, RelationshipType: "FINAL_EXPORT", Confidence: 0.92},
			{ID: 3, ReviewState: "AUTO_ACCEPTED", Tier: 3, RelationshipType: "DERIVED_FROM", Confidence: 0.99}, // higher confidence but Tier 3
		}
		got := PickWinningParent(edges)
		if got == nil || got.ID != 2 {
			t.Fatalf("PickWinningParent = %+v, want edge ID 2", got)
		}
	})

	t.Run("tie-breaks by lowest edge ID", func(t *testing.T) {
		edges := []sqlcgen.MediaEdge{
			{ID: 5, ReviewState: "AUTO_ACCEPTED", Tier: 2, RelationshipType: "DERIVED_FROM", Confidence: 0.90},
			{ID: 3, ReviewState: "AUTO_ACCEPTED", Tier: 2, RelationshipType: "DERIVED_FROM", Confidence: 0.90},
		}
		got := PickWinningParent(edges)
		if got == nil || got.ID != 3 {
			t.Fatalf("PickWinningParent = %+v, want lowest edge ID 3", got)
		}
	})
}

func TestHasResolvedButIneligibleParent(t *testing.T) {
	t.Run("returns true when resolved edge present", func(t *testing.T) {
		edges := []sqlcgen.MediaEdge{
			{ID: 1, ReviewState: "AUTO_ACCEPTED", Tier: 3, RelationshipType: "DERIVED_FROM"},
		}
		if !HasResolvedButIneligibleParent(edges) {
			t.Error("expected true")
		}
	})

	t.Run("returns false when no resolved edge present", func(t *testing.T) {
		edges := []sqlcgen.MediaEdge{
			{ID: 1, ReviewState: "NEEDS_REVIEW", Tier: 2, RelationshipType: "DERIVED_FROM"},
			{ID: 2, ReviewState: "REJECTED", Tier: 2, RelationshipType: "DERIVED_FROM"},
		}
		if HasResolvedButIneligibleParent(edges) {
			t.Error("expected false")
		}
	})
}

func TestInheritMetadataPreconditions(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)

	locID := seedLocation(t, database, "TIER2_EXPORTS", false)

	insertNode := func(uuid, path, state string) sqlcgen.MediaNode {
		var node sqlcgen.MediaNode
		err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
			n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
				NodeUuid:          uuid,
				StorageLocationID: locID,
				FilePath:          path,
				FileName:          filepath.Base(path),
				FileExt:           filepath.Ext(path),
				SizeBytes:         100,
				MtimeUnix:         time.Now().Unix(),
				FastHash:          &[]string{"0123456789abcdef"}[0],
				IndexingStatus:    "INDEXED_SHALLOW",
				GraphStatus:       "LINKED",
				LifecycleState:    state,
			})
			node = n
			return err
		})
		if err != nil {
			t.Fatalf("insert node: %v", err)
		}
		return node
	}

	t.Run("unknown asset returns ErrNodeNotFound", func(t *testing.T) {
		deps := InheritDeps{DB: database}
		_, err := InheritMetadata(ctx, deps, 999999)
		if !errors.Is(err, ErrNodeNotFound) {
			t.Errorf("got %v, want ErrNodeNotFound", err)
		}
	})

	t.Run("archived asset returns ErrNodeNotFound", func(t *testing.T) {
		n := insertNode("uuid-archived", "/tmp/archived.jpg", "ARCHIVED")
		deps := InheritDeps{DB: database}
		_, err := InheritMetadata(ctx, deps, n.ID)
		if !errors.Is(err, ErrNodeNotFound) {
			t.Errorf("got %v, want ErrNodeNotFound", err)
		}
	})

	t.Run("project file child returns ErrIsProjectFile", func(t *testing.T) {
		n := insertNode("uuid-proj", "/tmp/project.drp", "ACTIVE")
		deps := InheritDeps{DB: database}
		_, err := InheritMetadata(ctx, deps, n.ID)
		if !errors.Is(err, ErrIsProjectFile) {
			t.Errorf("got %v, want ErrIsProjectFile", err)
		}
	})

	t.Run("read-only location returns ErrReadOnlyTier", func(t *testing.T) {
		roLocID := seedLocation(t, database, "TIER3_MASTER_ARCHIVE", true)
		var roNode sqlcgen.MediaNode
		err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
			n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
				NodeUuid:          "uuid-ro",
				StorageLocationID: roLocID,
				FilePath:          "/ro/child.jpg",
				FileName:          "child.jpg",
				FileExt:           "jpg",
				SizeBytes:         100,
				MtimeUnix:         time.Now().Unix(),
				FastHash:          &[]string{"0123456789abcdef"}[0],
				IndexingStatus:    "INDEXED_SHALLOW",
				GraphStatus:       "LINKED",
				LifecycleState:    "ACTIVE",
			})
			roNode = n
			return err
		})
		if err != nil {
			t.Fatalf("insert ro node: %v", err)
		}
		guard := storage.NewGuard([]storage.Location{
			{ID: roLocID, Name: "ro", RootPath: "/ro", Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
		})
		deps := InheritDeps{DB: database, Guard: guard}
		_, err = InheritMetadata(ctx, deps, roNode.ID)
		var roErr *storage.ErrReadOnlyTier
		if !errors.As(err, &roErr) {
			t.Errorf("got %v, want *storage.ErrReadOnlyTier", err)
		}
	})

	t.Run("no parent edge returns ErrNoParent", func(t *testing.T) {
		n := insertNode("uuid-noparent", "/tmp/noparent.jpg", "ACTIVE")
		deps := InheritDeps{DB: database}
		_, err := InheritMetadata(ctx, deps, n.ID)
		if !errors.Is(err, ErrNoParent) {
			t.Errorf("got %v, want ErrNoParent", err)
		}
	})

	t.Run("ineligible parent returns ErrIneligibleParent", func(t *testing.T) {
		parent := insertNode("uuid-parent-t3", "/tmp/p-t3.jpg", "ACTIVE")
		child := insertNode("uuid-child-t3", "/tmp/c-t3.jpg", "ACTIVE")
		err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
			_, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
				SourceNodeID:     parent.ID,
				TargetNodeID:     child.ID,
				RelationshipType: "DERIVED_FROM",
				Confidence:       0.99,
				Tier:             3,
				Resolver:         "test",
				ReviewState:      "AUTO_ACCEPTED",
			})
			return err
		})
		if err != nil {
			t.Fatalf("create edge: %v", err)
		}
		deps := InheritDeps{DB: database}
		_, err = InheritMetadata(ctx, deps, child.ID)
		if !errors.Is(err, ErrIneligibleParent) {
			t.Errorf("got %v, want ErrIneligibleParent", err)
		}
	})

	t.Run("missing parent returns ErrParentUnavailable", func(t *testing.T) {
		parent := insertNode("uuid-parent-miss", "/tmp/p-miss.jpg", "MISSING")
		child := insertNode("uuid-child-miss", "/tmp/c-miss.jpg", "ACTIVE")
		err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
			_, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
				SourceNodeID:     parent.ID,
				TargetNodeID:     child.ID,
				RelationshipType: "DERIVED_FROM",
				Confidence:       0.90,
				Tier:             2,
				Resolver:         "test",
				ReviewState:      "AUTO_ACCEPTED",
			})
			return err
		})
		if err != nil {
			t.Fatalf("create edge: %v", err)
		}
		deps := InheritDeps{DB: database}
		_, err = InheritMetadata(ctx, deps, child.ID)
		var pErr *ErrParentUnavailable
		if !errors.As(err, &pErr) || pErr.State != "MISSING" {
			t.Errorf("got %v, want ErrParentUnavailable with state MISSING", err)
		}
	})
}

func TestScanDepsShouldAutoInherit(t *testing.T) {
	t.Run("uses AutoInheritMetadata when AutoInheritFn is nil", func(t *testing.T) {
		depsTrue := ScanDeps{AutoInheritMetadata: true}
		if !depsTrue.shouldAutoInherit() {
			t.Errorf("expected shouldAutoInherit = true when AutoInheritMetadata = true")
		}

		depsFalse := ScanDeps{AutoInheritMetadata: false}
		if depsFalse.shouldAutoInherit() {
			t.Errorf("expected shouldAutoInherit = false when AutoInheritMetadata = false")
		}
	})

	t.Run("AutoInheritFn takes precedence over AutoInheritMetadata", func(t *testing.T) {
		depsFnTrue := ScanDeps{
			AutoInheritMetadata: false,
			AutoInheritFn:       func() bool { return true },
		}
		if !depsFnTrue.shouldAutoInherit() {
			t.Errorf("expected shouldAutoInherit = true when AutoInheritFn returns true")
		}

		depsFnFalse := ScanDeps{
			AutoInheritMetadata: true,
			AutoInheritFn:       func() bool { return false },
		}
		if depsFnFalse.shouldAutoInherit() {
			t.Errorf("expected shouldAutoInherit = false when AutoInheritFn returns false")
		}
	})
}

func TestNodeLockerSerializesSameNodeAndCleansUp(t *testing.T) {
	locker := &nodeLocker{
		locks: make(map[int64]*refCountedLock),
	}

	nodeID := int64(42)
	unlock1 := locker.lock(nodeID)

	acquired2 := make(chan bool)
	done2 := make(chan struct{})
	go func() {
		unlock2 := locker.lock(nodeID)
		acquired2 <- true
		unlock2()
		close(done2)
	}()

	// Ensure goroutine 2 is blocked waiting for lock on node 42
	select {
	case <-acquired2:
		t.Fatal("goroutine 2 acquired lock before unlock1 was released")
	case <-time.After(50 * time.Millisecond):
	}

	// Release lock 1
	unlock1()

	// Now goroutine 2 should acquire and release
	select {
	case <-acquired2:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("goroutine 2 timed out waiting for lock")
	}

	select {
	case <-done2:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("goroutine 2 timed out waiting to finish unlock")
	}

	// Verify the lock entry was cleaned up after both released
	locker.mu.Lock()
	remaining := len(locker.locks)
	locker.mu.Unlock()
	if remaining != 0 {
		t.Errorf("expected 0 remaining locks, got %d", remaining)
	}
}

func TestInheritMetadataShortCircuitsWhenTagsAlreadyMatch(t *testing.T) {
	ctx := context.Background()
	database := openTestDB(t)
	locID := seedLocation(t, database, "TIER2_EXPORTS", false)

	var parent, child sqlcgen.MediaNode
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		fastHashParent := "0123456789abcdef"
		fastHashChild := "fedcba9876543210"
		parent, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "parent-uuid-1",
			StorageLocationID: locID,
			FilePath:          "/tmp/parent.jpg",
			FileName:          "parent.jpg",
			FileExt:           "jpg",
			SizeBytes:         100,
			MtimeUnix:         time.Now().Unix(),
			FastHash:          &fastHashParent,
			IndexingStatus:    "INDEXED_SHALLOW",
			GraphStatus:       "LINKED",
			LifecycleState:    "ACTIVE",
		})
		if err != nil {
			return err
		}
		child, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          "child-uuid-1",
			StorageLocationID: locID,
			FilePath:          "/tmp/child.jpg",
			FileName:          "child.jpg",
			FileExt:           "jpg",
			SizeBytes:         100,
			MtimeUnix:         time.Now().Unix(),
			FastHash:          &fastHashChild,
			IndexingStatus:    "INDEXED_SHALLOW",
			GraphStatus:       "LINKED",
			LifecycleState:    "ACTIVE",
		})
		if err != nil {
			return err
		}
		// Set child.derived_from_id = parent-uuid-1 (already inherited)
		err = q.UpdateMediaNodePromotedColumns(ctx, sqlcgen.UpdateMediaNodePromotedColumnsParams{
			ID:            child.ID,
			DerivedFromID: sql.NullString{String: parent.NodeUuid, Valid: true},
		})
		if err != nil {
			return err
		}
		_, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID:     parent.ID,
			TargetNodeID:     child.ID,
			RelationshipType: "DERIVED_FROM",
			Confidence:       0.95,
			Tier:             2,
			Resolver:         "test",
			ReviewState:      "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("setup nodes: %v", err)
	}

	// Deps with prober=nil: if it does NOT short-circuit, it would fail with probe.ErrToolUnavailable.
	// Since tags already match (DerivedFromID == parent.NodeUuid and no extra metadata to inherit),
	// it must short-circuit and succeed without calling exiftool.
	deps := InheritDeps{
		DB:     database,
		Prober: nil,
	}
	tags, err := InheritMetadata(ctx, deps, child.ID)
	if err != nil {
		t.Fatalf("InheritMetadata failed: %v (expected short-circuit without needing prober)", err)
	}
	if tags["XMP-xmpMM:DerivedFrom"] != parent.NodeUuid {
		t.Errorf("got DerivedFrom %q, want %q", tags["XMP-xmpMM:DerivedFrom"], parent.NodeUuid)
	}
}
