package probe

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// requireTool skips the test with a clear message when the named binary
// isn't installed, rather than failing -- neither exiftool nor ffprobe/
// ffmpeg is present on every machine that runs `go test ./...` (notably,
// the CI Go job doesn't install them), matching the graceful-degradation
// requirement this whole package exists to satisfy.
func requireTool(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not installed, skipping (this package must still build and its pure-function tests must still pass without it)", name)
	}
	return path
}

// makeFixtureJPEG generates a tiny real JPEG via ffmpeg and tags it with
// exiftool, so this test exercises the actual subprocess path end to end
// rather than a canned fixture file checked into the repo.
func makeFixtureJPEG(t *testing.T, dir string) string {
	t.Helper()
	requireTool(t, "ffmpeg")
	exiftoolPath := requireTool(t, "exiftool")

	path := filepath.Join(dir, "fixture.jpg")
	ffmpeg := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=blue:s=64x64", "-frames:v", "1", path)
	if out, err := ffmpeg.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture jpeg: %v\n%s", err, out)
	}

	tagArgs := []string{
		"-overwrite_original",
		"-EXIF:Make=SONY", "-EXIF:Model=ILCE-7M4", "-EXIF:LensModel=FE 24-70mm F2.8 GM",
		"-EXIF:SerialNumber=1234567",
		"-EXIF:DateTimeOriginal=2026:07:15 14:30:00", "-EXIF:OffsetTimeOriginal=+02:00",
		"-GPSLatitude=33.9", "-GPSLatitudeRef=S", "-GPSLongitude=18.4", "-GPSLongitudeRef=W",
		"-XMP-xmpMM:DocumentID=doc-abc-123", "-XMP-xmpMM:OriginalDocumentID=orig-doc-xyz",
		path,
	}
	tagCmd := exec.Command(exiftoolPath, tagArgs...)
	if out, err := tagCmd.CombinedOutput(); err != nil {
		t.Fatalf("tag fixture jpeg: %v\n%s", err, out)
	}
	return path
}

func makeFixtureMP4(t *testing.T, dir string) string {
	t.Helper()
	requireTool(t, "ffmpeg")

	path := filepath.Join(dir, "fixture.mp4")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:duration=1",
		"-f", "lavfi", "-i", "sine=frequency=1000:duration=1",
		"-c:v", "libx264", "-c:a", "aac", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture mp4: %v\n%s", err, out)
	}
	return path
}

func TestExifRealFile(t *testing.T) {
	requireTool(t, "exiftool")
	path := makeFixtureJPEG(t, t.TempDir())

	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := p.Exif(ctx, path)
	if err != nil {
		t.Fatalf("Exif: %v", err)
	}

	if result.Make != "SONY" || result.Model != "ILCE-7M4" {
		t.Errorf("Make/Model = %q/%q, want SONY/ILCE-7M4", result.Make, result.Model)
	}
	if result.LensModel != "FE 24-70mm F2.8 GM" {
		t.Errorf("LensModel = %q, want FE 24-70mm F2.8 GM", result.LensModel)
	}
	if result.OriginalDocumentID != "orig-doc-xyz" {
		t.Errorf("OriginalDocumentID = %q, want orig-doc-xyz", result.OriginalDocumentID)
	}
	if result.DocumentID != "doc-abc-123" {
		t.Errorf("DocumentID = %q, want doc-abc-123", result.DocumentID)
	}

	// The load-bearing assertion: GPS must come from the sign-corrected
	// Composite group. S/W hemispheres, so both must be NEGATIVE -- the raw
	// EXIF:GPSLatitude/Longitude tags stay positive (33.9, 18.4) regardless
	// of hemisphere, which is exactly the bug using the wrong group would
	// reproduce.
	if result.GPSLatitude == nil || *result.GPSLatitude >= 0 {
		t.Errorf("GPSLatitude = %v, want a negative value (S hemisphere)", result.GPSLatitude)
	}
	if result.GPSLongitude == nil || *result.GPSLongitude >= 0 {
		t.Errorf("GPSLongitude = %v, want a negative value (W hemisphere)", result.GPSLongitude)
	}

	if result.CapturedAt == nil {
		t.Fatal("CapturedAt is nil, want a parsed timestamp")
	}
	if result.CapturedAt.Year() != 2026 || result.CapturedAt.Month() != time.July || result.CapturedAt.Day() != 15 {
		t.Errorf("CapturedAt = %v, want 2026-07-15", result.CapturedAt)
	}
	if _, offset := result.CapturedAt.Zone(); offset != 2*3600 {
		t.Errorf("CapturedAt offset = %ds, want +02:00 (7200s)", offset)
	}

	if len(result.Raw) == 0 {
		t.Error("Raw is empty, want the full flattened exiftool output for node_metadata overflow")
	}
}

