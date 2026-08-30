package settings

import (
	"strings"
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

func TestKindAndApplyModeString(t *testing.T) {
	kinds := []struct {
		k    Kind
		want string
	}{
		{KindString, "string"},
		{KindInt, "int"},
		{KindBool, "bool"},
		{KindStringList, "stringList"},
		{KindPathRewriteList, "pathRewriteList"},
		{Kind(999), "unknown"},
	}
	for _, tc := range kinds {
		if got := tc.k.String(); got != tc.want {
			t.Errorf("Kind(%d).String() = %q, want %q", tc.k, got, tc.want)
		}
	}

	modes := []struct {
		m    ApplyMode
		want string
	}{
		{ApplyLive, "live"},
		{ApplyRestart, "restart"},
		{ApplyNever, "never"},
		{ApplyMode(999), "unknown"},
	}
	for _, tc := range modes {
		if got := tc.m.String(); got != tc.want {
			t.Errorf("ApplyMode(%d).String() = %q, want %q", tc.m, got, tc.want)
		}
	}
}

func TestTrustedProxiesFieldValidation(t *testing.T) {
	field, ok := Lookup("http.trustedProxies")
	if !ok {
		t.Fatal("http.trustedProxies not registered")
	}

	valid := [][]string{
		nil,
		{},
		{"*"},
		{"10.0.0.1"},
		{"10.0.0.0/24"},
		{"2001:db8::1"},
		{"2001:db8::/32"},
		{" 10.0.0.1 "},
	}
	for _, entries := range valid {
		if err := field.Validate(entries); err != nil {
			t.Errorf("Validate(%v) = %v, want nil", entries, err)
		}
	}

	invalid := []struct {
		name    string
		value   any
		wantErr string
	}{
		{"wrong type", "10.0.0.1", "must be a string list"},
		{"empty entry", []string{""}, "proxy entry cannot be empty"},
		{"invalid IP", []string{"not-an-ip"}, "invalid IP address"},
		{"invalid CIDR", []string{"10.0.0.0/not-a-prefix"}, "invalid CIDR"},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			err := field.Validate(tc.value)
			if err == nil {
				t.Fatal("Validate = nil, want error")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("Validate error = %q, want it to contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestTrustedProxiesFieldSetRejectsWrongType(t *testing.T) {
	field, ok := Lookup("http.trustedProxies")
	if !ok {
		t.Fatal("http.trustedProxies not registered")
	}

	cfg := &config.Config{}
	if err := field.Set(cfg, "10.0.0.1"); err == nil {
		t.Fatal("Set with wrong type = nil, want error")
	}
}
