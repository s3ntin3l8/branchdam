package db_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

func TestAgentScratchTelemetry_Lifecycle(t *testing.T) {
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "telemetry-test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		_ = database.Close()
	})

	now := time.Now().Unix()

	// 1. Initial upsert
	var inserted sqlcgen.AgentScratchTelemetry
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		inserted, err = q.UpsertAgentScratchTelemetry(ctx, sqlcgen.UpsertAgentScratchTelemetryParams{
			AgentID:                "agent-macbook-1",
			ClientVersion:          "1.0.0",
			TimestampUnix:          now,
			MountPath:              "/Volumes/Scratch",
			TotalBytes:             1000000000000,
			FreeBytes:              400000000000,
			UsedBytes:              600000000000,
			MirrorsSizeBytes:       100000000000,
			RenderCacheSizeBytes:   300000000000,
			ProxiesSizeBytes:       200000000000,
			PrunableBytes:          250000000000,
			LastPruneTimestampUnix: now - 3600,
			LastReclaimedBytes:     50000000000,
			LastPruneDurationMs:    1500,
			PrunedItemCounts:       `{"mirrors":5,"renderCacheProjects":2}`,
		})
		return err
	})
	if err != nil {
		t.Fatalf("UpsertAgentScratchTelemetry: %v", err)
	}

	if inserted.AgentID != "agent-macbook-1" {
		t.Errorf("AgentID = %q, want agent-macbook-1", inserted.AgentID)
	}
	if inserted.FreeBytes != 400000000000 {
		t.Errorf("FreeBytes = %d, want 400000000000", inserted.FreeBytes)
	}

	// 2. Query single agent
	got, err := database.Reader.GetAgentScratchTelemetry(ctx, "agent-macbook-1")
	if err != nil {
		t.Fatalf("GetAgentScratchTelemetry: %v", err)
	}
	if got.MountPath != "/Volumes/Scratch" {
		t.Errorf("MountPath = %q, want /Volumes/Scratch", got.MountPath)
	}
	if got.LastReclaimedBytes != 50000000000 {
		t.Errorf("LastReclaimedBytes = %d, want 50000000000", got.LastReclaimedBytes)
	}

	// 3. Upsert update (conflict resolution)
	updatedTime := now + 120
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.UpsertAgentScratchTelemetry(ctx, sqlcgen.UpsertAgentScratchTelemetryParams{
			AgentID:                "agent-macbook-1",
			ClientVersion:          "1.0.1",
			TimestampUnix:          updatedTime,
			MountPath:              "/Volumes/Scratch",
			TotalBytes:             1000000000000,
			FreeBytes:              350000000000,
			UsedBytes:              650000000000,
			MirrorsSizeBytes:       120000000000,
			RenderCacheSizeBytes:   330000000000,
			ProxiesSizeBytes:       200000000000,
			PrunableBytes:          280000000000,
			LastPruneTimestampUnix: updatedTime,
			LastReclaimedBytes:     80000000000,
			LastPruneDurationMs:    2100,
			PrunedItemCounts:       `{"mirrors":8}`,
		})
		return err
	})
	if err != nil {
		t.Fatalf("Upsert update: %v", err)
	}

	// 4. List all agents
	list, err := database.Reader.ListAgentScratchTelemetry(ctx)
	if err != nil {
		t.Fatalf("ListAgentScratchTelemetry: %v", err)
	}
	if len(list) != 1 {
		t.Fatalf("len(list) = %d, want 1", len(list))
	}
	if list[0].ClientVersion != "1.0.1" {
		t.Errorf("ClientVersion = %q, want 1.0.1", list[0].ClientVersion)
	}
	if list[0].FreeBytes != 350000000000 {
		t.Errorf("FreeBytes = %d, want 350000000000", list[0].FreeBytes)
	}

	// 5. Delete agent
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.DeleteAgentScratchTelemetry(ctx, "agent-macbook-1")
	})
	if err != nil {
		t.Fatalf("DeleteAgentScratchTelemetry: %v", err)
	}

	// 6. Verify empty list
	listAfter, err := database.Reader.ListAgentScratchTelemetry(ctx)
	if err != nil {
		t.Fatalf("List after delete: %v", err)
	}
	if len(listAfter) != 0 {
		t.Errorf("len(listAfter) = %d, want 0 after deletion", len(listAfter))
	}
}
