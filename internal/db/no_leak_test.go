package db

import (
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
//     same way -- see check 2).
//  2. An EXPORTED struct field of this package's own type declarations with
//     one of those same types -- DB.Reader is exactly this shape (a field,
//     not a function) and is the one sanctioned exception, allowlisted by
//     name: it's bound to a query_only=ON pool, so it can't write regardless
//     of how it's exposed. A hypothetical symmetric DB.Writer field bound to
//     d.writer would reintroduce #85's capability under a different name and
//     a function-only check would miss it entirely. Unexported fields
//     (writer, reader) are excluded for the same reason as check 1.
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

	entries, err := os.ReadDir(dbDir)
	if err != nil {
		t.Fatalf("read %s: %v", dbDir, err)
	}

	const allowlistedField = "Reader" // query_only=ON, can't write regardless of exposure

	fset := token.NewFileSet()
	var violations []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dbDir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
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
					for _, fname := range field.Names {
						// Unexported fields (writer, reader) can never leak
						// outside this package regardless of their type --
						// only an exported field is a potential accessor.
						if !fname.IsExported() || fname.Name == allowlistedField {
							continue
						}
						violations = append(violations, name+": field "+fname.Name+" is a write-capable handle")
					}
				}
			}
			return true
		})
	}

	for _, v := range violations {
		t.Errorf("%s -- a production accessor outside InTx/ExecInTx reintroduces the write-capable, non-transactional escape hatch #85 removed (ReaderQueriesForTest); route through InTx instead", v)
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
