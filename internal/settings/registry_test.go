package settings

import (
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
)

// TestFieldsWellFormed pins the registry's own internal contract: every
// entry has a unique Key, a working Get/Set round-trip against a
// representative value for its Kind, and a non-editable field always
// carries a ReadOnlyReason (otherwise the settings API would have nothing
// to show the operator for why the field is greyed out).
func TestFieldsWellFormed(t *testing.T) {
	seen := make(map[string]bool)
	for _, f := range Fields() {
		if f.Key == "" {
			t.Fatal("field with empty Key")
		}
		if seen[f.Key] {
			t.Errorf("duplicate field key %q", f.Key)
		}
		seen[f.Key] = true

		if !f.Editable && f.ReadOnlyReason == "" {
			t.Errorf("field %q is not editable but has no ReadOnlyReason", f.Key)
		}
		if f.Editable && f.Apply == ApplyNever {
			t.Errorf("field %q is editable but ApplyNever -- an editable field must be Live or Restart", f.Key)
		}

		var sample any
		switch f.Type {
		case KindString:
			sample = "x"
		case KindInt:
			sample = 1
		case KindBool:
			sample = true
		case KindStringList:
			sample = []string{"x"}
		case KindPathRewriteList:
			sample = []config.PathRewrite{{From: "x", To: "y"}}
		default:
			t.Fatalf("field %q has unhandled Kind %v", f.Key, f.Type)
		}

		cfg := &config.Config{}
		if err := f.Set(cfg, sample); err != nil {
			t.Errorf("field %q: Set(%v) = %v, want nil", f.Key, sample, err)
			continue
		}
		got := f.Get(cfg)
		if !equalAny(got, sample) {
			t.Errorf("field %q: Get() after Set(%v) = %v, want the same value round-tripped", f.Key, sample, got)
		}
	}
}

func equalAny(a, b any) bool {
	as, aok := a.([]string)
	bs, bok := b.([]string)
	if aok || bok {
		if !aok || !bok || len(as) != len(bs) {
			return false
		}
		for i := range as {
			if as[i] != bs[i] {
				return false
			}
		}
		return true
	}
	ar, arok := a.([]config.PathRewrite)
	br, brok := b.([]config.PathRewrite)
	if arok || brok {
		if !arok || !brok || len(ar) != len(br) {
			return false
		}
		for i := range ar {
			if ar[i] != br[i] {
				return false
			}
		}
		return true
	}
	return a == b
}

func TestLookupFindsEveryRegisteredField(t *testing.T) {
	for _, f := range Fields() {
		got, ok := Lookup(f.Key)
		if !ok {
			t.Errorf("Lookup(%q) not found", f.Key)
			continue
		}
		if got.Key != f.Key {
			t.Errorf("Lookup(%q).Key = %q", f.Key, got.Key)
		}
	}
	if _, ok := Lookup("not.a.real.field"); ok {
		t.Error("Lookup(unregistered) = ok, want not found")
	}
}
