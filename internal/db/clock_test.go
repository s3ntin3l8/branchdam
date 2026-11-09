package db

import (
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/clock"
)

// A frozen server clock reaches every timestamp the database writes: a column
// DEFAULT, an UPDATE ... = unixepoch(), and a WHERE comparison, on both pools.
func TestUnixepochFollowsTheClock(t *testing.T) {
	instant := time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)
	clock.Freeze(instant)
	t.Cleanup(clock.Unfreeze)
	database := openTestDB(t)

	if _, err := database.writer.Exec(`INSERT INTO scan_jobs (kind) VALUES ('FULL_SCAN')`); err != nil {
		t.Fatalf("insert (column DEFAULT): %v", err)
	}
	read := func(label string) (started, updated, finished int64) {
		t.Helper()
		var fin *int64
		if err := database.writer.QueryRow(`SELECT started_at, updated_at, finished_at FROM scan_jobs`).Scan(&started, &updated, &fin); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if fin != nil {
			finished = *fin
		}
		return
	}
	if started, updated, _ := read("after insert"); started != instant.Unix() || updated != instant.Unix() {
		t.Fatalf("DEFAULT (unixepoch()): started %d updated %d, want %d", started, updated, instant.Unix())
	}

	if _, err := database.writer.Exec(`UPDATE scan_jobs SET state = 'COMPLETED', finished_at = unixepoch()`); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, _, finished := read("after update"); finished != instant.Unix() {
		t.Fatalf("SET finished_at = unixepoch(): %d, want %d", finished, instant.Unix())
	}

	// A comparison against the clock, on the reader pool: the row started "now".
	var n int
	if err := database.reader.QueryRow(`SELECT COUNT(*) FROM scan_jobs WHERE started_at = unixepoch()`).Scan(&n); err != nil {
		t.Fatalf("reader: %v", err)
	}
	if n != 1 {
		t.Fatalf("WHERE started_at = unixepoch() on the reader pool matched %d rows, want 1", n)
	}
}

// Unfrozen, unixepoch() is the system clock (to the second).
func TestUnixepochIsTheSystemClockUnlessFrozen(t *testing.T) {
	clock.Unfreeze()
	database := openTestDB(t)
	var got int64
	if err := database.writer.QueryRow(`SELECT unixepoch()`).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(time.Unix(got, 0)); d < -2*time.Second || d > 2*time.Second {
		t.Fatalf("unixepoch() = %d, %s from the system clock", got, d)
	}
}
