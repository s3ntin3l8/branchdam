package db

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoWriteCapableQueriesAccessorOutsideInTx guards against reintroducing
// something like the old ReaderQueriesForTest (#85): a production
// (non-_test.go) declaration in this package that exposes a write-capable
// database handle outside InTx/ExecInTx's transactional wrapping -- a
// non-transactional escape hatch that punctures this package's own doc
// comment ("Nothing outside this package gets a raw *sql.DB") and InTx's
// ("This is the ONLY way to write to the database"). Checks two shapes,
// since ReaderQueriesForTest's own bypass (a function returning one) is not
// the only way this invariant could be punctured again:
//
//  1. An EXPORTED function or method whose return type is *sqlcgen.Queries,
//     *sql.DB, *sql.Tx, or sqlcgen.DBTX -- ReaderQueriesForTest's own shape,
//     widened to the raw *sql.DB/*sql.Tx/DBTX handles the package doc's
//     broader invariant actually names, not just the one column of it
//     ReaderQueriesForTest happened to leak. Unexported functions are
//     excluded: they can't be called outside this package, so an internal
//     helper returning a raw handle for another package-internal function to
//     use isn't itself a leak (the unexported writer/reader fields work the
//     same way -- see check 2). Asymmetry, not a gap: a hypothetical future
//     exported Reader() method that just returns d.Reader would trip this
//     check even though the field itself is allowlisted below -- accepted,
//     since no such method exists today and adding an exemption for a
//     hypothetical one isn't worth the complexity yet.
//  2. An EXPORTED struct field of this package's own type declarations with
//     one of those same types -- DB.Reader is exactly this shape (a field,
//     not a function) and is the one sanctioned exception, allowlisted by
//     name: it's bound to a query_only=ON pool, so it can't write regardless
//     of how it's exposed. A hypothetical symmetric DB.Writer field bound to
//     d.writer would reintroduce #85's capability under a different name and
//     a function-only check would miss it entirely. Unexported fields
//     (writer, reader) are excluded for the same reason as check 1. Embedded
//     (anonymous) fields are included too -- struct{ *sqlcgen.Queries }
//     promotes a field literally named Queries, exactly as exported and
//     leaky as a named one, and Go's ast.Field.Names is empty for them, so
//     naively ranging over Names alone would silently miss this shape.
//
// InTx itself takes a func(*sqlcgen.Queries) error *parameter*, not a
// *sqlcgen.Queries *return value* -- the transaction, not a raw handle, is
// what InTx hands control over, so its signature never trips check 1, and it
// isn't a struct field so it never trips check 2 either.
//
// Polarity note (inverted from internal/auth/no_leak_test.go's guard): that
// test exempts _test.go files repo-wide and flags production files reading
// X-Authentik-* headers directly. Here the legitimate consumer of a raw
// *sqlcgen.Queries handle IS a _test.go file (internal/pipeline/commit_test.go,
// via InTx's callback) -- the violation this guards against is a
// *production* declaration in this package handing one out directly, which
// is exactly what ReaderQueriesForTest did.
//
// Scoped to internal/db's own directory (not internal/db/sqlcgen, generated
// and never hand-edited per CLAUDE.md, and not recursive): sqlcgen.New's own
// `func New(dbtx DBTX) *Queries` is the intended low-level building block
// InTx/Reader are built from, referenced unqualified (*Queries, not
// *sqlcgen.Queries) from inside its own package, so it never matches this
// check's qualified-selector pattern either way.
//
// Deliberately not chased further (a real but accepted limitation, not a gap
// to fix here): a type alias laundering one of these selectors, an
// unqualified same-package reference, a closure-returning-closure, or a
// return type widened to `any` would all evade this -- none of those are
// things a maintainer writes by accident while exposing a test helper, they
// require deliberately routing around a known guard. This is a regression
// tripwire for the ReaderQueriesForTest shape and its closest variants, not
// a hard security boundary.
func TestNoWriteCapableQueriesAccessorOutsideInTx(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this test file's own path via runtime.Caller")
	}
	dbDir := filepath.Dir(thisFile)

	violations, err := findWriteCapableHandleViolations(dbDir)
	if err != nil {
		t.Fatalf("scan %s: %v", dbDir, err)
	}
	for _, v := range violations {
		t.Errorf("%s -- a production accessor outside InTx/ExecInTx reintroduces the write-capable, non-transactional escape hatch #85 removed (ReaderQueriesForTest); route through InTx instead", v)
	}
}

