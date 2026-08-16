// Package db owns the two SQLite connection pools (writer, reader), the
// per-connection pragma setup, and the embedded goose migration runner.
// Nothing outside this package gets a raw *sql.DB — see DB.InTx and
// DB.Reader.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/mattn/go-sqlite3"
	"github.com/pressly/goose/v3"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

const (
	driverRW = "sqlite3_branchdam_rw"
	driverRO = "sqlite3_branchdam_ro"
)

var registerOnce sync.Once

// registerDrivers registers the two named SQLite drivers exactly once per
// process. sql.Register panics on a duplicate name, and Open may reasonably
// be called more than once in tests.
func registerDrivers() {
	registerOnce.Do(func() {
		sql.Register(driverRW, &sqlite3.SQLiteDriver{ConnectHook: applyCommonPragmas})
		sql.Register(driverRO, &sqlite3.SQLiteDriver{ConnectHook: applyReaderPragmas})
	})
}

// pragmas applied on every pooled connection, both writer and reader. This is
// the Go analogue of the SQLAlchemy connect-event pragma block in
// runway-ai-usage-tracker/app/core/db.py: pragmas are connection-scoped in
// SQLite (unlike journal_mode, which persists in the DB header), so they
// must be reapplied on every new connection, not just once at startup.
//
//   - journal_mode=WAL / synchronous=NORMAL: readers proceed alongside a
//     writer instead of serializing through the same lock as every write.
//   - foreign_keys=ON: fix #6 (docs/schema.md) — every FK in the schema is
//     RESTRICT rather than CASCADE, but that constraint is INERT without
//     this pragma, which defaults OFF. Verified by TestForeignKeysEnforced.
//   - busy_timeout=10000: concurrent access waits up to 10s for the writer's
//     lock instead of failing immediately with "database is locked".
//   - temp_store=MEMORY / cache_size / mmap_size: read-performance pragmas,
//     values matched to runway's documented reasoning.
var commonPragmas = []string{
	"PRAGMA journal_mode=WAL",
	"PRAGMA synchronous=NORMAL",
	"PRAGMA foreign_keys=ON",
	"PRAGMA busy_timeout=10000",
	"PRAGMA temp_store=MEMORY",
	"PRAGMA cache_size=-20000",
	"PRAGMA mmap_size=268435456",
}

func applyCommonPragmas(conn *sqlite3.SQLiteConn) error {
	for _, p := range commonPragmas {
		if _, err := conn.Exec(p, nil); err != nil {
			return fmt.Errorf("apply pragma %q: %w", p, err)
		}
	}
	return nil
}

// applyReaderPragmas additionally sets query_only=ON, so the reader pool
// rejects writes at the SQLite connection level. Deliberately NOT `?mode=ro`
// in the DSN: a mode=ro handle can't create the -wal/-shm files if the DB
// hasn't already been opened for writing, which turns a fresh install into a
// confusing failure. query_only gives the same "readers can't write"
// guarantee without that first-boot ordering dependency.
func applyReaderPragmas(conn *sqlite3.SQLiteConn) error {
	if err := applyCommonPragmas(conn); err != nil {
		return err
	}
	if _, err := conn.Exec("PRAGMA query_only=ON", nil); err != nil {
		return fmt.Errorf("apply pragma query_only: %w", err)
	}
	return nil
}

// DB bundles the writer and reader pools. Every write goes through InTx (one
// transaction, the single writer connection); every read goes through
// Reader directly.
type DB struct {
	writer *sql.DB
	reader *sql.DB

	// Reader is bound to the reader pool. Safe for concurrent use by many
	// goroutines -- SetMaxOpenConns is sized to allow it.
	Reader *sqlcgen.Queries
}

// Open opens both pools against the same SQLite file at path, applies pending
// migrations on the writer pool, and returns a ready DB. Callers must call
// Close when done (see cmd/branchdam/main.go's shutdown sequence).
func Open(ctx context.Context, path string) (*DB, error) {
	registerDrivers()

	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0o750); err != nil {
			return nil, fmt.Errorf("create database directory %q: %w", dir, err)
		}
	}

	writer, err := sql.Open(driverRW, fmt.Sprintf("file:%s?_txlock=immediate", path))
	if err != nil {
		return nil, fmt.Errorf("open writer pool: %w", err)
	}
	// SQLite is single-writer. One connection is what makes the
	// read-then-write cycle check in internal/graph (PR 7) sound without any
	// additional application-level locking -- there is no second writer
	// connection that could interleave between the check and the insert.
	writer.SetMaxOpenConns(1)
	writer.SetMaxIdleConns(1)
	writer.SetConnMaxLifetime(0)

	reader, err := sql.Open(driverRO, fmt.Sprintf("file:%s", path))
	if err != nil {
		_ = writer.Close()
		return nil, fmt.Errorf("open reader pool: %w", err)
	}
	readerConns := runtime.NumCPU()
	if readerConns < 4 {
		readerConns = 4
	}
	reader.SetMaxOpenConns(readerConns)

	if err := writer.PingContext(ctx); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return nil, fmt.Errorf("ping writer pool: %w", err)
	}

	if err := migrate(writer); err != nil {
		_ = writer.Close()
		_ = reader.Close()
		return nil, fmt.Errorf("run migrations: %w", err)
	}

	return &DB{
		writer: writer,
		reader: reader,
		Reader: sqlcgen.New(reader),
	}, nil
}

// Close closes both pools. Safe to call once; matches the shutdown order in
// cmd/branchdam/main.go (HTTP server first, then the DB).
func (d *DB) Close() error {
	writerErr := d.writer.Close()
	readerErr := d.reader.Close()
	if writerErr != nil {
		return writerErr
	}
	return readerErr
}

// InTx runs fn inside a single write transaction on the writer pool's one
// connection. fn's error rolls the transaction back; a nil error commits.
// This is the ONLY way to write to the database -- see the package doc.
func (d *DB) InTx(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	if err := fn(sqlcgen.New(tx)); err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return fmt.Errorf("%w (rollback also failed: %v)", err, rbErr)
		}
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// ExecInTx runs a raw SQL statement inside a single write transaction on
// the writer pool's one connection, returning the sql.Result so callers can
// read RowsAffected. Test fixtures that need to set columns no sqlc query
// writes use this; production code should prefer a named query. Error
// handling mirrors InTx: an Exec error rolls the transaction back (surfacing
// a rollback failure), a Commit error is wrapped with its context.
func (d *DB) ExecInTx(ctx context.Context, query string, args ...any) (sql.Result, error) {
	tx, err := d.writer.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin transaction: %w", err)
	}

	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return nil, fmt.Errorf("%w (rollback also failed: %v)", err, rbErr)
		}
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit transaction: %w", err)
	}
	return res, nil
}

// ReaderQueriesForTest exposes a writer-bound Queries handle to tests that
// need to exercise persistence helpers inside a pipeline transaction.
func (d *DB) ReaderQueriesForTest() *sqlcgen.Queries {
	return sqlcgen.New(d.writer)
}

// migrate applies pending goose migrations against db (the writer pool).
// goose's package-level API carries global dialect/baseFS state, which is
// fine here -- this process opens exactly one database.
func migrate(writerDB *sql.DB) error {
	goose.SetBaseFS(migrationsFS)
	defer goose.SetBaseFS(nil)

	if err := goose.SetDialect("sqlite3"); err != nil {
		return fmt.Errorf("set goose dialect: %w", err)
	}

	if err := goose.Up(writerDB, migrationsDir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}
