package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/workers"
)

func writeFixtureFile(t *testing.T, path string, contents string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestWalkFindsAllFiles(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "a.txt"), "a")
	writeFixtureFile(t, filepath.Join(root, "sub", "b.txt"), "bb")
	writeFixtureFile(t, filepath.Join(root, "sub", "deeper", "c.txt"), "ccc")

	var got []Record
	err := Walk(context.Background(), root, func(r Record) error {
		got = append(got, r)
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("found %d files, want 3: %+v", len(got), got)
	}

	bySize := map[int64]bool{}
	for _, r := range got {
		bySize[r.Size] = true
		if r.IsDir {
			t.Errorf("Record for %s has IsDir=true, want false (directories should not be reported)", r.Path)
		}
	}
	for _, want := range []int64{1, 2, 3} {
		if !bySize[want] {
			t.Errorf("no file of size %d found among %+v", want, got)
		}
	}
}

func TestWalkReportsSymlinksWithoutFollowing(t *testing.T) {
	root := t.TempDir()
	target := filepath.Join(root, "real.txt")
	writeFixtureFile(t, target, "real content")
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(target, link); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	var sawSymlink bool
	err := Walk(context.Background(), root, func(r Record) error {
		if r.Path == link {
			sawSymlink = true
			if !r.IsSymlink {
				t.Error("Record for the symlink has IsSymlink=false, want true")
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}
	if !sawSymlink {
		t.Error("Walk did not report the symlink at all")
	}
}

func TestWalkRespectsContextCancellation(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "sub", "a.txt"), "x")

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled before Walk even starts

	err := Walk(ctx, root, func(r Record) error {
		return nil
	})
	if err == nil {
		t.Fatal("Walk with an already-cancelled context succeeded, want an error")
	}
}

// TestWalkReturnsBeforeHashingCompletes is the paired test the build plan
// names alongside T6: the walk itself must complete (or at least keep
// discovering files) without waiting for the slow "hashing" work it
// dispatches onto a workers.Pool to finish. It proves indexer.Walk never
// does the slow work inline -- it only enqueues.
func TestWalkReturnsBeforeHashingCompletes(t *testing.T) {
	root := t.TempDir()
	const fileCount = 20
	for i := 0; i < fileCount; i++ {
		writeFixtureFile(t, filepath.Join(root, "f"+string(rune('a'+i))+".txt"), "x")
	}

	pool := workers.New[string](2, fileCount)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	pool.Run(ctx)

	var hashesCompleted int32
	const hashDuration = 50 * time.Millisecond

	walkStart := time.Now()
	err := Walk(ctx, root, func(r Record) error {
		pool.Submit(ctx, workers.Job[string]{
			Key: r.Path,
			Run: func(context.Context) error {
				time.Sleep(hashDuration) // stand-in for real hashing/EXIF work
				atomic.AddInt32(&hashesCompleted, 1)
				return nil
			},
		})
		return nil
	})
	walkDur := time.Since(walkStart)
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	// The walk of 20 tiny files should take well under a millisecond of
	// real work; the important assertion is that it finished before ANY
	// individual 50ms hash job could have completed, proving the walk
	// never blocked on one.
	if walkDur >= hashDuration {
		t.Errorf("Walk took %s, want it to complete well before a single %s hash job", walkDur, hashDuration)
	}
	if got := atomic.LoadInt32(&hashesCompleted); got != 0 {
		t.Errorf("hashesCompleted = %d immediately after Walk returned, want 0 (they run concurrently, off the walk)", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for atomic.LoadInt32(&hashesCompleted) < fileCount && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&hashesCompleted); got != fileCount {
		t.Fatalf("hashesCompleted eventually = %d, want all %d to finish", got, fileCount)
	}
}

func TestWalkSkipsDotFilesAndTrash(t *testing.T) {
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "valid.jpg"), "valid")
	writeFixtureFile(t, filepath.Join(root, "sub", "valid2.jpg"), "valid2")
	writeFixtureFile(t, filepath.Join(root, ".trash", "deleted.jpg"), "trash")
	writeFixtureFile(t, filepath.Join(root, ".trash", "2026", "08", "nested.jpg"), "trash-nested")
	writeFixtureFile(t, filepath.Join(root, ".git", "config"), "git")
	writeFixtureFile(t, filepath.Join(root, ".DS_Store"), "ds_store")

	var paths []string
	err := Walk(context.Background(), root, func(r Record) error {
		paths = append(paths, filepath.Base(r.Path))
		return nil
	})
	if err != nil {
		t.Fatalf("Walk: %v", err)
	}

	if len(paths) != 2 {
		t.Fatalf("found %d files, want 2: %+v", len(paths), paths)
	}
	for _, p := range paths {
		if p != "valid.jpg" && p != "valid2.jpg" {
			t.Errorf("unexpected file walked: %s", p)
		}
	}
}

// One unreadable subdirectory (lost+found, a root-owned @eaDir, a flaky NAS
// folder) must not abort the whole walk: the readable files are still
// reported and the walk completes.
func TestWalkSkipsUnreadableSubdirectory(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("permission bits are not enforced for root")
	}
	root := t.TempDir()
	writeFixtureFile(t, filepath.Join(root, "before.txt"), "x")
	locked := filepath.Join(root, "locked")
	if err := os.Mkdir(locked, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile(t, filepath.Join(locked, "hidden-from-us.txt"), "x")
	if err := os.Chmod(locked, 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(locked, 0o755) })
	writeFixtureFile(t, filepath.Join(root, "zzz-after.txt"), "x")

	var seen []string
	err := Walk(context.Background(), root, func(r Record) error {
		seen = append(seen, filepath.Base(r.Path))
		return nil
	})
	if err != nil {
		t.Fatalf("Walk aborted on an unreadable subdirectory: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("seen = %v, want before.txt and zzz-after.txt", seen)
	}
}

func TestWalkStillFailsWhenRootIsMissing(t *testing.T) {
	err := Walk(context.Background(), filepath.Join(t.TempDir(), "does-not-exist"), func(Record) error { return nil })
	if err == nil {
		t.Fatal("Walk of a missing root must return an error, not silently report an empty tree")
	}
}