// TestExifSidecarMerge is issue #227's acceptance criterion: a RAW (here, a
// JPEG fixture standing in for one) with a sibling .xmp sidecar has the
// sidecar's metadata reflected in the probed Result -- specifically,
// XMP:OriginalDocumentID from the sidecar, which is what internal/graph's
// Tier-2 XMPOriginalDocumentIDResolver reads via media_nodes.
// original_document_id. The sidecar overwrites OriginalDocumentID (present
// in both), while Make/Model (RAW-only, the sidecar never touches them)
// prove this is a genuine merge, not a full replace.
func TestExifSidecarMerge(t *testing.T) {
	exiftoolPath := requireTool(t, "exiftool")
	dir := t.TempDir()
	rawPath := makeFixtureJPEG(t, dir)

	sidecarFile := rawPath[:len(rawPath)-len(filepath.Ext(rawPath))] + ".xmp"
	tagCmd := exec.Command(exiftoolPath,
		"-XMP-xmpMM:OriginalDocumentID=sidecar-orig-999",
		"-XMP-dc:Subject+=sidecar-keyword",
		sidecarFile,
	)
	if out, err := tagCmd.CombinedOutput(); err != nil {
		t.Fatalf("write sidecar xmp: %v\n%s", err, out)
	}

	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := p.Exif(ctx, rawPath)
	if err != nil {
		t.Fatalf("Exif: %v", err)
	}

	// The sidecar's OriginalDocumentID must win over the RAW's own
	// embedded value ("orig-doc-xyz", set by makeFixtureJPEG).
	if result.OriginalDocumentID != "sidecar-orig-999" {
		t.Errorf("OriginalDocumentID = %q, want sidecar-orig-999 (the sidecar's value)", result.OriginalDocumentID)
	}
	// A tag only the sidecar carries must show up in Raw too.
	if result.Raw["XMP:Subject"] != "sidecar-keyword" {
		t.Errorf("Raw[XMP:Subject] = %q, want sidecar-keyword (raw=%v)", result.Raw["XMP:Subject"], result.Raw)
	}
	// RAW-only fields the sidecar never mentions must survive the merge
	// untouched -- this is what makes it a merge and not a replace.
	if result.Make != "SONY" || result.Model != "ILCE-7M4" {
		t.Errorf("Make/Model = %q/%q, want SONY/ILCE-7M4 (sidecar merge must not clobber RAW-only fields)", result.Make, result.Model)
	}
	// Sidecar bookkeeping (its own File:*/SourceFile) must never leak into
	// the RAW's result row.
	if result.Raw["File:FileName"] != filepath.Base(rawPath) {
		t.Errorf("Raw[File:FileName] = %q, want the RAW's own filename %q (sidecar bookkeeping must not overwrite it)", result.Raw["File:FileName"], filepath.Base(rawPath))
	}
}

// TestExifNoSidecarUnaffected proves the common case (no .xmp next to the
// RAW) behaves exactly as before -- no second exiftool invocation changes
// its behavior or exit code just because sidecarPath's stat check ran.
func TestExifNoSidecarUnaffected(t *testing.T) {
	requireTool(t, "exiftool")
	path := makeFixtureJPEG(t, t.TempDir())

	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := p.Exif(ctx, path)
	if err != nil {
		t.Fatalf("Exif: %v", err)
	}
	if result.OriginalDocumentID != "orig-doc-xyz" {
		t.Errorf("OriginalDocumentID = %q, want orig-doc-xyz (no sidecar present, RAW's own value)", result.OriginalDocumentID)
	}
}

