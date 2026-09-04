package indexer

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
)

func testLogger() *slog.Logger { return slog.New(slog.DiscardHandler) }

// noRemovals is the stub onRemove for tests that never delete files: it logs
// rather than fails so a spurious removal callback is visible in the output.
func noRemovals(t *testing.T) func(string) error {
	t.Helper()
	return func(path string) error {
		t.Logf("watch removal (unexpected in this test): %s", path)
		return nil
	}
}

// waitFor polls cond until it's true or the deadline passes, failing the
// test otherwise. fsnotify delivery is asynchronous (a real kernel queue),
// so watch tests poll rather than asserting on a fixed sleep.
func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", timeout)
}

func TestWatchReportsNewFile(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var seen []string

	go func() {
		_ = Watch(ctx, root, 20*time.Millisecond, testLogger(), func(r Record) error {
			mu.Lock()
			seen = append(seen, r.Path)
			mu.Unlock()
			return nil
		}, noRemovals(t))
	}()

	// Give the watcher a moment to install its inotify watch before the
	// write happens, or the event could be missed entirely.
	time.Sleep(50 * time.Millisecond)

	target := filepath.Join(root, "new.txt")
	if err := os.WriteFile(target, []byte("hello"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range seen {
			if p == target {
				return true
			}
		}
		return false
	})
}

func TestWatchDebouncesRapidWrites(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var callCount int

	const debounce = 100 * time.Millisecond
	go func() {
		_ = Watch(ctx, root, debounce, testLogger(), func(r Record) error {
			mu.Lock()
			callCount++
			mu.Unlock()
			return nil
		}, noRemovals(t))
	}()
	time.Sleep(50 * time.Millisecond)

	target := filepath.Join(root, "burst.txt")
	// Ten rapid writes within well under the debounce window should
	// coalesce into far fewer than ten callbacks.
	for i := 0; i < 10; i++ {
		if err := os.WriteFile(target, []byte{byte(i)}, 0o644); err != nil {
			t.Fatalf("write fixture: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Wait past the debounce window from the last write, then check.
	time.Sleep(debounce + 200*time.Millisecond)

	mu.Lock()
	got := callCount
	mu.Unlock()
	if got == 0 {
		t.Fatal("debounced callback never fired")
	}
	if got >= 10 {
		t.Errorf("callCount = %d, want well under 10 (writes should have coalesced)", got)
	}
}

func TestWatchTracksNewSubdirectory(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var seen []string
	go func() {
		_ = Watch(ctx, root, 20*time.Millisecond, testLogger(), func(r Record) error {
			mu.Lock()
			seen = append(seen, r.Path)
			mu.Unlock()
			return nil
		}, noRemovals(t))
	}()
	time.Sleep(50 * time.Millisecond)

	subdir := filepath.Join(root, "newsub")
	if err := os.Mkdir(subdir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	// Give addRecursive time to pick up and watch the new directory before
	// a file is created inside it.
	time.Sleep(100 * time.Millisecond)

	target := filepath.Join(subdir, "inside.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("write fixture in new subdir: %v", err)
	}

	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range seen {
			if p == target {
				return true
			}
		}
		return false
	})
}

func TestWatchReportsRemoval(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var mu sync.Mutex
	var removed []string
	go func() {
		_ = Watch(ctx, root, 20*time.Millisecond, testLogger(), func(r Record) error { return nil },
			func(path string) error {
				mu.Lock()
				removed = append(removed, path)
				mu.Unlock()
				return nil
			})
	}()
	time.Sleep(50 * time.Millisecond)

	target := filepath.Join(root, "gone.txt")
	if err := os.WriteFile(target, []byte("x"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.Remove(target); err != nil {
		t.Fatalf("remove fixture: %v", err)
	}
	waitFor(t, 3*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, p := range removed {
			if p == target {
				return true
			}
		}
		return false
	})
}

// TestLogWatcherErrorDistinguishesOverflow backs #87: an operator must be
// able to tell "fsnotify's kernel queue overflowed, events were dropped"
// apart from any other watcher error in the logs -- a plain uniform "watch
// error" line for both looked identical to "nothing happened" at a glance.
func TestLogWatcherErrorDistinguishesOverflow(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))

	logWatcherError(log, fsnotify.ErrEventOverflow)
	overflowMsg := buf.String()
	if !strings.Contains(overflowMsg, "overflow") {
		t.Errorf("overflow log missing the word 'overflow': %q", overflowMsg)
	}
	if !strings.Contains(overflowMsg, "level=ERROR") {
		t.Errorf("overflow log level = %q, want ERROR (this is real, silent data loss at the kernel level)", overflowMsg)
	}

	buf.Reset()
	logWatcherError(log, errors.New("some other watcher error"))
	otherMsg := buf.String()
	if strings.Contains(otherMsg, "overflow") {
		t.Errorf("a non-overflow error was logged as an overflow: %q", otherMsg)
	}
	if !strings.Contains(otherMsg, "level=WARN") {
		t.Errorf("non-overflow log level = %q, want WARN", otherMsg)
	}

	if overflowMsg == otherMsg {
		t.Error("overflow and non-overflow errors produced identical log lines -- not distinguishable")
	}
}

func TestDebouncerStopAllPreventsPendingCallbacks(t *testing.T) {
	d := newDebouncer(20 * time.Millisecond)

	var mu sync.Mutex
	called := false

	d.trigger("key", func() {
		mu.Lock()
		called = true
		mu.Unlock()
	})

	// Stop before timer fires
	d.stopAll()

	// Wait past delay
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got := called
	mu.Unlock()

	if got {
		t.Errorf("callback was executed after stopAll")
	}

	// Triggering on a stopped debouncer should also be a no-op
	d.trigger("key2", func() {
		mu.Lock()
		called = true
		mu.Unlock()
	})
	time.Sleep(50 * time.Millisecond)

	mu.Lock()
	got2 := called
	mu.Unlock()

	if got2 {
		t.Errorf("callback was executed on closed debouncer")
	}
}