// TestFindWriteCapableHandleViolationsCatchesKnownShapes is the fixture-based
// self-test of the detection logic itself: writes a small synthetic package
// exercising every shape the guard above claims to catch (named field,
// embedded field, exported func return) alongside shapes it should leave
// alone (the allowlisted Reader field, an unexported func, a _test.go file),
// and asserts exactly the expected violations are found. Exists because the
// production test above can only be verified by manually reintroducing and
// reverting a violation in this package's real source (as this PR's own
// description does) -- this test catches a regression in the detection
// logic itself without needing that manual cycle, and is what would have
// caught the embedded-field gap during review instead of after.
func TestFindWriteCapableHandleViolationsCatchesKnownShapes(t *testing.T) {
	dir := t.TempDir()
	const fixture = `package fixture

import "example.com/sqlcgen"

type Good struct {
	Reader *sqlcgen.Queries // allowlisted field name -- not a violation
}

type BadNamedField struct {
	Writer *sqlcgen.Queries
}

type BadEmbeddedField struct {
	*sqlcgen.Queries
}

func GoodFunc() error { return nil }

func BadFunc() *sqlcgen.Queries { return nil }

func badUnexportedFunc() *sqlcgen.Queries { return nil } // unexported -- not a violation
`
	if err := os.WriteFile(filepath.Join(dir, "fixture.go"), []byte(fixture), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	// A _test.go file in the same dir must never be scanned, even with an
	// obvious violation in it.
	const testFixture = `package fixture

import "example.com/sqlcgen"

func ViolationInTestFile() *sqlcgen.Queries { return nil }
`
	if err := os.WriteFile(filepath.Join(dir, "fixture_test.go"), []byte(testFixture), 0o644); err != nil {
		t.Fatalf("write test fixture: %v", err)
	}

	got, err := findWriteCapableHandleViolations(dir)
	if err != nil {
		t.Fatalf("findWriteCapableHandleViolations: %v", err)
	}

	wantSubstrings := []string{
		"field Writer is a write-capable handle",
		"field Queries is a write-capable handle", // the embedded field's promoted name
		"func BadFunc returns a write-capable handle directly",
	}
	for _, want := range wantSubstrings {
		found := false
		for _, v := range got {
			if strings.Contains(v, want) {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("violations = %v, want one containing %q", got, want)
		}
	}
	if len(got) != len(wantSubstrings) {
		t.Errorf("violations = %v (%d), want exactly %d (Reader, GoodFunc, badUnexportedFunc, and the _test.go file must not appear)", got, len(got), len(wantSubstrings))
	}
}

// findWriteCapableHandleViolations walks every non-_test.go .go file
// directly in dir (not recursive) and returns a description of each
// production declaration that exposes a write-capable database handle --
// see TestNoWriteCapableQueriesAccessorOutsideInTx's doc comment for what
// counts and why. Factored out from that test so the detection logic can be
// exercised against a fixture directory (see
// TestFindWriteCapableHandleViolationsCatchesKnownShapes), not only against
// this package's own current source.
func findWriteCapableHandleViolations(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}

	const allowlistedField = "Reader" // query_only=ON, can't write regardless of exposure

	fset := token.NewFileSet()
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		ast.Inspect(file, func(n ast.Node) bool {
			switch decl := n.(type) {
			case *ast.FuncDecl:
				// Unexported functions can't be called outside this
				// package, so an internal helper returning a raw handle
				// (e.g. for another package-internal function to use) is
				// not itself a leak -- only an exported entry point is.
				if decl.Type.Results == nil || !decl.Name.IsExported() {
					return true
				}
				for _, res := range decl.Type.Results.List {
					if isWriteCapableHandleType(res.Type) {
						violations = append(violations, name+": func "+decl.Name.Name+" returns a write-capable handle directly")
					}
				}
			case *ast.StructType:
				if decl.Fields == nil {
					return true
				}
				for _, field := range decl.Fields.List {
					if !isWriteCapableHandleType(field.Type) {
						continue
					}
					for _, fname := range fieldNames(field) {
						// Unexported fields (writer, reader) can never leak
						// outside this package regardless of their type --
						// only an exported field is a potential accessor.
						if !ast.IsExported(fname) || fname == allowlistedField {
							continue
						}
						violations = append(violations, name+": field "+fname+" is a write-capable handle")
					}
				}
			}
			return true
		})
	}
	return violations, nil
}

// fieldNames returns field's declared names, or -- for an embedded
// (anonymous) field, where ast.Field.Names is empty -- the single promoted
// name Go derives from the embedded type itself (e.g. an embedded
// *sqlcgen.Queries promotes as "Queries").
func fieldNames(field *ast.Field) []string {
	if len(field.Names) > 0 {
		names := make([]string, len(field.Names))
		for i, n := range field.Names {
			names[i] = n.Name
		}
		return names
	}
	if name := embeddedFieldName(field.Type); name != "" {
		return []string{name}
	}
	return nil
}

func embeddedFieldName(expr ast.Expr) string {
	if star, ok := expr.(*ast.StarExpr); ok {
		expr = star.X
	}
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.Ident:
		return e.Name
	default:
		return ""
	}
}

// isWriteCapableHandleType reports whether expr names one of the raw,
// write-capable database handle types this package's InTx exists to
// mediate: *sqlcgen.Queries (ReaderQueriesForTest's own shape), *sql.DB /
// *sql.Tx (the package doc's broader "no raw *sql.DB outside this package"
// invariant), or sqlcgen.DBTX (the interface both *sql.DB and *sql.Tx
// satisfy, referenced unqualified/without a pointer since it's an
// interface).
func isWriteCapableHandleType(expr ast.Expr) bool {
	if sel, ok := expr.(*ast.SelectorExpr); ok {
		return qualifiedIdent(sel) == "sqlcgen.DBTX"
	}
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	switch qualifiedIdent(sel) {
	case "sqlcgen.Queries", "sql.DB", "sql.Tx":
		return true
	default:
		return false
	}
}

func qualifiedIdent(sel *ast.SelectorExpr) string {
	pkgIdent, ok := sel.X.(*ast.Ident)
	if !ok {
		return ""
	}
	return pkgIdent.Name + "." + sel.Sel.Name
}
