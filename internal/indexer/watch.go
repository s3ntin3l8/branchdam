package indexer

import (
	"context"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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
// been opened.
func Watch(ctx context.Context, root string, debounce time.Duration, log *slog.Logger, onEvent func(Record) error) error {
	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return fmt.Errorf("indexer: create watcher: %w", err)
	}
	defer func() { _ = watcher.Close() }()

	if err := addRecursive(watcher, root); err != nil {
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
			handleEvent(watcher, event, deb, log, onEvent)

		case werr, ok := <-watcher.Errors:
			if !ok {
				return nil
			}
			if log != nil {
				log.Warn("indexer: watcher error", "err", werr)
			}
		}
	}
}

func handleEvent(watcher *fsnotify.Watcher, event fsnotify.Event, deb *debouncer, log *slog.Logger, onEvent func(Record) error) {
	// A newly created directory needs its own watch, and needs it added
	// promptly (not after the debounce delay) or files created inside it
	// in the same burst would be missed entirely.
	if event.Has(fsnotify.Create) {
		if info, err := os.Lstat(event.Name); err == nil && info.IsDir() {
			if err := addRecursive(watcher, event.Name); err != nil && log != nil {
				log.Warn("indexer: watch new directory", "path", event.Name, "err", err)
			}
		}
	}

	deb.trigger(event.Name, func() {
		info, err := os.Lstat(event.Name)
		if err != nil {
			// Removed (or never existed by the time we got to it) -- not
			// this package's decision what that means for the node graph;
			// the caller (internal/pipeline) owns MISSING-state transitions.
			return
		}
		if info.IsDir() {
			return
		}
		if err := onEvent(Record{
			Path:      event.Name,
			Size:      info.Size(),
			ModTime:   info.ModTime(),
			IsDir:     false,
			IsSymlink: info.Mode()&fs.ModeSymlink != 0,
		}); err != nil && log != nil {
			log.Warn("indexer: onEvent", "path", event.Name, "err", err)
		}
	})
}

// addRecursive adds a watch on root and every subdirectory beneath it.
// fs.WalkDir never follows symlinks for recursion (a symlink's DirEntry.
// IsDir() is always false, even when it points at a directory), so a
// symlinked directory is naturally never watched here -- following one is a
// storage.Guard-mediated decision, not this package's.
func addRecursive(watcher *fsnotify.Watcher, root string) error {
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() {
			return nil
		}
		return watcher.Add(path)
	})
}

// debouncer coalesces repeated triggers for the same key within delay into
// a single call, fired delay after the last trigger for that key.
type debouncer struct {
	delay time.Duration

	mu     sync.Mutex
	timers map[string]*time.Timer
}

func newDebouncer(delay time.Duration) *debouncer {
	return &debouncer{delay: delay, timers: make(map[string]*time.Timer)}
}

func (d *debouncer) trigger(key string, fn func()) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if t, ok := d.timers[key]; ok {
		t.Stop()
	}
	d.timers[key] = time.AfterFunc(d.delay, func() {
		d.mu.Lock()
		delete(d.timers, key)
		d.mu.Unlock()
		fn()
	})
}

func (d *debouncer) stopAll() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range d.timers {
		t.Stop()
	}
}
