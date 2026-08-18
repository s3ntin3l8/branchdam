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
// (non-_test.go) function or method in this package that returns
// *sqlcgen.Queries directly, bypassing InTx/ExecInTx's transactional
// wrapping -- a write-capable, non-transactional escape hatch that punctures
// this package's own doc comment ("Nothing outside this package gets a raw
// *sql.DB") and InTx's ("This is the ONLY way to write to the database").
// DB.Reader is a *field*, not a function, and is deliberately excluded: it's
// bound to a query_only=ON pool, so it can't write regardless of how it's
// exposed. InTx itself takes a func(*sqlcgen.Queries) error *parameter*, not
// a *sqlcgen.Queries *return value* -- the transaction, not a raw handle, is
// what InTx hands control over, so its signature never trips this check.
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
			fn, ok := n.(*ast.FuncDecl)
			if !ok || fn.Type.Results == nil {
				return true
			}
			for _, res := range fn.Type.Results.List {
				if returnsSqlcgenQueriesPointer(res.Type) {
					violations = append(violations, name+": func "+fn.Name.Name)
				}
			}
			return true
		})
	}

	for _, v := range violations {
		t.Errorf("%s returns *sqlcgen.Queries directly -- a production accessor outside InTx/ExecInTx reintroduces the write-capable, non-transactional escape hatch #85 removed (ReaderQueriesForTest); route through InTx instead", v)
	}
}

func returnsSqlcgenQueriesPointer(expr ast.Expr) bool {
	star, ok := expr.(*ast.StarExpr)
	if !ok {
		return false
	}
	sel, ok := star.X.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkgIdent, ok := sel.X.(*ast.Ident)
	return ok && pkgIdent.Name == "sqlcgen" && sel.Sel.Name == "Queries"
}
