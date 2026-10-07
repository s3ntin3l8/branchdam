package indexer

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
)

// Watch watches root and all its subdirectories for changes, calling
// onEvent for each path once activity on it has been quiet for debounce --
// so a burst of writes to the same file (e.g. an export tool flushing in
// chunks) produces one callback, not one per write. New subdirectories are
// watched automatically as they're created; fsnotify has no native
// recursive-watch API, so this package adds one path per directory itself.
//
// Watch blocks until ctx is cancelled or an unrecoverable error occurs.
// Lstat only, same as Walk -- onEvent's Record never implies the file has
// been opened. onRemove fires for a path that has genuinely disappeared
// (os.Lstat returns fs.ErrNotExist) after the debounce window; it may be nil.
func Watch(ctx context.Context, root string, debounce time.Duration, log *slog.Logger,
	onEvent func(Record) error, onRemove func(path string) error) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("indexer: create watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := addRecursive(watcher, root, log); err != nil {
		return fmt.Errorf("indexer: watch %q: %w", root, err)
	}

	deb := newDebouncer(debounce)
	defer deb.stopAll()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()

		case event, ok := <-watcher.Events:
			if !ok {
				return nil
			}
			handleEvent(watcher, root, event, deb, log, onEvent, onRemove)

		case werr, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			logWatcherError(log, werr)
		}
	}
}

// logWatcherError logs an error from fsnotify's own Errors channel,
// distinguishing a genuine event-queue overflow (the kernel dropped events
// because this process wasn't draining fsnotify's channel fast enough --
// real, silent data loss at the OS level, distinct from any backpressure
// policy this package's own consumer applies) from any other watcher error,
// so an operator can tell "events were dropped" from "nothing happened."
// A full rescan of the affected location is the existing self-healing path
// for whatever an overflow missed -- the same fallback watcher.go's package
// doc already relies on for un-watched directory renames.
func logWatcherError(log *slog.Logger, werr error) {
	if log == nil {
		return
	}
	if errors.Is(werr, fsnotify.ErrEventOverflow) {
		log.Error("indexer: watch queue overflowed, filesystem events were dropped by the kernel -- run a full rescan of this location to catch up", "err", werr)
		return
	}
	log.Warn("indexer: watcher error", "err", werr)
}

// isHiddenBelow reports whether path (which must be root or below it) has a
// hidden or .trash component BELOW root. Components of root itself don't
// count: a library living under /home/u/.local/share/... or /mnt/.snapshots/...
// must not have every event silently dropped.
func isHiddenBelow(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return false
	}
	for _, part := range strings.Split(rel, string(filepath.Separator)) {
		if part == ".trash" || (len(part) > 0 && part[0] == '.') {
			return true
		}
	}
	return false
}

func handleEvent(watcher *fsnotify.Watcher, root string, event fsnotify.Event, deb *debouncer, log *slog.Logger, onEvent func(Record) error, onRemove func(path string) error) {
	// Skip .trash and hidden paths from being watched or processed
	if isHiddenBelow(root, event.Name) {
		return
	}

	// A newly created directory needs its own watch, and needs it added
	// promptly (not after the debounce delay) or files created inside it
	// in the same burst would be missed entirely. A directory that arrives
	// already populated (moved or copied in) produces no events for the
	// files it carries, so walk it and report them too -- otherwise they
	// stay unindexed until a manual full scan.
	if event.Has(fsnotify.Create) {
		if info, err := os.Lstat(event.Name); err == nil && info.IsDir() {
			if err := addRecursive(watcher, event.Name, log); err != nil && log != nil {
				log.Warn("indexer: watch new directory", "err", err)
			}
			_ = filepath.WalkDir(event.Name, func(p string, d fs.DirEntry, werr error) error {
				if werr != nil {
					return nil
				}
				if d.IsDir() {
					if p != event.Name && isHiddenBelow(root, p) {
						return filepath.SkipDir
					}
					return nil
				}
				if !isHiddenBelow(root, p) {
					triggerPath(deb, log, p, onEvent, onRemove)
				}
				return nil
			})
		}
	}

	triggerPath(deb, log, event.Name, onEvent, onRemove)
}

// triggerPath debounces one path and, once quiet, reports it: onEvent for a
// present file, onRemove for a path that no longer exists.
func triggerPath(deb *debouncer, log *slog.Logger, path string, onEvent func(Record) error, onRemove func(path string) error) {
	deb.trigger(path, func() {
		info, err := os.Lstat(path)
		if err == nil && !info.IsDir() {
			// A debounce only says "no event for a while", not "the writer is
			// done": a multi-GB copy over SMB pauses longer than the debounce
			// and would be hashed half-written, indexing a wrong hash that
			// later archives the node as a spurious version collision. Sample
			// the file again one debounce later; if it moved, re-arm and wait.
			time.Sleep(deb.delay)
			again, aerr := os.Lstat(path)
			if aerr == nil && (again.Size() != info.Size() || !again.ModTime().Equal(info.ModTime())) {
				triggerPath(deb, log, path, onEvent, onRemove)
				return
			}
			info, err = again, aerr
		}
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) && onRemove != nil {
				if rerr := onRemove(path); rerr != nil && log != nil {
					log.Warn("indexer: onRemove", "path", path, "err", rerr)
				}
			}
			return
		}
		if info.IsDir() {
			return
		}
		if err := onEvent(Record{
			Path:      path,
			Size:      info.Size(),
			ModTime:   info.ModTime(),
			IsDir:     false,
			IsSymlink: info.Mode()&fs.ModeSymlink != 0,
		}); err != nil && log != nil {
			log.Warn("indexer: onEvent", "path", path, "err", err)
		}
	})
}

// addRecursive adds a watch on root and every subdirectory beneath it.
// fs.WalkDir never follows symlinks for recursion (a symlink's DirEntry.
// IsDir() is always false, even when it points at a directory), so a
// symlinked directory is naturally never watched here -- following one is a
// storage.Guard-mediated decision, not this package's.
//
// A failure at the root itself is returned; below it, an unreadable
// subdirectory or a failed watch add (e.g. the inotify watch limit) is
// logged and skipped so one bad directory can't stop the watcher from
// starting. Directories skipped this way are covered by the next full scan.
func addRecursive(watcher *fsnotify.Watcher, root string, log *slog.Logger) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == root {
				return err
			}
			if log != nil {
				log.Warn("indexer: not watching unreadable path", "path", path, "err", err)
			}
			if d != nil && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.IsDir() {
			return nil
		}
		if path != root && (d.Name() == ".trash" || d.Name()[0] == '.') {
			return filepath.SkipDir
		}
		if addErr := watcher.Add(path); addErr != nil {
			if path == root {
				return addErr
			}
			if log != nil {
				log.Warn("indexer: could not watch directory (inotify limit?); run a full scan to cover it", "path", path, "err", addErr)
			}
		}
		return nil
	})
}

// debouncer coalesces repeated triggers for the same key within delay into
// a single call, fired delay after the last trigger for that key.
type debouncer struct {
	delay time.Duration

	mu     sync.Mutex
	closed bool
	timers map[string]*time.Timer
}

func newDebouncer(delay time.Duration) *debouncer {
	return &debouncer{delay: delay, timers: make(map[string]*time.Timer)}
}

func (d *debouncer) trigger(key string, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return
	}
	if t, ok := d.timers[key]; ok {
		t.Stop()
	}
	d.timers[key] = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			return
		}
		delete(d.timers, key)
		d.mu.Unlock()
		fn()
	})
}

func (d *debouncer) stopAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	for k, t := range d.timers {
		t.Stop()
		delete(d.timers, k)
	}
}
