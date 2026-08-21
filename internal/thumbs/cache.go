// Package thumbs generates and caches JPEG thumbnails for media assets.
//
// The cache lives under a configured directory on the app's own /data
// volume, not inside any storage_locations tier, and deliberately does not
// route its writes through storage.Guard -- Guard.Resolve returns
// ErrUnknownLocation for any path outside storage_locations, so routing
// cache writes through it isn't merely unnecessary, it's impossible. The
// cache directory is app state, exactly like branchdam.db, and is the one
// sanctioned os.* write path outside Guard for that reason -- see
// CLAUDE.md's Key invariants. Reading the SOURCE media a thumbnail is
// generated from still goes through Guard.OpenRead, since that's a
// storage-location path.
package thumbs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	stddraw "image/draw"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"os"
	"path/filepath"

	xdraw "golang.org/x/image/draw"

	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// ErrUnsupported is returned by Generate when path is neither a natively
// decodable image (Go's stdlib image package) nor a file exiftool can
// extract a decodable embedded preview from. Distinct from a transient or
// I/O failure -- a caller should mark the node permanently UNSUPPORTED on
// this error, not retry it on an unbounded schedule.
var ErrUnsupported = errors.New("thumbs: no decodable image or embedded preview available")

// DefaultMaxEdgePx is the longest-edge target used when Cache is
// constructed with maxEdgePx <= 0.
const DefaultMaxEdgePx = 512

// jpegQuality is the fixed encode quality for generated thumbnails. Not
// configurable: a 512px-longest-edge JPEG is small at any reasonable
// quality, and a single fixed value keeps every cached thumbnail's
// encoding predictable.
const jpegQuality = 80

// Cache generates and stores JPEG thumbnails for media files under a root
// directory on disk, sharded by node UUID so no single directory holds an
// unbounded number of files.
type Cache struct {
	root      string
	guard     *storage.Guard
	prober    *probe.Prober
	maxEdgePx int
}

// New returns a Cache rooted at root, reading source media through guard
// and falling back to RAW preview extraction via prober. maxEdgePx <= 0
// uses DefaultMaxEdgePx.
func New(root string, guard *storage.Guard, prober *probe.Prober, maxEdgePx int) *Cache {
	if maxEdgePx <= 0 {
		maxEdgePx = DefaultMaxEdgePx
	}
	return &Cache{root: root, guard: guard, prober: prober, maxEdgePx: maxEdgePx}
}

// Path returns the on-disk path a thumbnail for nodeUUID lives at, whether
// or not it currently exists.
func (c *Cache) Path(nodeUUID string) string {
	return filepath.Join(c.shardDir(nodeUUID), nodeUUID+".jpg")
}

// shardDir returns the two-level shard directory for nodeUUID
// (<uuid[0:2]>/<uuid[2:4]>), keeping any one directory small. node_uuid is
// always a 36-character UUIDv7 in practice; the short-uuid fallback below
// only exists so a malformed value degrades to a shared flat directory
// instead of panicking on a slice out of range.
func (c *Cache) shardDir(nodeUUID string) string {
	if len(nodeUUID) < 4 {
		return filepath.Join(c.root, "short")
	}
	return filepath.Join(c.root, nodeUUID[0:2], nodeUUID[2:4])
}

// Generate produces a thumbnail JPEG for the media file at filePath,
// trying direct decoding (via guard.OpenRead) first and falling back to
// prober.ExtractPreviewJPEG for formats Go's stdlib image package can't
// handle -- camera RAW. Returns ErrUnsupported, not a generic error, when
// the source opened fine but neither decoding path yields a decodable
// image; a source that couldn't be opened at all (missing, permission
// denied) is a real error, distinct from ErrUnsupported, since there was
// nothing to even attempt extraction against.
func (c *Cache) Generate(ctx context.Context, filePath string) ([]byte, error) {
	f, err := c.guard.OpenRead(filePath)
	if err != nil {
		return nil, fmt.Errorf("thumbs: open source: %w", err)
	}
	img, _, decodeErr := image.Decode(f)
	_ = f.Close()

	if decodeErr != nil {
		preview, pErr := c.prober.ExtractPreviewJPEG(ctx, filePath)
		if pErr != nil {
			return nil, fmt.Errorf("thumbs: extract preview: %w", pErr)
		}
		if len(preview) == 0 {
			return nil, ErrUnsupported
		}
		img, _, err = image.Decode(bytes.NewReader(preview))
		if err != nil {
			// ExtractPreviewJPEG already validates decodability itself, so
			// this branch is not expected to be reachable in practice --
			// treated as unsupported rather than a hard error since there
			// is no further fallback available.
			return nil, ErrUnsupported
		}
	}

	scaled := scaleToMaxEdge(img, c.maxEdgePx)

	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, scaled, &jpeg.Options{Quality: jpegQuality}); err != nil {
		return nil, fmt.Errorf("thumbs: encode jpeg: %w", err)
	}
	return buf.Bytes(), nil
}

// scaleToMaxEdge returns img scaled so its longest edge is maxEdgePx,
// preserving aspect ratio. Returns img unchanged (never upscales) if it is
// already at or under maxEdgePx on both axes.
func scaleToMaxEdge(img image.Image, maxEdgePx int) image.Image {
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 || (w <= maxEdgePx && h <= maxEdgePx) {
		return img
	}

	var newW, newH int
	if w >= h {
		newW = maxEdgePx
		newH = int(float64(h) * float64(maxEdgePx) / float64(w))
	} else {
		newH = maxEdgePx
		newW = int(float64(w) * float64(maxEdgePx) / float64(h))
	}
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	xdraw.CatmullRom.Scale(dst, dst.Bounds(), img, b, stddraw.Over, nil)
	return dst
}

// Write atomically stores data as the thumbnail for nodeUUID: written to a
// temp file in the destination shard directory, then renamed into place,
// so a concurrent reader (Path) never observes a partially-written file.
func (c *Cache) Write(nodeUUID string, data []byte) error {
	dir := c.shardDir(nodeUUID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("thumbs: mkdir shard dir: %w", err)
	}

	tmp, err := os.CreateTemp(dir, nodeUUID+".*.tmp")
	if err != nil {
		return fmt.Errorf("thumbs: create temp file: %w", err)
	}
	tmpPath := tmp.Name()
	renamed := false
	defer func() {
		if !renamed {
			_ = os.Remove(tmpPath)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("thumbs: write temp file: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("thumbs: close temp file: %w", err)
	}
	if err := os.Rename(tmpPath, c.Path(nodeUUID)); err != nil {
		return fmt.Errorf("thumbs: rename into place: %w", err)
	}
	renamed = true
	return nil
}

// Delete removes nodeUUID's cached thumbnail, if any. A missing file is
// not an error: Delete's contract is "the thumbnail is gone after this
// call," which already holds if it never existed.
func (c *Cache) Delete(nodeUUID string) error {
	if err := os.Remove(c.Path(nodeUUID)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("thumbs: delete: %w", err)
	}
	return nil
}
