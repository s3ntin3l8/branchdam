package thumbs

import (
	"bytes"
	"context"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// requireFFmpeg skips the test with a clear message when ffmpeg isn't
// installed, rather than failing -- mirrors internal/probe's requireTool:
// neither exiftool nor ffprobe/ffmpeg is present on every machine that runs
// `go test ./...` (notably, the CI Go job doesn't install them), and this
// package must still build and its non-ffmpeg tests must still pass without
// it.
func requireFFmpeg(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed, skipping")
	}
}

// makeFixtureMP4 generates a tiny real MP4 via ffmpeg, mirroring
// internal/probe's own probe_test.go fixture helper.
func makeFixtureMP4(t *testing.T, dir string) string {
	t.Helper()
	requireFFmpeg(t)

	path := filepath.Join(dir, "fixture.mp4")
	cmd := exec.Command("ffmpeg", "-y",
		"-f", "lavfi", "-i", "testsrc=size=320x240:duration=1",
		"-c:v", "libx264", path)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture mp4: %v\n%s", err, out)
	}
	return path
}

func writeTestPNG(t *testing.T, path string, w, h int) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x % 256), G: uint8(y % 256), B: 200, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
	defer func() { _ = f.Close() }()
	if err := png.Encode(f, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
}

func TestScaleToMaxEdgeDownscalesLandscape(t *testing.T) {
	t.Parallel()
	img := image.NewRGBA(image.Rect(0, 0, 2000, 1000))
	scaled := scaleToMaxEdge(img, 500)
	b := scaled.Bounds()
	if b.Dx() != 500 {
		t.Errorf("width = %d, want 500", b.Dx())
	}
	if b.Dy() != 250 {
		t.Errorf("height = %d, want 250 (aspect ratio preserved)", b.Dy())
	}
}

func TestScaleToMaxEdgeDownscalesPortrait(t *testing.T) {
	t.Parallel()
	img := image.NewRGBA(image.Rect(0, 0, 1000, 2000))
	scaled := scaleToMaxEdge(img, 500)
	b := scaled.Bounds()
	if b.Dy() != 500 {
		t.Errorf("height = %d, want 500", b.Dy())
	}
	if b.Dx() != 250 {
		t.Errorf("width = %d, want 250 (aspect ratio preserved)", b.Dx())
	}
}

func TestScaleToMaxEdgeNeverUpscales(t *testing.T) {
	t.Parallel()
	img := image.NewRGBA(image.Rect(0, 0, 100, 80))
	scaled := scaleToMaxEdge(img, 500)
	b := scaled.Bounds()
	if b.Dx() != 100 || b.Dy() != 80 {
		t.Errorf("bounds = %dx%d, want unchanged 100x80", b.Dx(), b.Dy())
	}
}

func TestCachePathIsSharded(t *testing.T) {
	t.Parallel()
	c := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	uuid := "0198abcd-1234-7000-8000-000000000001"
	got := c.Path(uuid)
	want := filepath.Join(c.root, "01", "98", uuid+".jpg")
	if got != want {
		t.Errorf("Path(%q) = %q, want %q", uuid, got, want)
	}
}

func TestCachePathShortUUIDFallback(t *testing.T) {
	t.Parallel()
	c := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	got := c.Path("ab")
	want := filepath.Join(c.root, "short", "ab.jpg")
	if got != want {
		t.Errorf("Path(%q) = %q, want %q", "ab", got, want)
	}
}

func TestCacheWriteThenPathReadable(t *testing.T) {
	t.Parallel()
	c := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	uuid := "0198abcd-1234-7000-8000-000000000002"
	data := []byte("fake jpeg bytes")

	if err := c.Write(uuid, data); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, err := os.ReadFile(c.Path(uuid))
	if err != nil {
		t.Fatalf("read written thumbnail: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("read back %q, want %q", got, data)
	}

	// No leftover temp file in the shard directory.
	entries, err := os.ReadDir(filepath.Dir(c.Path(uuid)))
	if err != nil {
		t.Fatalf("read shard dir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("shard dir has %d entries, want exactly 1 (no leftover temp file)", len(entries))
	}
}

func TestCacheDeleteMissingIsNotError(t *testing.T) {
	t.Parallel()
	c := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	if err := c.Delete("0198abcd-1234-7000-8000-000000000003"); err != nil {
		t.Errorf("Delete on a never-written thumbnail returned an error: %v", err)
	}
}

func TestCacheDeleteThenPathGone(t *testing.T) {
	t.Parallel()
	c := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	uuid := "0198abcd-1234-7000-8000-000000000004"
	if err := c.Write(uuid, []byte("data")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := c.Delete(uuid); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := os.Stat(c.Path(uuid)); !os.IsNotExist(err) {
		t.Errorf("thumbnail still present after Delete, stat err = %v", err)
	}
}

func TestCacheGenerateDirectImage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "photo.png")
	writeTestPNG(t, path, 1200, 800)

	c := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 500)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	data, err := c.Generate(ctx, path)
	if err != nil {
		t.Fatalf("Generate: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("generated thumbnail is not a decodable JPEG: %v", err)
	}
	b := img.Bounds()
	if b.Dx() != 500 {
		t.Errorf("thumbnail width = %d, want 500", b.Dx())
	}
}

func TestCacheGenerateUnsupportedFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(path, []byte("not an image, no embedded preview tag either"), 0o644); err != nil {
		t.Fatalf("write text file: %v", err)
	}

	c := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.Generate(ctx, path)
	if err != ErrUnsupported {
		t.Errorf("Generate on a non-image file returned err = %v, want ErrUnsupported", err)
	}
}

func TestCacheGenerateMissingFile(t *testing.T) {
	t.Parallel()
	c := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	_, err := c.Generate(ctx, filepath.Join(t.TempDir(), "does-not-exist.jpg"))
	if err == nil {
		t.Fatal("Generate on a nonexistent file returned nil error")
	}
	if err == ErrUnsupported {
		t.Error("Generate on a nonexistent file returned ErrUnsupported, want a real I/O error -- OpenRead should fail before extraction is even attempted")
	}
}

// TestCacheGenerateVideoPoster is the acceptance-criteria test for #224: a
// video file -- neither natively decodable via image.Decode nor carrying an
// exiftool-extractable embedded preview -- gets a real READY-able JPEG
// thumbnail via the ffmpeg poster-frame fallback, instead of landing on
// ErrUnsupported the way it did before this fallback existed.
func TestCacheGenerateVideoPoster(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := makeFixtureMP4(t, dir)

	c := New(t.TempDir(), storage.NewGuard(nil), probe.New(), 500)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	data, err := c.Generate(ctx, path)
	if err != nil {
		t.Fatalf("Generate on a video file: %v", err)
	}
	img, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("generated video thumbnail is not a decodable JPEG: %v", err)
	}
	b := img.Bounds()
	// The fixture is 320x240 (landscape); maxEdgePx=500 exceeds the source's
	// longest edge, so scaleToMaxEdge must leave it unscaled.
	if b.Dx() != 320 || b.Dy() != 240 {
		t.Errorf("thumbnail dimensions = %dx%d, want 320x240 (source unscaled, maxEdgePx exceeds source)", b.Dx(), b.Dy())
	}
}