// TestExifOnSidecarItselfDoesNotSelfMerge covers gap 2's leftover surface:
// registering ".xmp" as its own internal/projectfile extension (so
// indexer.Walk stops handing sidecars to Exif at all) is explicitly out of
// scope for this PR, so a bare .xmp can still reach Exif directly today.
// sidecarPath(".../x.xmp") is a fixed point ("x.xmp" again) -- without the
// readSidecarRow guard, Exif would spawn a second exiftool against the same
// file and merge its own row into itself on every call. This proves Exif on
// an .xmp still succeeds and returns exactly the sidecar's own values, with
// no doubled subprocess spawn.
func TestExifOnSidecarItselfDoesNotSelfMerge(t *testing.T) {
	exiftoolPath := requireTool(t, "exiftool")
	dir := t.TempDir()
	sidecarFile := filepath.Join(dir, "standalone.xmp")
	tagCmd := exec.Command(exiftoolPath,
		"-XMP-xmpMM:OriginalDocumentID=standalone-orig-id",
		sidecarFile,
	)
	if out, err := tagCmd.CombinedOutput(); err != nil {
		t.Fatalf("write standalone xmp: %v\n%s", err, out)
	}

	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := p.Exif(ctx, sidecarFile)
	if err != nil {
		t.Fatalf("Exif: %v", err)
	}
	if result.OriginalDocumentID != "standalone-orig-id" {
		t.Errorf("OriginalDocumentID = %q, want standalone-orig-id", result.OriginalDocumentID)
	}
}

func TestFFProbeRealFile(t *testing.T) {
	requireTool(t, "ffprobe")
	path := makeFixtureMP4(t, t.TempDir())

	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	result, err := p.FFProbe(ctx, path)
	if err != nil {
		t.Fatalf("FFProbe: %v", err)
	}

	if result.VideoCodec != "h264" {
		t.Errorf("VideoCodec = %q, want h264", result.VideoCodec)
	}
	if result.AudioCodec != "aac" {
		t.Errorf("AudioCodec = %q, want aac", result.AudioCodec)
	}
	if result.Width != 320 || result.Height != 240 {
		t.Errorf("dimensions = %dx%d, want 320x240", result.Width, result.Height)
	}
	if result.DurationSeconds == nil || *result.DurationSeconds < 0.9 || *result.DurationSeconds > 1.1 {
		t.Errorf("DurationSeconds = %v, want ~1.0", result.DurationSeconds)
	}
	if result.SizeBytes == nil || *result.SizeBytes <= 0 {
		t.Errorf("SizeBytes = %v, want a positive value", result.SizeBytes)
	}
	if result.RawJSON == "" {
		t.Error("RawJSON is empty, want the full ffprobe output for node_metadata overflow")
	}
}

// TestToolUnavailableIsGraceful proves the core contract this package
// exists to satisfy (spec directive 9.4's "fall back gracefully"): a Prober
// constructed against binaries that don't exist returns a typed,
// recognizable error rather than panicking or hanging.
func TestToolUnavailableIsGraceful(t *testing.T) {
	p := &Prober{} // deliberately empty -- as if New() found neither tool

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	if _, err := p.Exif(ctx, "/nonexistent"); err == nil {
		t.Error("Exif with no exiftool resolved succeeded, want ErrToolUnavailable")
	} else if !errors.Is(err, ErrToolUnavailable) {
		t.Errorf("Exif error = %v, want it to wrap ErrToolUnavailable", err)
	}

	if _, err := p.FFProbe(ctx, "/nonexistent"); err == nil {
		t.Error("FFProbe with no ffprobe resolved succeeded, want ErrToolUnavailable")
	} else if !errors.Is(err, ErrToolUnavailable) {
		t.Errorf("FFProbe error = %v, want it to wrap ErrToolUnavailable", err)
	}
}

func TestNewDetectsAbsentTools(t *testing.T) {
	// Regardless of what's actually installed on this machine, New() must
	// not panic and must produce a Prober whose HasX() methods are
	// consistent with what it resolved.
	p := New()
	if p.HasExiftool() {
		if _, err := exec.LookPath("exiftool"); err != nil {
			t.Error("HasExiftool() = true but exiftool is not actually on PATH")
		}
	}
	if p.HasFFProbe() {
		if _, err := exec.LookPath("ffprobe"); err != nil {
			t.Error("HasFFProbe() = true but ffprobe is not actually on PATH")
		}
	}
}

func TestExifMissingFilePropagatesError(t *testing.T) {
	requireTool(t, "exiftool")
	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	_, err := p.Exif(ctx, filepath.Join(t.TempDir(), "does-not-exist.jpg"))
	if err == nil {
		t.Fatal("Exif on a nonexistent file succeeded, want an error")
	}
}

