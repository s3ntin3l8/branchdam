package prune

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNoFilesystemCallsInsideThePruneTransaction is the structural guard
// for the "the writer connection must not be held across a filesystem
// call" invariant.
//
// The server has ONE writer connection. The prune path's destructive
// step is an unlink, and its verification step stats the Tier-3
// ancestors. On an NFS or SMB mount either can block for seconds -- a
// stale NFS handle can block for minutes -- so running them inside the
// write transaction turns a slow mount into a server-wide write stall:
// every other write in the process queues behind the prune.
//
// The functional behaviour is covered by the behavioural tests in
// engine_test.go; this test pins the SHAPE, because the timing is not
// observable from a test without a slow filesystem.
//
// The check is deliberately simple: every database.InTx closure in
// engine.go is brace-matched, and none of them may mention a call that
// touches the filesystem. A new InTx that reintroduces one fails here.
func TestNoFilesystemCallsInsideThePruneTransaction(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("could not determine this test file's own path via runtime.Caller")
	}
	enginePath := filepath.Join(filepath.Dir(thisFile), "engine.go")

	data, err := os.ReadFile(enginePath)
	if err != nil {
		t.Fatalf("read %s: %v", enginePath, err)
	}
	src := string(data)

	// Calls that touch the filesystem. verifyOnDisk stats the ancestors
	// and the candidate; guard.Remove unlinks; the os.* calls are listed
	// so a direct syscall in a transaction fails here too.
	forbidden := []string{
		"guard.Remove",
		"verifyOnDisk",
		"os.Lstat",
		"os.Stat",
		"os.Remove",
		"os.ReadDir",
		"os.Open",
	}

	const marker = "database.InTx("
	offset := 0
	found := 0
	for {
		idx := strings.Index(src[offset:], marker)
		if idx < 0 {
			break
		}
		start := offset + idx
		found++

		// Find the closure's opening brace: the first '{' after the
		// call's argument list starts.
		brace := strings.Index(src[start:], "{")
		if brace < 0 {
			t.Fatalf("could not find the closure body for %s at offset %d", marker, start)
		}
		brace += start

		// Brace-match the closure body.
		depth := 0
		end := -1
		for i := brace; i < len(src); i++ {
			switch src[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					end = i
				}
			}
			if end >= 0 {
				break
			}
		}
		if end < 0 {
			t.Fatalf("could not brace-match the closure body at offset %d", brace)
		}
		body := src[brace : end+1]

		for _, call := range forbidden {
			if strings.Contains(body, call) {
				t.Errorf("the write transaction at offset %d calls %s -- the writer connection is a process-wide resource and a filesystem call on a slow mount must not hold it", start, call)
			}
		}
		offset = end + 1
	}

	if found == 0 {
		t.Fatal("no database.InTx call found in engine.go -- the path derivation or the code shape changed")
	}
}
