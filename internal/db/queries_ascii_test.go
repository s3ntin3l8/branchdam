package db

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestQueryFilesAreASCII guards against issue #413: sqlc v1.31.1's SQLite
// engine gets statement spans from its ANTLR parser in rune offsets and
// slices the source with them as byte offsets. A single multi-byte UTF-8
// character anywhere in internal/db/queries/*.sql (even inside a `--`
// comment) shifts every later statement's start/end left by that
// character's byte-rune delta, corrupting the generated Go: dropped
// placeholder digits, truncated RETURNING lists, stray garbage lines, and
// more -- see docs/schema.md's "sqlc risk: non-ASCII in query comments"
// section for the full catalogue and reproduction.
//
// Upstream: sqlc#4372 and sqlc#4523 (both closed-completed) name this bug;
// sqlc#4535 replaced the SQLite engine's ANTLR parser on main in August
// 2026, after v1.31.1 (the latest release as of this writing) shipped.
// Drop this guard once a release past v1.31.1 ships and is adopted.
//
// Scoped to internal/db/queries/ only: sqlc only parses schema files
// (internal/db/migrations/*.sql) for column/type info, never for statement
// spans, so non-ASCII there does not trigger the bug (verified by
// regenerating with the 21 non-ASCII migration lines ASCII-ized: byte-
// identical output).
func TestQueryFilesAreASCII(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this test file's own path via runtime.Caller")
	}
	queriesDir := filepath.Join(filepath.Dir(thisFile), "queries")

	entries, err := os.ReadDir(queriesDir)
	if err != nil {
		t.Fatalf("read %q: %v", queriesDir, err)
	}

	var violations []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".sql") {
			continue
		}
		path := filepath.Join(queriesDir, e.Name())
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %q: %v", path, err)
		}

		line, col := 1, 1
		for i := 0; i < len(data); {
			r, size := utf8.DecodeRune(data[i:])
			if r >= utf8.RuneSelf {
				violations = append(violations, fmt.Sprintf(
					"%s:%d:%d: non-ASCII rune %q (U+%04X) -- ASCII-only, see docs/schema.md's sqlc risk section",
					e.Name(), line, col, r, r))
			}
			if r == '\n' {
				line++
				col = 1
			} else {
				col++
			}
			i += size
		}
	}

	for _, v := range violations {
		t.Error(v)
	}
}
