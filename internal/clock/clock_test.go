package clock

import (
	"testing"
	"time"
)

var instant = time.Date(2026, 10, 15, 9, 0, 0, 0, time.UTC)

func TestNowIsTheSystemClockUntilFrozen(t *testing.T) {
	Unfreeze()
	if d := time.Since(Now()); d < 0 || d > time.Minute {
		t.Fatalf("Now is %s away from the system clock", d)
	}
	if Frozen() {
		t.Fatal("Frozen before Freeze")
	}
}

func TestFreezeFixesNowAndUnix(t *testing.T) {
	t.Cleanup(Unfreeze)
	Freeze(instant.In(time.FixedZone("x", 7200)))
	time.Sleep(5 * time.Millisecond)
	if got := Now(); !got.Equal(instant) || got.Location() != time.UTC {
		t.Fatalf("Now = %v, want %v UTC", got, instant)
	}
	if Unix() != instant.Unix() {
		t.Fatalf("Unix = %d", Unix())
	}
}

func TestRealKeepsAdvancingWhileFrozen(t *testing.T) {
	t.Cleanup(Unfreeze)
	Freeze(instant)
	start := Real()
	time.Sleep(10 * time.Millisecond)
	if time.Since(start) < 10*time.Millisecond {
		t.Fatal("Real did not advance")
	}
	if !Now().Equal(instant) {
		t.Fatal("Now moved")
	}
}

func TestFrozenV7IsValidDeterministicAndOrdered(t *testing.T) {
	t.Cleanup(Unfreeze)
	run := func() []string {
		Freeze(instant)
		var out []string
		for i := 0; i < 3; i++ {
			u, err := NewV7()
			if err != nil {
				t.Fatal(err)
			}
			if u.Version() != 7 || u.Variant().String() != "RFC4122" {
				t.Fatalf("%s: version %d variant %s", u, u.Version(), u.Variant())
			}
			out = append(out, u.String())
		}
		return out
	}
	a, b := run(), run()
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("run-dependent: %v vs %v", a, b)
		}
		if i > 0 && a[i] <= a[i-1] {
			t.Fatalf("not ordered: %v", a)
		}
	}
	if a[0] == a[1] {
		t.Fatal("not distinct")
	}
	// The embedded time is the frozen instant.
	Freeze(instant)
	u, _ := NewV7()
	sec, nsec := u.Time().UnixTime()
	if time.Unix(sec, nsec).UTC().UnixMilli() != instant.UnixMilli() {
		t.Fatalf("embedded time %v", time.Unix(sec, nsec))
	}
}

func TestUnfrozenV7IsTheLibraryOne(t *testing.T) {
	Unfreeze()
	a, _ := NewV7()
	b, _ := NewV7()
	if a == b || a.Version() != 7 {
		t.Fatalf("%s %s", a, b)
	}
}