func TestExifRespectsContextTimeout(t *testing.T) {
	requireTool(t, "exiftool")
	path := makeFixtureJPEG(t, t.TempDir())
	p := New()

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // ensure the deadline has actually passed

	_, err := p.Exif(ctx, path)
	if err == nil {
		t.Fatal("Exif with an already-expired context succeeded, want an error")
	}
}

func TestExtractPHashDirectImage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.png")

	img := image.NewRGBA(image.Rect(0, 0, 100, 100))
	for x := 0; x < 100; x++ {
		for y := 0; y < 100; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 255, A: 255})
		}
	}

	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create test.png: %v", err)
	}
	if err := png.Encode(f, img); err != nil {
		_ = f.Close()
		t.Fatalf("encode test.png: %v", err)
	}
	_ = f.Close()

	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	phash, err := p.ExtractPHash(ctx, path)
	if err != nil {
		t.Fatalf("ExtractPHash: %v", err)
	}
	if phash == nil {
		t.Fatal("ExtractPHash returned nil phash for a valid PNG image")
	}
}

func TestExtractPHashNonImageFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "test.txt")
	if err := os.WriteFile(path, []byte("hello world text file"), 0644); err != nil {
		t.Fatalf("write text file: %v", err)
	}

	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	phash, err := p.ExtractPHash(ctx, path)
	if err != nil {
		t.Fatalf("ExtractPHash on non-image file returned error: %v", err)
	}
	if phash != nil {
		t.Errorf("ExtractPHash on text file returned %v, want nil", phash)
	}
}

func TestExtractPreviewJPEGNoExiftool(t *testing.T) {
	t.Parallel()
	p := &Prober{} // zero value: exiftoolPath unset, HasExiftool() false regardless of the host

	preview, err := p.ExtractPreviewJPEG(context.Background(), "/nonexistent/path.arw")
	if err != nil {
		t.Fatalf("ExtractPreviewJPEG with no exiftool returned error: %v", err)
	}
	if preview != nil {
		t.Errorf("ExtractPreviewJPEG with no exiftool returned %d bytes, want nil", len(preview))
	}
}

func TestExtractPreviewJPEGNoEmbeddedPreview(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := makeFixtureJPEG(t, dir) // plain JPEG -- no PreviewImage/JpgFromRaw/ThumbnailImage tag

	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	preview, err := p.ExtractPreviewJPEG(ctx, path)
	if err != nil {
		t.Fatalf("ExtractPreviewJPEG on a JPEG with no embedded preview tag returned error: %v", err)
	}
	if preview != nil {
		t.Errorf("ExtractPreviewJPEG on a JPEG with no embedded preview tag returned %d bytes, want nil", len(preview))
	}
}

func TestWriteTagsRoundTrip(t *testing.T) {
	exiftool := requireTool(t, "exiftool")
	requireTool(t, "ffmpeg")
	prober := &Prober{exiftoolPath: exiftool}

	dir := t.TempDir()
	fixture := filepath.Join(dir, "fixture.jpg")
	out, err := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=blue:s=64x64", "-frames:v", "1", fixture).CombinedOutput()
	if err != nil {
		t.Fatalf("generate fixture: %v\n%s", err, out)
	}

	tags := map[string]string{
		"EXIF:Make":              "SONY",
		"Composite:GPSLatitude":  "-33.9151",
		"Composite:GPSLongitude": "18.4115",
		"XMP-dc:Identifier":      "uuid-child",
	}
	if err := prober.WriteTags(context.Background(), fixture, tags); err != nil {
		t.Fatalf("WriteTags: %v", err)
	}

	res, err := prober.Exif(context.Background(), fixture)
	if err != nil {
		t.Fatalf("Exif: %v", err)
	}
	if res.Make != "SONY" {
		t.Errorf("EXIF:Make = %q, want SONY", res.Make)
	}
	// GPS is the most format-sensitive write (signed decimal degrees): it must
	// survive a write via EXIF:GPS* and read back via the signed Composite
	// group (not flipped to the wrong hemisphere by a lost ref).
	if res.GPSLatitude == nil || *res.GPSLatitude != -33.9151 {
		t.Errorf("GPSLatitude = %v, want -33.9151", res.GPSLatitude)
	}
	if res.GPSLongitude == nil || *res.GPSLongitude != 18.4115 {
		t.Errorf("GPSLongitude = %v, want 18.4115", res.GPSLongitude)
	}
	// exiftool reports the dc:identifier field (written via -XMP-dc:Identifier)
	// under the generic XMP group when reading back with -G.
	if res.Raw["XMP:Identifier"] != "uuid-child" {
		t.Errorf("XMP:Identifier = %q, want uuid-child (raw=%v)", res.Raw["XMP:Identifier"], res.Raw)
	}
}

