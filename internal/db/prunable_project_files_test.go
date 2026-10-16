package db_test

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/projectfile"
)

// TestListPrunableNodesExcludesEveryProjectFileExtension keeps the
// hard-coded `NOT LIKE` list in ListPrunableNodes (queries/media_nodes.sql)
// in step with internal/projectfile's registry. A project file or sidecar
// holds unique content that cannot be rebuilt from its Tier-3 master, so a
// parser registered for a new extension without a matching exclusion would
// silently make those files prune-eligible.
func TestListPrunableNodesExcludesEveryProjectFileExtension(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this test file's own path via runtime.Caller")
	}
	raw, err := os.ReadFile(filepath.Join(filepath.Dir(thisFile), "queries", "media_nodes.sql"))
	if err != nil {
		t.Fatalf("read media_nodes.sql: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "-- name: ListPrunableNodes ")
	if start < 0 {
		t.Fatal("ListPrunableNodes not found in media_nodes.sql")
	}
	query := src[start:]
	if next := strings.Index(query[1:], "-- name: "); next >= 0 {
		query = query[:next+1]
	}

	exts := projectfile.SupportedExtensions()
	if len(exts) == 0 {
		t.Fatal("projectfile registry is empty; the parsers' init() registrations did not run")
	}
	for _, ext := range exts {
		want := "lower(n.file_name) NOT LIKE '%." + ext + "'"
		if !strings.Contains(query, want) {
			t.Errorf("ListPrunableNodes does not exclude registered project-file extension %q; add `AND %s`", ext, want)
		}
	}
}
