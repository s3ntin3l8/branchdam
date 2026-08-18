// Package metadata decides which EXIF/XMP tags to write into a child asset
// so it carries its parent's identity metadata (spec Pillar 4). Pure logic --
// no filesystem, no database, no exiftool. The endpoint in internal/httpapi
// assembles TagSets from the DB and hands the result to probe.WriteTags.
package metadata

import "strconv"

// TagSet holds the tag values Plan reasons about for one node. Empty fields
// mean "not present" -- Plan never invents a value for an absent field.
type TagSet struct {
	DateTimeOriginal   string   // EXIF:DateTimeOriginal as exiftool reports it, e.g. "2023:05:01 12:34:56"
	OffsetTimeOriginal string   // EXIF:OffsetTimeOriginal, e.g. "+02:00"
	GPSLatitude        *float64 // signed decimal degrees (Composite:GPSLatitude)
	GPSLongitude       *float64
	Make               string
	Model              string
	LensModel          string
	SerialNumber       string
	Identifier         string // XMP-dc:Identifier -- always the node's OWN node_uuid
	DerivedFrom        string // XMP-xmpMM:DerivedFrom -- the parent's node_uuid
}

// Plan decides which exiftool tags to write into the child's file (spec
// Pillar 4). Rules:
//   - Inherit each of DateTimeOriginal / OffsetTimeOriginal / GPS / Make /
//     Model / LensModel / SerialNumber from parent ONLY when the child has no
//     non-empty value of its own -- an export that already carries the tag is
//     never overwritten.
//   - XMP-dc:Identifier is always the child's node_uuid (identity).
//   - XMP-xmpMM:DerivedFrom is always the parent's node_uuid.
//   - A tag is only emitted when its source value is non-empty.
//
// Returns the tag->value map ready for exiftool's -TAG=value syntax. Returns
// an empty map when there is nothing to write.
func Plan(parent, child TagSet) (map[string]string, error) {
	tags := map[string]string{}

	inherit := func(tag, parentVal, childVal string) {
		if parentVal != "" && childVal == "" {
			tags[tag] = parentVal
		}
	}
	// DateTimeOriginal + OffsetTimeOriginal are a coupled pair: the offset is
	// only meaningful alongside the timestamp it modifies. Inherit them
	// together, and never write the parent's offset alone onto a child that
	// already carries its own DateTimeOriginal -- that would silently
	// reinterpret the child's capture time in the parent's timezone.
	if parent.DateTimeOriginal != "" && child.DateTimeOriginal == "" {
		tags["EXIF:DateTimeOriginal"] = parent.DateTimeOriginal
		if parent.OffsetTimeOriginal != "" && child.OffsetTimeOriginal == "" {
			tags["EXIF:OffsetTimeOriginal"] = parent.OffsetTimeOriginal
		}
	}
	inherit("EXIF:Make", parent.Make, child.Make)
	inherit("EXIF:Model", parent.Model, child.Model)
	inherit("EXIF:LensModel", parent.LensModel, child.LensModel)
	inherit("EXIF:SerialNumber", parent.SerialNumber, child.SerialNumber)

	if parent.GPSLatitude != nil && child.GPSLatitude == nil {
		// Composite:GPS* is signed decimal degrees; writing it (not the raw
		// EXIF:GPS* magnitude) is what makes exiftool emit the correct
		// GPSLatitudeRef/GPSLongitudeRef hemisphere and preserve the sign.
		tags["Composite:GPSLatitude"] = formatFloat(*parent.GPSLatitude)
	}
	if parent.GPSLongitude != nil && child.GPSLongitude == nil {
		tags["Composite:GPSLongitude"] = formatFloat(*parent.GPSLongitude)
	}

	if child.Identifier != "" {
		tags["XMP-dc:Identifier"] = child.Identifier
	}
	if child.DerivedFrom != "" {
		tags["XMP-xmpMM:DerivedFrom"] = child.DerivedFrom
	}

	return tags, nil
}

func formatFloat(f float64) string {
	return strconv.FormatFloat(f, 'f', -1, 64)
}