func TestExtractVideoPosterRealFile(t *testing.T) {
	requireTool(t, "ffmpeg")
	path := makeFixtureMP4(t, t.TempDir())

	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	poster, err := p.ExtractVideoPoster(ctx, path)
	if err != nil {
		t.Fatalf("ExtractVideoPoster: %v", err)
	}
	if len(poster) == 0 {
		t.Fatal("ExtractVideoPoster returned no bytes for a valid mp4 fixture")
	}
	img, _, err := image.Decode(bytes.NewReader(poster))
	if err != nil {
		t.Fatalf("ExtractVideoPoster returned bytes that don't decode as an image: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 320 || b.Dy() != 240 {
		t.Errorf("poster frame dimensions = %dx%d, want 320x240 (matching the fixture)", b.Dx(), b.Dy())
	}
}

// TestExtractVideoPosterRespectsContextTimeout is the regression test for
// the terminal-UNSUPPORTED-on-cancellation bug: an already-expired ctx must
// come back as a real error (so internal/thumbs.Cache.Generate's caller
// retries via FAILED), never as nil, nil (which maps to the terminal,
// never-retried UNSUPPORTED thumb_state) -- mirrors
// TestExifRespectsContextTimeout's shape for the exiftool path.
func TestExtractVideoPosterRespectsContextTimeout(t *testing.T) {
	requireTool(t, "ffmpeg")
	path := makeFixtureMP4(t, t.TempDir())
	p := New()

	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond) // ensure the deadline has actually passed

	poster, err := p.ExtractVideoPoster(ctx, path)
	if err == nil {
		t.Fatal("ExtractVideoPoster with an already-expired context returned nil error, want a real error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("ExtractVideoPoster error = %v, want it to wrap context.DeadlineExceeded", err)
	}
	if poster != nil {
		t.Errorf("ExtractVideoPoster with an expired context returned %d bytes, want nil", len(poster))
	}
}

func TestExtractVideoPosterNoFFmpeg(t *testing.T) {
	t.Parallel()
	p := &Prober{} // zero value: ffmpegPath unset, HasFFmpeg() false regardless of the host

	poster, err := p.ExtractVideoPoster(context.Background(), "/nonexistent/video.mp4")
	if err != nil {
		t.Fatalf("ExtractVideoPoster with no ffmpeg returned error: %v", err)
	}
	if poster != nil {
		t.Errorf("ExtractVideoPoster with no ffmpeg returned %d bytes, want nil", len(poster))
	}
}

func TestExtractVideoPosterNonVideoFile(t *testing.T) {
	requireTool(t, "ffmpeg")
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("not a video file"), 0o644); err != nil {
		t.Fatalf("write text file: %v", err)
	}

	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	poster, err := p.ExtractVideoPoster(ctx, path)
	if err != nil {
		t.Fatalf("ExtractVideoPoster on a non-video file returned error: %v", err)
	}
	if poster != nil {
		t.Errorf("ExtractVideoPoster on a non-video file returned %d bytes, want nil", len(poster))
	}
}

func TestExtractVideoPosterMissingFile(t *testing.T) {
	requireTool(t, "ffmpeg")
	p := New()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	poster, err := p.ExtractVideoPoster(ctx, filepath.Join(t.TempDir(), "does-not-exist.mp4"))
	if err != nil {
		t.Fatalf("ExtractVideoPoster on a nonexistent file returned error: %v", err)
	}
	if poster != nil {
		t.Errorf("ExtractVideoPoster on a nonexistent file returned %d bytes, want nil", len(poster))
	}
}

func TestMain(m *testing.M) {
	// Fail fast with a clear signal if TMPDIR itself is unwritable, rather
	// than every fixture-generating test failing with a confusing ffmpeg
	// error.
	f, err := os.CreateTemp("", "probe-smoke-*")
	if err != nil {
		panic("probe package tests require a writable temp dir: " + err.Error())
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)

	os.Exit(m.Run())
}
