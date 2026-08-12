// Package probe wraps exiftool and ffprobe subprocess execution for EXIF/XMP
// and video-stream metadata extraction (spec directive 9.4). Every
// subprocess call goes through storage.Guard's OpenRead first at the caller
// (internal/pipeline) -- this package only shells out with a fixed argv
// allowlist and never asks either tool to modify the file it's reading.
//
// Neither tool is installed on every machine that runs `go test ./...`
// (notably: the CI Go job does not install them). Prober resolves both
// binaries once at construction; Exif and FFProbe return ErrToolUnavailable
// when the corresponding binary is missing, and callers are expected to
// fall back to fast_hash-only indexing per spec directive 9.4 -- this
// package never fails loudly just because a machine lacks the tools.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"time"
)

// ErrToolUnavailable is returned by Exif or FFProbe when the underlying
// binary was not found on PATH at Prober construction time.
var ErrToolUnavailable = errors.New("probe: required external tool is not installed")

// Prober resolves exiftool and ffprobe's paths once and reuses them.
type Prober struct {
	exiftoolPath string
	ffprobePath  string
}

// New resolves exiftool and ffprobe via exec.LookPath. A missing tool is not
// an error here -- it only becomes one if a caller actually tries to use it,
// so a machine with only one of the two tools installed can still use the
// other.
func New() *Prober {
	p := &Prober{}
	if path, err := exec.LookPath("exiftool"); err == nil {
		p.exiftoolPath = path
	}
	if path, err := exec.LookPath("ffprobe"); err == nil {
		p.ffprobePath = path
	}
	return p
}

// HasExiftool reports whether exiftool was found at construction time.
func (p *Prober) HasExiftool() bool { return p.exiftoolPath != "" }

// HasFFProbe reports whether ffprobe was found at construction time.
func (p *Prober) HasFFProbe() bool { return p.ffprobePath != "" }

// exiftoolArgs builds exiftool's argv from a fixed allowlist: JSON output
// (-j), grouped tag names (-G) so e.g. "EXIF:Make" and "XMP:Identifier"
// don't collide, numeric values (-n) so GPS coordinates and similar come
// back as JSON numbers, and "--" before path so a filename that happens to
// start with "-" can never be parsed as a flag. This is a pure function
// specifically so TestExiftoolArgsNeverWrite can assert -- across arbitrary
// path inputs -- that no constructed argv ever contains a write flag
// (-overwrite_original) or a tag-assignment (-TAG=value), independent of
// whatever exec.CommandContext does with the result.
func exiftoolArgs(path string) []string {
	return []string{"-j", "-n", "-G", "--", path}
}

// ffprobeArgs mirrors exiftoolArgs' path-injection protection even though
// ffprobe has no write mode of its own to guard against.
func ffprobeArgs(path string) []string {
	return []string{"-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", "--", path}
}

// ExifResult holds the fields spec Pillar 4 inherits from parent to child
// (DateTimeOriginal/OffsetTimeOriginal preserved as CapturedAt, GPS,
// Make/Model/LensModel/SerialNumber) plus the graph-relevant XMP identity
// tags, with everything else exiftool reported available in Raw for
// node_metadata overflow (source='exiftool').
type ExifResult struct {
	OriginalDocumentID string // XMP:OriginalDocumentID
	DocumentID         string // XMP:DocumentID
	DerivedFromID      string // XMP:DerivedFrom

	CapturedAt   *time.Time // from Composite:SubSecDateTimeOriginal, falling back to EXIF:DateTimeOriginal(+OffsetTimeOriginal)
	Make         string
	Model        string
	LensModel    string
	SerialNumber string

	// GPSLatitude/GPSLongitude come from the Composite group, NOT the raw
	// EXIF group: EXIF:GPSLatitude/GPSLongitude are unsigned magnitudes --
	// verified against a real exiftool build, a file tagged
	// GPSLatitudeRef=S keeps EXIF:GPSLatitude positive while
	// Composite:GPSLatitude is the correctly hemisphere-signed value. Using
	// the EXIF group directly would silently put southern/western
	// coordinates in the wrong hemisphere.
	GPSLatitude  *float64
	GPSLongitude *float64

	Raw map[string]string
}

// Exif runs exiftool against path and parses its JSON output. ctx controls
// both the subprocess's lifetime (via exec.CommandContext) and any timeout
// -- callers should set a deadline (spec directive 9.4 implies a bounded
// per-file budget); this package does not impose one of its own.
func (p *Prober) Exif(ctx context.Context, path string) (*ExifResult, error) {
	if !p.HasExiftool() {
		return nil, fmt.Errorf("%w: exiftool", ErrToolUnavailable)
	}

	cmd := exec.CommandContext(ctx, p.exiftoolPath, exiftoolArgs(path)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("probe: exiftool %s: %w (stderr: %s)", path, err, stderr.String())
	}

	var rows []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil {
		return nil, fmt.Errorf("probe: parse exiftool output for %s: %w", path, err)
	}
	if len(rows) == 0 {
		return nil, fmt.Errorf("probe: exiftool returned no result for %s", path)
	}
	row := rows[0]

	result := &ExifResult{
		OriginalDocumentID: valString(row, "XMP:OriginalDocumentID"),
		DocumentID:         valString(row, "XMP:DocumentID"),
		DerivedFromID:      valString(row, "XMP:DerivedFrom"),
		Make:               valString(row, "EXIF:Make"),
		Model:              valString(row, "EXIF:Model"),
		LensModel:          valString(row, "EXIF:LensModel"),
		SerialNumber:       valString(row, "EXIF:SerialNumber"),
		GPSLatitude:        valFloatPtr(row, "Composite:GPSLatitude"),
		GPSLongitude:       valFloatPtr(row, "Composite:GPSLongitude"),
		Raw:                flattenToStrings(row),
	}
	result.CapturedAt = capturedAt(row)

	return result, nil
}

