package auth

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoDirectAuthentikHeaderReads is the cheap, structural half of the
// header-spoofing defense: BrowserChain (browser.go) is the only
// PRODUCTION code permitted to reference "X-Authentik" anywhere in the
// repo. This test walks every non-test .go file outside internal/auth and
// fails if any of them mention it -- a future handler reading
// r.Header.Get("X-Authentik-...") directly (bypassing auth.From) would
// reintroduce exactly the spoofing risk AgentChain's header-stripping
// exists to close, and this test fails loudly the day that happens rather
// than relying on code review to catch it.
//
// _test.go files are excluded repo-wide (not just in internal/auth): a
// test constructing a request with req.Header.Set("X-Authentik-Username",
// ...) to simulate what Traefik/Authentik would send is a normal,
// necessary testing pattern (see internal/httpapi/routes_test.go), not the
// production read-path this guard exists to catch.
func TestNoDirectAuthentikHeaderReads(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this test file's own path via runtime.Caller")
	}
	// this file is internal/auth/no_leak_test.go -> repo root is three
	// directories up.
	authDir := filepath.Dir(thisFile)
	repoRoot := filepath.Dir(filepath.Dir(authDir))

	if _, err := os.Stat(filepath.Join(repoRoot, "go.mod")); err != nil {
		t.Fatalf("computed repo root %q doesn't contain go.mod, path derivation is wrong: %v", repoRoot, err)
	}

	var violations []string
	err := filepath.WalkDir(repoRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "dist", "web", ".mullion-worktrees":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if strings.HasPrefix(path, authDir+string(filepath.Separator)) {
			return nil // internal/auth itself is the one allowed place
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if strings.Contains(strings.ToLower(string(data)), "x-authentik") {
			rel, _ := filepath.Rel(repoRoot, path)
			violations = append(violations, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk repo: %v", err)
	}

	for _, v := range violations {
		t.Errorf("%s references X-Authentik-* outside internal/auth -- read the Principal via auth.From(ctx) instead", v)
	}
}
