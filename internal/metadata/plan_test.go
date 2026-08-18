package metadata

import (
	"reflect"
	"testing"
)

func fptr(f float64) *float64 { return &f }

func TestPlan(t *testing.T) {
	parent := TagSet{
		DateTimeOriginal: "2023:05:01 12:34:56", OffsetTimeOriginal: "+02:00",
		GPSLatitude: fptr(-33.9151), GPSLongitude: fptr(18.4115),
		Make: "SONY", Model: "ILCE-7M4", LensModel: "FE 24-70mm F2.8 GM", SerialNumber: "1234567",
		Identifier: "uuid-parent", DerivedFrom: "uuid-grandparent",
	}
	// A child that HAS a parent: the endpoint pre-fills DerivedFrom with the
	// parent's node_uuid, and Identifier with the child's own uuid.
	childWithParent := TagSet{Identifier: "uuid-child", DerivedFrom: "uuid-parent"}
	// A child with NO parent: DerivedFrom is empty (nothing to inject it from).
	childNoParent := TagSet{Identifier: "uuid-child"}

	t.Run("full inheritance into an empty child", func(t *testing.T) {
		got, err := Plan(parent, childWithParent)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		want := map[string]string{
			"EXIF:DateTimeOriginal":   "2023:05:01 12:34:56",
			"EXIF:OffsetTimeOriginal": "+02:00",
			"Composite:GPSLatitude":   "-33.9151",
			"Composite:GPSLongitude":  "18.4115",
			"EXIF:Make":               "SONY",
			"EXIF:Model":              "ILCE-7M4",
			"EXIF:LensModel":          "FE 24-70mm F2.8 GM",
			"EXIF:SerialNumber":       "1234567",
			"XMP-dc:Identifier":       "uuid-child",
			"XMP-xmpMM:DerivedFrom":   "uuid-parent",
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("Plan = %v, want %v", got, want)
		}
	})

	t.Run("partial parent inherits only what it has", func(t *testing.T) {
		partial := TagSet{Make: "SONY", Model: "ILCE-7M4", Identifier: "uuid-parent", DerivedFrom: "gp"}
		got, err := Plan(partial, childWithParent)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if _, ok := got["EXIF:Make"]; !ok {
			t.Errorf("expected EXIF:Make to be inherited, got %v", got)
		}
		if _, ok := got["EXIF:DateTimeOriginal"]; ok {
			t.Errorf("did not expect EXIF:DateTimeOriginal (absent on parent), got %v", got)
		}
		if got["XMP-xmpMM:DerivedFrom"] != "uuid-parent" {
			t.Errorf("DerivedFrom = %q, want parent's uuid", got["XMP-xmpMM:DerivedFrom"])
		}
	})

	t.Run("child already populated is never overwritten", func(t *testing.T) {
		child := TagSet{
			DateTimeOriginal: "2019:01:01 00:00:00", OffsetTimeOriginal: "-05:00",
			GPSLatitude: fptr(40.0), GPSLongitude: fptr(-74.0),
			Make: "Canon", Model: "EOS R5", LensModel: "RF 50mm", SerialNumber: "998877",
			Identifier: "uuid-child", DerivedFrom: "uuid-parent",
		}
		got, err := Plan(parent, child)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		for _, tag := range []string{"EXIF:DateTimeOriginal", "EXIF:OffsetTimeOriginal", "Composite:GPSLatitude", "Composite:GPSLongitude", "EXIF:Make", "EXIF:Model", "EXIF:LensModel", "EXIF:SerialNumber"} {
			if _, ok := got[tag]; ok {
				t.Errorf("tag %q should NOT be overwritten, got %v", tag, got)
			}
		}
		// Identity tags are still (re)written.
		if got["XMP-dc:Identifier"] != "uuid-child" {
			t.Errorf("Identifier = %q, want child's uuid", got["XMP-dc:Identifier"])
		}
		if got["XMP-xmpMM:DerivedFrom"] != "uuid-parent" {
			t.Errorf("DerivedFrom = %q, want parent's uuid", got["XMP-xmpMM:DerivedFrom"])
		}
	})

	t.Run("offset is not inherited alone onto a child with its own timestamp", func(t *testing.T) {
		child := TagSet{
			DateTimeOriginal: "2019:01:01 00:00:00", // own time, no offset
			Identifier:       "uuid-child", DerivedFrom: "uuid-parent",
		}
		got, err := Plan(parent, child)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if _, ok := got["EXIF:DateTimeOriginal"]; ok {
			t.Errorf("child's own DateTimeOriginal must not be overwritten, got %v", got)
		}
		if _, ok := got["EXIF:OffsetTimeOriginal"]; ok {
			t.Errorf("parent's OffsetTimeOriginal must not be written without DateTimeOriginal (misinterprets the child's time), got %v", got)
		}
	})

	t.Run("missing parent writes only injected tags", func(t *testing.T) {
		noParent := TagSet{Identifier: "uuid-parent", DerivedFrom: ""}
		got, err := Plan(noParent, childNoParent)
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(got) != 1 || got["XMP-dc:Identifier"] != "uuid-child" {
			t.Errorf("Plan = %v, want only XMP-dc:Identifier", got)
		}
	})

	t.Run("both empty yields empty map", func(t *testing.T) {
		got, err := Plan(TagSet{}, TagSet{})
		if err != nil {
			t.Fatalf("Plan: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("Plan = %v, want empty", got)
		}
	})
}
