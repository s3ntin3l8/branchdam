package probe

import (
	"errors"
	"regexp"
	"testing"
)

// tagAssignmentRe matches exiftool's -TAG=VALUE write syntax, e.g.
// "-EXIF:Make=SONY" or "-overwrite_original" style bare write flags.
var tagAssignmentRe = regexp.MustCompile(`^-[A-Za-z0-9:_-]+=`)

// adversarialPaths includes paths crafted to look like exiftool flags, to
// prove "--" actually stops them from being parsed as such rather than
// relying on exiftool happening to reject them.
var adversarialPaths = []string{
	"/normal/path/DSC001.ARW",
	"-overwrite_original",
	"-TAG=value.jpg",
	"-EXIF:Make=HACKED",
	"--exec=rm -rf /",
	"-j",
	"-a",
	"",
}

// TestExiftoolArgsNeverWrite is the load-bearing test for the storage.Guard
// invariant this package must uphold on its own: exiftool defaults to
// writing files in place, so the constructed argv must never contain a
// write flag or a tag assignment, for ANY path input -- including a path
// that is itself crafted to look like a flag. The "--" separator is what
// makes that safe; this test proves it holds across the whole allowlist.
func TestExiftoolArgsNeverWrite(t *testing.T) {
	for _, path := range adversarialPaths {
		args := exiftoolArgs(path)

		// Only the portion BEFORE "--" matters for this check: anything
		// after it (including a path deliberately crafted to look like a
		// flag, e.g. "-overwrite_original" as a *filename*) is safe by
		// construction -- exiftool treats it as a literal positional
		// argument, never as a flag. Flagging matches in that region would
		// fail on the very adversarial inputs this test exists to prove
		// are handled safely.
		sawSeparator := false
		for _, a := range args {
			if a == "--" {
				sawSeparator = true
				continue
			}
			if sawSeparator {
				continue
			}
			if a == "-overwrite_original" {
				t.Errorf("exiftoolArgs(%q) contains -overwrite_original before --: %v", path, args)
			}
			if tagAssignmentRe.MatchString(a) {
				t.Errorf("exiftoolArgs(%q) contains a tag assignment before --: %v", path, args)
			}
		}
		if !sawSeparator {
			t.Errorf("exiftoolArgs(%q) has no -- separator: %v", path, args)
		}
		// The path itself must be the last argument, after --, so it can
		// never be merged with or mistaken for a preceding flag.
		if args[len(args)-1] != path {
			t.Errorf("exiftoolArgs(%q) does not end with the path: %v", path, args)
		}
	}
}

func TestExiftoolWriteArgsAllowlist(t *testing.T) {
	t.Parallel()
	// Only allowlisted tags may be emitted.
	allowlistOK := map[string]string{
		"EXIF:Make":               "SONY",
		"EXIF:Model":              "ILCE-7M4",
		"EXIF:LensModel":          "FE 24-70mm F2.8 GM",
		"EXIF:SerialNumber":       "1234567",
		"EXIF:DateTimeOriginal":   "2026:07:15 14:30:00",
		"EXIF:OffsetTimeOriginal": "+02:00",
		"Composite:GPSLatitude":   "-33.9151",
		"Composite:GPSLongitude":  "18.4115",
		"XMP-dc:Identifier":       "uuid-abc",
		"XMP-xmpMM:DerivedFrom":   "uuid-parent",
	}
	args, err := exiftoolWriteArgs(allowlistOK, "/path/shot.jpg")
	if err != nil {
		t.Fatalf("exiftoolWriteArgs(allowlisted): %v", err)
	}
	sawOverwrite := false
	sawSep := false
	for _, a := range args {
		if a == "-overwrite_original" {
			sawOverwrite = true
			continue
		}
		if a == "--" {
			sawSep = true
			continue
		}
		if sawSep {
			continue // the path (and anything after --) is a literal positional arg
		}
		if !tagAssignmentRe.MatchString(a) {
			t.Errorf("write argv contains unexpected argument %q", a)
			continue
		}
		tag := a[1:indexOf(a, '=')]
		if !exiftoolWriteAllowlist[tag] {
			t.Errorf("write argv contains non-allowlisted tag %q", tag)
		}
	}
	if !sawOverwrite {
		t.Error("write argv missing -overwrite_original")
	}
	if !sawSep {
		t.Error("write argv missing -- separator")
	}
	if args[len(args)-1] != "/path/shot.jpg" {
		t.Errorf("write argv does not end with the path: %v", args)
	}

	// A disallowed tag is a hard error, not a silent drop.
	if _, err := exiftoolWriteArgs(map[string]string{"EXIF:MakerNotes": "hack"}, "/x.jpg"); !errors.Is(err, ErrTagNotAllowed) {
		t.Errorf("disallowed tag error = %v, want ErrTagNotAllowed", err)
	}
}

