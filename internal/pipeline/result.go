// Package pipeline turns indexer.Records and computed hashes into
// media_nodes/media_edges rows. It owns the ONLY write access to those
// tables outside migrations: internal/graph (edge resolution) and
// internal/httpapi both go through Commit, never sqlcgen directly.
package pipeline

import (
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/probe"
)

// Result is one file's fully-computed index data, ready to commit. It is
// produced by a worker job (indexer.Walk/Watch -> workers.Pool -> hashing +
// probe), entirely OUTSIDE any database transaction -- Commit only reads and
// writes rows given an already-resolved Result, so the single writer
// connection is never held during file I/O or subprocess execution.
type Result struct {
	Path     string
	FileName string
	FileExt  string
	Size     int64
	ModTime  time.Time

	FastHash string // always populated -- xxHash64, 16 hex chars
	FullHash string // empty means "not computed for this pass" (see docs/schema.md fix #8's policy)
	PHash    *int64

	// Promoted EXIF fields -- exactly the set the schema has columns for
	// (docs/schema.md's media_nodes). CameraMake is deliberately absent:
	// only camera_model is a promoted column (it's what the Tier-2
	// "same camera" resolver signal uses); Make lives in node_metadata
	// overflow once that's wired up, not here. Empty/nil when probe.Exif
	// wasn't run or found nothing -- Commit never requires these to be
	// present.
	OriginalDocumentID string
	DocumentID         string
	DerivedFromID      string
	CapturedAt         *time.Time
	CameraModel        string

	// New in #33. Typed fields mirror probe.ExifResult; persisted as
	// node_metadata rows with source='exiftool' (keyed by the same grouped tag
	// names exiftool reports), alongside a bounded allowlist of Raw tags.
	// CameraModel is not repeated here -- it's the promoted camera_model column.
	Make         string
	LensModel    string
	SerialNumber string
	GPSLatitude  *float64
	GPSLongitude *float64

	// ExifRaw is the bounded, explicit allowlist subset of probe.ExifResult.Raw
	// the scan decided to persist. Empty means "nothing persisted". The full
	// Raw map is deliberately never dumped -- see exifRawAllowlist.
	ExifRaw map[string]string

	// FFProbe holds the parsed video-stream probe for this file; nil when the
	// file isn't a video, ffprobe is absent, or probing failed. Persisted as
	// node_metadata rows with source='ffprobe'. RawJSON is deliberately not
	// persisted (#34).
	FFProbe *probe.FFProbeResult
}

// Stats summarizes what a Commit call actually did, for scan_jobs progress
// reporting and test assertions.
type Stats struct {
	Inserted                 int // brand new nodes
	Touched                  int // same content at the same path, no new row
	VersionCollisions        int // docs/schema.md fix #3: old archived, new inserted
	Moved                    int // Pillar 5: MISSING node's path rebased
	EdgesCreated             int // graph.Engine.ResolveAndCommit's newly-created (not merely refreshed) edges
	MetadataWritten          int // #105: node_metadata rows actually upserted on touched/rebased nodes (0 when unchanged)
	PromotedColumnsRefreshed int // #197: nodes whose promoted columns were refreshed on a touched/rebased pass (0 when nothing differed)
}

// exifRawAllowlist is the closed set of exiftool tag names persisted into
// node_metadata. Everything else exiftool reports is discarded -- an explicit
// allowlist, not the full map, so tag sets we never query are never written.
var exifRawAllowlist = map[string]bool{
	"EXIF:ExposureTime":            true,
	"EXIF:FNumber":                 true,
	"EXIF:ISO":                     true,
	"EXIF:FocalLength":             true,
	"EXIF:FocalLengthIn35mmFormat": true,
	"EXIF:WhiteBalance":            true,
	"EXIF:ColorSpace":              true,
	"EXIF:Orientation":             true,
	"EXIF:Software":                true,
	"EXIF:Artist":                  true,
	"EXIF:Copyright":               true,
	"EXIF:ImageDescription":        true,
	"EXIF:DateTimeOriginal":        true, // persisted so #54 can inherit the parent's raw capture time + offset
	"EXIF:OffsetTimeOriginal":      true,
	"XMP:Rating":                   true,
	"XMP:Label":                    true,
	"XMP:Subject":                  true,
}

func selectExifRaw(raw map[string]string) map[string]string {
	out := make(map[string]string)
	for k, v := range raw {
		if exifRawAllowlist[k] {
			out[k] = v
		}
	}
	return out
}

// videoExts is the closed set of extensions FFProbe runs for (#34). Kept
// explicit so adding a format is a conscious one-line change. ".ts" here
// means MPEG transport stream, not TypeScript source.
var videoExts = map[string]bool{
	"mp4": true, "mov": true, "m4v": true, "mkv": true, "avi": true,
	"webm": true, "wmv": true, "mts": true, "m2ts": true, "ts": true,
	"3gp": true, "flv": true, "mpg": true, "mpeg": true,
}

func isVideoExt(ext string) bool { return videoExts[strings.ToLower(strings.TrimPrefix(ext, "."))] }

// imageExts is the set of image and camera RAW extensions for which pHash is attempted.
var imageExts = map[string]bool{
	"jpg": true, "jpeg": true, "png": true, "gif": true, "webp": true,
	"tif": true, "tiff": true, "bmp": true, "heic": true, "heif": true,
	"arw": true, "cr2": true, "cr3": true, "nef": true, "dng": true,
	"raf": true, "rw2": true, "orf": true, "pef": true, "srw": true,
}

func isImageExt(ext string) bool { return imageExts[strings.ToLower(strings.TrimPrefix(ext, "."))] }