// exifTimeLayout matches exiftool's "YYYY:MM:DD HH:MM:SS[+-]HH:MM" format.
// Go's time layout is token-positional (2006/01/02/etc.), so the literal
// colons and space here are matched literally against the input -- this is
// not a coincidence of exiftool's format resembling Go's reference time.
const exifTimeLayout = "2006:01:02 15:04:05-07:00"

// exifTimeLayoutNoOffset is used when only EXIF:DateTimeOriginal is present
// with no offset at all (parsed as UTC, since that's Go's default for an
// unzoned layout -- callers should treat this case as lower-confidence than
// an offset-bearing timestamp).
const exifTimeLayoutNoOffset = "2006:01:02 15:04:05"

func capturedAt(row map[string]any) *time.Time {
	// Composite:SubSecDateTimeOriginal already combines DateTimeOriginal and
	// OffsetTimeOriginal (and sub-seconds, when present) into one string --
	// prefer it over manually combining the two EXIF fields.
	if s := valString(row, "Composite:SubSecDateTimeOriginal"); s != "" {
		if t, err := time.Parse(exifTimeLayout, s); err == nil {
			return &t
		}
	}

	dto := valString(row, "EXIF:DateTimeOriginal")
	if dto == "" {
		return nil
	}
	if offset := valString(row, "EXIF:OffsetTimeOriginal"); offset != "" {
		if t, err := time.Parse(exifTimeLayout, dto+offset); err == nil {
			return &t
		}
	}
	if t, err := time.Parse(exifTimeLayoutNoOffset, dto); err == nil {
		return &t
	}
	return nil
}

func valString(row map[string]any, key string) string {
	v, ok := row[key]
	if !ok || v == nil {
		return ""
	}
	switch t := v.(type) {
	case string:
		return t
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return fmt.Sprint(t)
	}
}

func valFloatPtr(row map[string]any, key string) *float64 {
	v, ok := row[key]
	if !ok {
		return nil
	}
	f, ok := v.(float64)
	if !ok {
		return nil
	}
	return &f
}

func flattenToStrings(row map[string]any) map[string]string {
	out := make(map[string]string, len(row))
	for k, v := range row {
		out[k] = fmt.Sprint(v)
	}
	return out
}

// FFProbeResult holds the video/audio stream shape spec directive 9.4
// extracts. ffprobe's JSON schema is stable and well-documented (unlike
// exiftool's dynamic per-file tag set), so this is parsed onto typed Go
// structs rather than a generic map; RawJSON keeps the full output for
// node_metadata overflow (source='ffprobe') without needing to model every
// field ffprobe can report.
type FFProbeResult struct {
	FormatName      string
	DurationSeconds *float64
	SizeBytes       *int64
	VideoCodec      string
	Width, Height   int
	AudioCodec      string
	RawJSON         string
}

type ffprobeOutput struct {
	Streams []struct {
		CodecName string `json:"codec_name"`
		CodecType string `json:"codec_type"`
		Width     int    `json:"width"`
		Height    int    `json:"height"`
	} `json:"streams"`
	Format struct {
		FormatName string `json:"format_name"`
		Duration   string `json:"duration"`
		Size       string `json:"size"`
	} `json:"format"`
}

// FFProbe runs ffprobe against path and parses its JSON output. Same ctx
// contract as Exif.
func (p *Prober) FFProbe(ctx context.Context, path string) (*FFProbeResult, error) {
	if !p.HasFFProbe() {
		return nil, fmt.Errorf("%w: ffprobe", ErrToolUnavailable)
	}

	cmd := exec.CommandContext(ctx, p.ffprobePath, ffprobeArgs(path)...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("probe: ffprobe %s: %w (stderr: %s)", path, err, stderr.String())
	}

	var parsed ffprobeOutput
	if err := json.Unmarshal(stdout.Bytes(), &parsed); err != nil {
		return nil, fmt.Errorf("probe: parse ffprobe output for %s: %w", path, err)
	}

	result := &FFProbeResult{
		FormatName: parsed.Format.FormatName,
		RawJSON:    stdout.String(),
	}
	if parsed.Format.Duration != "" {
		if d, err := strconv.ParseFloat(parsed.Format.Duration, 64); err == nil {
			result.DurationSeconds = &d
		}
	}
	if parsed.Format.Size != "" {
		if sz, err := strconv.ParseInt(parsed.Format.Size, 10, 64); err == nil {
			result.SizeBytes = &sz
		}
	}
	for _, s := range parsed.Streams {
		switch s.CodecType {
		case "video":
			if result.VideoCodec == "" {
				result.VideoCodec = s.CodecName
				result.Width = s.Width
				result.Height = s.Height
			}
		case "audio":
			if result.AudioCodec == "" {
				result.AudioCodec = s.CodecName
			}
		}
	}

	return result, nil
}