func indexOf(s string, r rune) int {
	for i, c := range s {
		if c == r {
			return i
		}
	}
	return -1
}

// TestSidecarPath is deliberately case-preserving: on a case-sensitive
// filesystem, a sidecar an editor actually wrote alongside "DSC01234.ARW"
// is named "DSC01234.xmp", never the lowercased "dsc01234.xmp" internal/
// naming.Stem would produce (naming.Stem exists for graph-identity
// comparisons, not on-disk lookups -- see sidecarPath's doc comment).
func TestSidecarPath(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"/media/DSC01234.ARW":        "/media/DSC01234.xmp",
		"/media/DSC01234.arw":        "/media/DSC01234.xmp",
		"/media/trip/IMG_0001.CR2":   "/media/trip/IMG_0001.xmp",
		"/media/no_extension_at_all": "/media/no_extension_at_all.xmp",
	}
	for path, want := range cases {
		if got := sidecarPath(path); got != want {
			t.Errorf("sidecarPath(%q) = %q, want %q", path, got, want)
		}
	}
}

// TestMergeSidecarRow proves the precedence match to exiftool's own
// -tagsFromFile default ("-all": every source tag overwrites the same-
// named destination tag), while sidecar bookkeeping about the sidecar file
// itself (File:*, ExifTool:*, SourceFile) never leaks into the RAW's row.
func TestMergeSidecarRow(t *testing.T) {
	t.Parallel()
	row := map[string]any{
		"EXIF:Make":              "SONY",
		"XMP:OriginalDocumentID": "raw-orig-id",
		"File:FileName":          "raw.arw",
		"File:FileSize":          float64(123),
	}
	sidecar := map[string]any{
		"SourceFile":               "raw.xmp",
		"File:FileName":            "raw.xmp",
		"ExifTool:ExifToolVersion": 12.76,
		"XMP:OriginalDocumentID":   "sidecar-orig-id",
		"XMP:Subject":              "sidecar-keyword",
	}

	mergeSidecarRow(row, sidecar)

	if row["XMP:OriginalDocumentID"] != "sidecar-orig-id" {
		t.Errorf("OriginalDocumentID = %v, want sidecar-orig-id (sidecar must win)", row["XMP:OriginalDocumentID"])
	}
	if row["XMP:Subject"] != "sidecar-keyword" {
		t.Errorf("XMP:Subject = %v, want sidecar-keyword (sidecar-only tag must be merged in)", row["XMP:Subject"])
	}
	if row["EXIF:Make"] != "SONY" {
		t.Errorf("EXIF:Make = %v, want SONY (RAW-only tag must survive the merge)", row["EXIF:Make"])
	}
	if row["File:FileName"] != "raw.arw" {
		t.Errorf("File:FileName = %v, want raw.arw (sidecar bookkeeping must not overwrite the RAW's own)", row["File:FileName"])
	}
	if _, ok := row["ExifTool:ExifToolVersion"]; ok {
		t.Error("ExifTool:ExifToolVersion leaked from sidecar bookkeeping into row")
	}
	if row["File:FileSize"] != float64(123) {
		t.Errorf("File:FileSize = %v, want 123 (RAW's own file bookkeeping must survive)", row["File:FileSize"])
	}
}

func TestFFProbeArgsPathAfterSeparator(t *testing.T) {
	t.Parallel()
	for _, path := range adversarialPaths {
		args := ffprobeArgs(path)
		sawSeparator := false
		for _, a := range args {
			if a == "--" {
				sawSeparator = true
			}
		}
		if !sawSeparator {
			t.Errorf("ffprobeArgs(%q) has no -- separator: %v", path, args)
		}
		if args[len(args)-1] != path {
			t.Errorf("ffprobeArgs(%q) does not end with the path: %v", path, args)
		}
	}
}

func TestPreviewImageArgsNeverWrite(t *testing.T) {
	t.Parallel()
	for _, fn := range []func(string) []string{previewImageArgs, jpgFromRawArgs, thumbnailImageArgs} {
		for _, path := range adversarialPaths {
			args := fn(path)
			sawSeparator := false
			for _, a := range args {
				if a == "--" {
					sawSeparator = true
					continue
				}
				if sawSeparator {
					continue
				}
				if a == "-overwrite_original" {
					t.Errorf("args(%q) contains -overwrite_original before --: %v", path, args)
				}
				if tagAssignmentRe.MatchString(a) {
					t.Errorf("args(%q) contains tag assignment before --: %v", path, args)
				}
			}
			if !sawSeparator {
				t.Errorf("args(%q) has no -- separator: %v", path, args)
			}
			if args[len(args)-1] != path {
				t.Errorf("args(%q) does not end with path: %v", path, args)
			}
		}
	}
}

