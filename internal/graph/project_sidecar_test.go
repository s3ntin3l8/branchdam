package graph_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	_ "github.com/s3ntin3l8/branchdam/internal/projectfile"
)

type mockLookup struct {
	nodesByPath     map[string]graph.Node
	nodesByFileName map[string][]graph.Node
}

func (m *mockLookup) ByOriginalDocumentID(_ context.Context, _ string) ([]graph.Node, error) {
	return nil, nil
}

func (m *mockLookup) ByFilenameStem(_ context.Context, _ string) ([]graph.Node, error) {
	return nil, nil
}

func (m *mockLookup) BySpatialTemporal(_ context.Context, _ string, _ time.Time, _ time.Duration, _ int64) ([]graph.Node, error) {
	return nil, nil
}

func (m *mockLookup) ByPath(_ context.Context, filePath string) (*graph.Node, error) {
	if n, ok := m.nodesByPath[filePath]; ok {
		return &n, nil
	}
	return nil, nil
}

func (m *mockLookup) ByFileName(_ context.Context, fileName string) ([]graph.Node, error) {
	if nodes, ok := m.nodesByFileName[fileName]; ok {
		return nodes, nil
	}
	return nil, nil
}

func TestProjectSidecarResolver_Resolve(t *testing.T) {
	tmpDir := t.TempDir()
	damJsonPath := filepath.Join(tmpDir, "project.dam.json")

	content := `{
		"version": "1.0",
		"project_name": "Test Project",
		"media_references": [
			{ "raw_path": "D:\\Footage\\Clip01.mov", "role": "media" },
			{ "raw_path": "/storage/projects/Footage/Clip02.mov", "role": "media" },
			{ "raw_path": "D:\\Footage\\Ambiguous.mov", "role": "media" }
		]
	}`
	if err := os.WriteFile(damJsonPath, []byte(content), 0644); err != nil {
		t.Fatalf("failed to write test dam.json: %v", err)
	}

	lookup := &mockLookup{
		nodesByPath: map[string]graph.Node{
			"/storage/projects/Footage/Clip01.mov": {ID: 10, FilePath: "/storage/projects/Footage/Clip01.mov", FileName: "Clip01.mov"},
			"/storage/projects/Footage/Clip02.mov": {ID: 20, FilePath: "/storage/projects/Footage/Clip02.mov", FileName: "Clip02.mov"},
		},
		nodesByFileName: map[string][]graph.Node{
			"Ambiguous.mov": {
				{ID: 30, FilePath: "/storage/loc1/Ambiguous.mov", FileName: "Ambiguous.mov"},
				{ID: 31, FilePath: "/storage/loc2/Ambiguous.mov", FileName: "Ambiguous.mov"},
			},
		},
	}

	rewrites := []config.PathRewrite{
		{From: `D:\Footage\`, To: `/storage/projects/Footage/`},
	}

	resolver := graph.NewProjectSidecarResolver(rewrites)

	if resolver.Name() != "project_sidecar" {
		t.Errorf("expected resolver name 'project_sidecar', got %s", resolver.Name())
	}
	if resolver.Tier() != 1 {
		t.Errorf("expected resolver tier 1, got %d", resolver.Tier())
	}

	childNode := graph.Node{ID: 99, FilePath: damJsonPath}

	candidates, err := resolver.Resolve(context.Background(), childNode, lookup)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should resolve Clip01.mov (ID 10 via rewrite) and Clip02.mov (ID 20 via exact match).
	// Ambiguous.mov has 2 candidates so it should be skipped.
	if len(candidates) != 2 {
		t.Fatalf("expected 2 candidates, got %d", len(candidates))
	}

	for _, c := range candidates {
		if c.Tier != 1 {
			t.Errorf("expected Tier 1 candidate, got %d", c.Tier)
		}
		if c.Confidence != 1.00 {
			t.Errorf("expected Confidence 1.00, got %f", c.Confidence)
		}
		if c.Rel != "PROJECT_SIDECAR" {
			t.Errorf("expected Rel PROJECT_SIDECAR, got %s", c.Rel)
		}
	}
}
