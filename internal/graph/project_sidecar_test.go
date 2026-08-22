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
	nodesByPath         map[string]graph.Node
	nodesByFileName     map[string][]graph.Node
	nodesByFilenameStem map[string][]graph.Node
}

func (m *mockLookup) ByOriginalDocumentID(_ context.Context, _ string) ([]graph.Node, error) {
	return nil, nil
}

func (m *mockLookup) ByFilenameStem(_ context.Context, stem string) ([]graph.Node, error) {
	if m.nodesByFilenameStem != nil {
		if nodes, ok := m.nodesByFilenameStem[stem]; ok {
			return nodes, nil
		}
	}
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

func TestProjectSidecarResolver_ResolveXMP(t *testing.T) {
	tmpDir := t.TempDir()
	rawPath := filepath.Join(tmpDir, "DSC_0001.ARW")
	xmpPath := filepath.Join(tmpDir, "DSC_0001.xmp")
	xmpDoubleExtPath := filepath.Join(tmpDir, "DSC_0002.NEF.xmp")
	otherDir := t.TempDir()
	diffDirRawPath := filepath.Join(otherDir, "DSC_0003.ARW")
	diffDirXmpPath := filepath.Join(tmpDir, "DSC_0003.xmp")

	xmpContent := `<x:xmpmeta xmlns:x="adobe:ns:meta/">
		<rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">
			<rdf:Description rdf:about="" xmlns:crs="http://ns.adobe.com/camera-raw-settings/1.0/"/>
		</rdf:RDF>
	</x:xmpmeta>`

	if err := os.WriteFile(xmpPath, []byte(xmpContent), 0644); err != nil {
		t.Fatalf("failed to write xmp: %v", err)
	}
	if err := os.WriteFile(xmpDoubleExtPath, []byte(xmpContent), 0644); err != nil {
		t.Fatalf("failed to write double ext xmp: %v", err)
	}
	if err := os.WriteFile(diffDirXmpPath, []byte(xmpContent), 0644); err != nil {
		t.Fatalf("failed to write diff dir xmp: %v", err)
	}

	rawNode := graph.Node{ID: 101, FilePath: rawPath, FileName: "DSC_0001.ARW", FileExt: "arw", FilenameStem: "dsc_0001"}
	rawDoubleExtNode := graph.Node{ID: 102, FilePath: filepath.Join(tmpDir, "DSC_0002.NEF"), FileName: "DSC_0002.NEF", FileExt: "nef", FilenameStem: "dsc_0002"}
	diffDirRawNode := graph.Node{ID: 103, FilePath: diffDirRawPath, FileName: "DSC_0003.ARW", FileExt: "arw", FilenameStem: "dsc_0003"}

	lookup := &mockLookup{
		nodesByFilenameStem: map[string][]graph.Node{
			"dsc_0001": {rawNode},
			"dsc_0002": {rawDoubleExtNode},
			"dsc_0003": {diffDirRawNode},
		},
	}

	resolver := graph.NewProjectSidecarResolver(nil)

	t.Run("same stem sibling in same directory", func(t *testing.T) {
		childNode := graph.Node{ID: 201, FilePath: xmpPath, FileName: "DSC_0001.xmp", FileExt: "xmp", FilenameStem: "dsc_0001"}
		candidates, err := resolver.Resolve(context.Background(), childNode, lookup)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(candidates) != 1 {
			t.Fatalf("expected 1 candidate, got %d", len(candidates))
		}
		c := candidates[0]
		if c.ParentID != rawNode.ID || c.ChildID != childNode.ID {
			t.Errorf("expected ParentID %d, ChildID %d, got %+v", rawNode.ID, childNode.ID, c)
		}
		if c.Rel != "PROJECT_SIDECAR" || c.Confidence != 1.00 {
			t.Errorf("expected PROJECT_SIDECAR at 1.00, got %s at %f", c.Rel, c.Confidence)
		}
	})

	t.Run("double extension xmp sibling in same directory", func(t *testing.T) {
		childNode := graph.Node{ID: 202, FilePath: xmpDoubleExtPath, FileName: "DSC_0002.NEF.xmp", FileExt: "xmp", FilenameStem: "dsc_0002.nef"}
		candidates, err := resolver.Resolve(context.Background(), childNode, lookup)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(candidates) != 1 {
			t.Fatalf("expected 1 candidate, got %d", len(candidates))
		}
		c := candidates[0]
		if c.ParentID != rawDoubleExtNode.ID || c.ChildID != childNode.ID {
			t.Errorf("expected ParentID %d, ChildID %d, got %+v", rawDoubleExtNode.ID, childNode.ID, c)
		}
		if c.Rel != "PROJECT_SIDECAR" || c.Confidence != 1.00 {
			t.Errorf("expected PROJECT_SIDECAR at 1.00, got %s at %f", c.Rel, c.Confidence)
		}
	})

	t.Run("different directory candidate is not linked", func(t *testing.T) {
		childNode := graph.Node{ID: 203, FilePath: diffDirXmpPath, FileName: "DSC_0003.xmp", FileExt: "xmp", FilenameStem: "dsc_0003"}
		candidates, err := resolver.Resolve(context.Background(), childNode, lookup)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(candidates) != 0 {
			t.Fatalf("expected 0 candidates for different directory, got %d", len(candidates))
		}
	})

	t.Run("index-suffixed sibling is not auto-linked to a bare-stem xmp (#132 safeguard)", func(t *testing.T) {
		idxDir := t.TempDir()
		idxXmpPath := filepath.Join(idxDir, "IMG_1234.xmp")
		if err := os.WriteFile(idxXmpPath, []byte(xmpContent), 0644); err != nil {
			t.Fatalf("failed to write xmp: %v", err)
		}

		// naming.Stem collapses "IMG_1234-2.CR3" to the same stem "img_1234"
		// as the bare "IMG_1234.xmp" -- that must not yield an auto-accepted
		// PROJECT_SIDECAR edge, since "-2" means "second copy", not "same asset".
		idxRawNode := graph.Node{ID: 301, FilePath: filepath.Join(idxDir, "IMG_1234-2.CR3"), FileName: "IMG_1234-2.CR3", FileExt: "cr3", FilenameStem: "img_1234"}
		idxLookup := &mockLookup{
			nodesByFilenameStem: map[string][]graph.Node{
				"img_1234": {idxRawNode},
			},
		}

		childNode := graph.Node{ID: 302, FilePath: idxXmpPath, FileName: "IMG_1234.xmp", FileExt: "xmp", FilenameStem: "img_1234"}
		candidates, err := resolver.Resolve(context.Background(), childNode, idxLookup)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(candidates) != 0 {
			t.Fatalf("expected 0 candidates for index-suffixed sibling, got %d: %+v", len(candidates), candidates)
		}
	})

	t.Run("index-suffixed xmp exactly matching an index-suffixed sibling still links", func(t *testing.T) {
		idxDir := t.TempDir()
		idxXmpPath := filepath.Join(idxDir, "IMG_1234-2.xmp")
		if err := os.WriteFile(idxXmpPath, []byte(xmpContent), 0644); err != nil {
			t.Fatalf("failed to write xmp: %v", err)
		}

		idxRawNode := graph.Node{ID: 303, FilePath: filepath.Join(idxDir, "IMG_1234-2.CR3"), FileName: "IMG_1234-2.CR3", FileExt: "cr3", FilenameStem: "img_1234"}
		idxLookup := &mockLookup{
			nodesByFilenameStem: map[string][]graph.Node{
				"img_1234": {idxRawNode},
			},
		}

		childNode := graph.Node{ID: 304, FilePath: idxXmpPath, FileName: "IMG_1234-2.xmp", FileExt: "xmp", FilenameStem: "img_1234"}
		candidates, err := resolver.Resolve(context.Background(), childNode, idxLookup)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(candidates) != 1 {
			t.Fatalf("expected 1 candidate for exact index-suffixed match, got %d", len(candidates))
		}
		if candidates[0].ParentID != idxRawNode.ID {
			t.Errorf("expected ParentID %d, got %+v", idxRawNode.ID, candidates[0])
		}
	})
}
