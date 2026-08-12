// Package indexer discovers files: a full directory walk for initial scans,
// and an fsnotify watch for ongoing changes. Both do Lstat-level work only
// -- they never open a file -- so a full-disk walk completes in seconds
// regardless of file count. Hashing and metadata extraction are the
// caller's job, dispatched onto a bounded internal/workers.Pool (PR 6), not
// this package's.
package indexer

import (
	"context"
	"io/fs"
	"path/filepath"
	"time"
)

// Record is a shallow filesystem observation -- no bytes read, no hash
// computed.
type Record struct {
	Path      string
	Size      int64
	ModTime   time.Time
	IsDir     bool
	IsSymlink bool
}

// Walk performs a full directory walk rooted at root, calling onFile for
// every entry (files and symlinks; directories are traversed but not
// reported). Uses fs.WalkDir, which stats each entry from the directory
// read itself rather than a fresh syscall per file, and does not follow
// symlinks -- a symlink is reported via onFile with IsSymlink set, never
// traversed into, so the walk itself can never be misdirected. Whether to
// follow it (and what storage.Guard makes of the target) is the caller's
// decision.
//
// Walk returns as soon as ctx is cancelled or the walk completes; it never
// blocks on onFile doing slow work -- callers must not hash inline here,
// only enqueue onto a workers.Pool.
func Walk(ctx context.Context, root string, onFile func(Record) error) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		isSymlink := d.Type()&fs.ModeSymlink != 0
		if d.IsDir() {
			return nil
		}

		info, err := d.Info()
		if err != nil {
			return err
		}
		return onFile(Record{
			Path:      path,
			Size:      info.Size(),
			ModTime:   info.ModTime(),
			IsDir:     false,
			IsSymlink: isSymlink,
		})
	})
}