// TestVideoPosterArgsPathAfterInputFlag proves path always lands directly
// after "-i" -- ffmpeg consumes the argument immediately following a flag
// like -i positionally, as that flag's own required value, never re-parsing
// it as a flag itself, so this is the ffmpeg-side analogue of exiftool's "--"
// separator proof above: even an adversarial path can't be misread as an
// option.
func TestVideoPosterArgsPathAfterInputFlag(t *testing.T) {
	t.Parallel()
	for _, seek := range videoPosterSeekOffsets {
		for _, path := range adversarialPaths {
			args := videoPosterArgs(path, seek)

			idx := -1
			for i, a := range args {
				if a == "-i" {
					idx = i
					break
				}
			}
			if idx == -1 {
				t.Fatalf("videoPosterArgs(%q, %q) has no -i flag: %v", path, seek, args)
			}
			if idx+1 >= len(args) || args[idx+1] != path {
				t.Errorf("videoPosterArgs(%q, %q) does not place path immediately after -i: %v", path, seek, args)
			}
		}
	}
}

// TestVideoPosterArgsNeverInteractive proves the constructed argv can never
// block waiting on a TTY prompt or stdin: -y (auto-confirm any overwrite
// prompt, even though there is no output file to overwrite here) and
// -nostdin (never read from stdin) must always be present, since this
// argv's stdout is itself the JPEG payload -- there is no terminal attached
// to answer a prompt in the worker's actual runtime.
func TestVideoPosterArgsNeverInteractive(t *testing.T) {
	t.Parallel()
	for _, seek := range videoPosterSeekOffsets {
		args := videoPosterArgs("/some/video.mp4", seek)
		var sawY, sawNostdin bool
		for _, a := range args {
			switch a {
			case "-y":
				sawY = true
			case "-nostdin":
				sawNostdin = true
			}
		}
		if !sawY {
			t.Errorf("videoPosterArgs(seek=%q) missing -y: %v", seek, args)
		}
		if !sawNostdin {
			t.Errorf("videoPosterArgs(seek=%q) missing -nostdin: %v", seek, args)
		}
	}
}

// TestVideoPosterArgsOutputsToStdout proves the argv writes its single-frame
// JPEG to stdout (pipe:1) rather than a file on disk -- ExtractVideoPoster
// reads cmd.Stdout directly, so a change here that redirected output to a
// file would silently make every call return empty bytes.
func TestVideoPosterArgsOutputsToStdout(t *testing.T) {
	t.Parallel()
	args := videoPosterArgs("/some/video.mov", "1")
	if got := args[len(args)-1]; got != "pipe:1" {
		t.Errorf("videoPosterArgs last arg = %q, want pipe:1", got)
	}
}

// TestVideoPosterSeekOffsetsFallsBackToFirstFrame proves the seek schedule
// tries a non-zero offset (to skip a likely black/fade-in opening frame)
// before falling back to "0" -- the one offset guaranteed to exist in any
// video with at least one frame, however short.
func TestVideoPosterSeekOffsetsFallsBackToFirstFrame(t *testing.T) {
	t.Parallel()
	if len(videoPosterSeekOffsets) == 0 {
		t.Fatal("videoPosterSeekOffsets is empty, want at least one offset")
	}
	last := videoPosterSeekOffsets[len(videoPosterSeekOffsets)-1]
	if last != "0" {
		t.Errorf("last videoPosterSeekOffsets entry = %q, want \"0\" (guaranteed fallback)", last)
	}
}

func FuzzArgvConstructors(f *testing.F) {
	for _, path := range adversarialPaths {
		f.Add(path)
	}
	f.Fuzz(func(t *testing.T, path string) {
		for _, fn := range []func(string) []string{exiftoolArgs, ffprobeArgs, previewImageArgs, jpgFromRawArgs, thumbnailImageArgs} {
			args := fn(path)
			if len(args) == 0 {
				t.Errorf("empty args for path %q", path)
			}
			if args[len(args)-1] != path {
				t.Errorf("args for path %q does not end with path: %v", path, args)
			}
			sawSep := false
			for _, a := range args {
				if a == "--" {
					sawSep = true
					break
				}
			}
			if !sawSep {
				t.Errorf("args for path %q missing -- separator: %v", path, args)
			}
		}
	})
}
