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
//
// Exif additionally merges in a standalone XMP sidecar's tags when one sits
// next to path (issue #227, gap 1: editors like Luminar write
// non-destructive edits to a sidecar rather than embedding them in the RAW
// itself). The sidecar path is derived deterministically from path alone
// (same directory, same stem, ".xmp" extension -- see sidecarPath) and read
// through the exact same fixed, never-writes argv as the RAW, so the
// allowlist property holds for both invocations. Registering ".xmp" as its
// own internal/projectfile extension so it stops surfacing as an orphan
// media_nodes row is handled via internal/projectfile.XMPParser and
// ProjectSidecarResolver (#249).
//
// ffmpeg (as opposed to ffprobe) was historically deliberately excluded --
// see the Dockerfile's ffprobe stage comment, prior to #224. ExtractVideoPoster
// revisits that: internal/thumbs needs a real decoded frame, not just stream
// metadata, to give video assets a non-UNSUPPORTED thumbnail, and ffprobe has
// no frame-decoding capability of its own. This is strictly additive --
// FFProbe's stream-metadata extraction is untouched -- and mirrors the same
// resolve-once/ErrToolUnavailable/fixed-argv shape as exiftool and ffprobe
// above, never invoking ffmpeg in any mode that could modify the source file.
package probe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/hashing"
)

// ErrToolUnavailable is returned by Exif or FFProbe when the underlying
// binary was not found on PATH at Prober construction time.
var ErrToolUnavailable = errors.New("probe: required external tool is not installed")

// Prober resolves exiftool, ffprobe, and ffmpeg's paths once and reuses
// them.
type Prober struct {
	exiftoolPath string
	ffprobePath  string
	ffmpegPath   string
}

// New resolves exiftool, ffprobe, and ffmpeg via exec.LookPath. A missing
// tool is not an error here -- it only becomes one if a caller actually
// tries to use it, so a machine with only some of the three tools installed
// can still use the others.
func New() *Prober {
	p := &Prober{}
	if path, err := exec.LookPath("exiftool"); err == nil {
		p.exiftoolPath = path
	}
	if path, err := exec.LookPath("ffprobe"); err == nil {
		p.ffprobePath = path
	}
	if path, err := exec.LookPath("ffmpeg"); err == nil {
		p.ffmpegPath = path
	}
	return p
}

// HasExiftool reports whether exiftool was found at construction time.
func (p *Prober) HasExiftool() bool { return p.exiftoolPath != "" }

// HasFFProbe reports whether ffprobe was found at construction time.
func (p *Prober) HasFFProbe() bool { return p.ffprobePath != "" }

// HasFFmpeg reports whether ffmpeg was found at construction time.
func (p *Prober) HasFFmpeg() bool { return p.ffmpegPath != "" }

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

// sidecarExt is the fixed extension of a standalone XMP sidecar file (issue
// #227, gap 1). Editors that write non-destructive edits to a sidecar
// rather than embedding them in the RAW (Luminar among them) always use
// this extension.
const sidecarExt = ".xmp"

// sidecarPath returns the deterministic path of path's possible XMP
// sidecar: same directory, same stem (everything before path's own last
// extension, case preserved), ".xmp" extension. Deliberately NOT built
// from internal/naming.Stem -- that normalizes case and strips duplicate/
// role suffixes for graph-identity comparisons, which would silently break
// this on-disk lookup on a case-sensitive filesystem (a RAW named
// "DSC01234.ARW" would look for "dsc01234.xmp" instead of the sidecar an
// editor actually wrote, "DSC01234.xmp"). The result is derived only from
// path itself -- never operator-supplied or accepted from external input --
// which is what keeps this consistent with exiftoolArgs' fixed-argv
// allowlist property: the sidecar path fed to exiftoolArgs can never be
// attacker-controlled independent of the RAW's own already-trusted path.
func sidecarPath(path string) string {
	return strings.TrimSuffix(path, filepath.Ext(path)) + sidecarExt
}

// sidecarBookkeepingPrefixes are exiftool's own per-file plumbing groups --
// file size/mtime/permissions and exiftool's own version banner -- which
// describe the sidecar file itself, never something a sidecar merge should
// overwrite on the RAW's own result row.
var sidecarBookkeepingPrefixes = []string{"File:", "ExifTool:"}

// mergeSidecarRow overlays sidecar's tags onto row, mutating row in place.
// This mirrors exiftool's own -tagsFromFile precedence when no explicit tag
// list is given: "the same as specifying -all" -- every tag from the
// source (here, the sidecar) is copied into the destination's same-named
// tag, overwriting whatever was already there. row plays the role of the
// -tagsFromFile destination (the RAW's own read), sidecar plays the role of
// the source; sidecar values win for every tag present in both, and
// RAW-only tags (e.g. camera-embedded EXIF the sidecar never touches) are
// left untouched. Per-file bookkeeping groups (File:*, ExifTool:*) and the
// "SourceFile" key are never merged -- those describe the sidecar file
// itself, not metadata about the asset.
func mergeSidecarRow(row, sidecar map[string]any) {
	for k, v := range sidecar {
		if k == "SourceFile" {
			continue
		}
		bookkeeping := false
		for _, prefix := range sidecarBookkeepingPrefixes {
			if strings.HasPrefix(k, prefix) {
				bookkeeping = true
				break
			}
		}
		if bookkeeping {
			continue
		}
		row[k] = v
	}
}

// exiftoolWriteAllowlist is the closed set of tags the write path may emit.
// It bounds what exiftool can do when invoked for metadata inheritance --
// see TestExiftoolWriteArgsAllowlist. It is deliberately separate from the
// read path's persistence allowlist (exifRawAllowlist in internal/pipeline).
var exiftoolWriteAllowlist = map[string]bool{
	"EXIF:DateTimeOriginal":   true,
	"EXIF:OffsetTimeOriginal": true,
	"Composite:GPSLatitude":   true, // signed decimal degrees -> exiftool derives value + hemisphere ref
	"Composite:GPSLongitude":  true,
	"EXIF:Make":               true,
	"EXIF:Model":              true,
	"EXIF:LensModel":          true,
	"EXIF:SerialNumber":       true,
	"XMP-dc:Identifier":       true,
	"XMP-xmpMM:DerivedFrom":   true,
}

// ErrTagNotAllowed is returned by exiftoolWriteArgs when a caller asks to
// write a tag outside exiftoolWriteAllowlist. Surfaced as a hard error (not a
// silent drop) because a tag outside the allowlist is a programming error.
var ErrTagNotAllowed = errors.New("probe: tag not on exiftool write allowlist")

// exiftoolWriteArgs builds exiftool's argv for an in-place metadata write:
// -overwrite_original -TAG=value ... -- path. Deliberately SEPARATE from
// exiftoolArgs (the read path), so the read path's prove-never-writes
// property is never contaminated -- TestExiftoolArgsNeverWrite must keep
// passing unchanged. Tags are sorted for a deterministic argv, and every tag
// is validated against exiftoolWriteAllowlist before it is emitted.
func exiftoolWriteArgs(tags map[string]string, path string) ([]string, error) {
	keys := make([]string, 0, len(tags))
	for k := range tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	args := []string{"-overwrite_original"}
	for _, k := range keys {
		if !exiftoolWriteAllowlist[k] {
			return nil, fmt.Errorf("%w: %s", ErrTagNotAllowed, k)
		}
		args = append(args, "-"+k+"="+tags[k])
	}
	args = append(args, "--", path)
	return args, nil
}

// WriteTags writes tags into the file at path in place, via exiftool. The
// caller is responsible for storage.Guard.CheckWrite BEFORE calling this --
// this method only shells out and never resolves storage tiers. Returns
// ErrToolUnavailable when exiftool is absent.
func (p *Prober) WriteTags(ctx context.Context, path string, tags map[string]string) error {
	if !p.HasExiftool() {
		return fmt.Errorf("%w: exiftool", ErrToolUnavailable)
	}
	args, err := exiftoolWriteArgs(tags, path)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, p.exiftoolPath, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("probe: exiftool write %s: %w (stderr: %s)", path, err, stderr.String())
	}
	return nil
}

// ffprobeArgs mirrors exiftoolArgs' path-injection protection even though
// ffprobe has no write mode of its own to guard against.
func ffprobeArgs(path string) []string {
	return []string{"-v", "quiet", "-print_format", "json", "-show_format", "-show_streams", "--", path}
}

func previewImageArgs(path string) []string {
	return []string{"-b", "-PreviewImage", "--", path}
}

func jpgFromRawArgs(path string) []string {
	return []string{"-b", "-JpgFromRaw", "--", path}
}

func thumbnailImageArgs(path string) []string {
	return []string{"-b", "-ThumbnailImage", "--", path}
}

// videoPosterSeekOffsets are the -ss timestamps ExtractVideoPoster tries in
// order. "1" (one second in) is preferred -- it skips a common all-black or
// fade-in opening frame -- but a clip shorter than one second has nothing
// there, so "0" (the very first frame) is the guaranteed-to-exist fallback.
var videoPosterSeekOffsets = []string{"1", "0"}

// videoPosterArgs builds ffmpeg's argv for extracting a single representative
// frame as an MJPEG stream on stdout: -ss (input-side, fast) seek before -i,
// -frames:v 1 to stop after exactly one frame, "-f image2pipe -vcodec mjpeg"
// to encode that frame as JPEG and write it to the pipe:1 (stdout) sink
// rather than a file on disk. -y suppresses ffmpeg's interactive
// overwrite-prompt (irrelevant here since there's no output file to
// overwrite, but keeps the invocation non-interactive regardless of ffmpeg
// version defaults). -nostdin additionally guarantees ffmpeg never tries to
// read from stdin and blocks. path is always ffmpeg's -i argument's own
// value -- consumed positionally as that flag's required argument by
// ffmpeg's own parser, never re-parsed as a flag itself, so a path starting
// with "-" can't be misread the way it could in a flag-value-ambiguous
// context.
func videoPosterArgs(path, seekSeconds string) []string {
	return []string{
		"-y", "-nostdin", "-v", "error",
		"-ss", seekSeconds,
		"-i", path,
		"-frames:v", "1",
		"-q:v", "3",
		"-f", "image2pipe",
		"-vcodec", "mjpeg",
		"pipe:1",
	}
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

	if sidecar, ok := p.readSidecarRow(ctx, path); ok {
		mergeSidecarRow(row, sidecar)
	}

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

// readSidecarRow looks for a standalone XMP sidecar next to path (see
// sidecarPath) and, if one exists, reads it through exiftool the same
// read-only way as the RAW itself -- exiftoolArgs, unchanged, so the
// never-writes property TestExiftoolArgsNeverWrite proves stays intact for
// this second invocation too. The stat check is what keeps exiftool's
// behavior/exit code unchanged for the common case of no sidecar: exiftool
// is never invoked a second time unless a sidecar file actually exists.
// Returns ok=false whenever there's no sidecar to merge, or the sidecar
// itself can't be read or parsed -- a missing or broken sidecar must never
// fail probing of the RAW it sits next to.
//
// Guards against two failure modes:
//   - path itself already IS a ".xmp" (even with .xmp registered in
//     internal/projectfile, a direct probe on an .xmp can still reach here).
//     sidecarPath("x.xmp") is a fixed point ("x.xmp" again), so without this
//     check Exif would spawn a second exiftool against the same file and merge
//     its own row into itself on every sidecar node, doubling the subprocess
//     cost for no effect.
//   - os.Lstat, deliberately not os.Stat -- same non-following stat used by
//     internal/prune.Execute's pre-delete check (see AGENTS.md's key
//     invariants): reject a symlink outright rather than following it, so
//     a sidecar path that's actually a symlink escape can't get read.
//     (storage.Guard.CheckWrite's own canonicalize step is the opposite
//     mechanism -- it uses filepath.EvalSymlinks to resolve a symlink to
//     its real target before checking that target, not to reject the
//     symlink itself; not the right comparison here.) Mode().IsRegular()
//     rather than a bare !IsDir() additionally rejects a FIFO planted at
//     the sidecar path, which would otherwise block exiftool's read until
//     probeTimeout fires on every scan of the RAW next to it.
func (p *Prober) readSidecarRow(ctx context.Context, path string) (map[string]any, bool) {
	if strings.EqualFold(filepath.Ext(path), sidecarExt) {
		return nil, false
	}

	sidecar := sidecarPath(path)
	info, err := os.Lstat(sidecar)
	if err != nil || !info.Mode().IsRegular() {
		return nil, false
	}

	cmd := exec.CommandContext(ctx, p.exiftoolPath, exiftoolArgs(sidecar)...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		return nil, false
	}

	var rows []map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &rows); err != nil || len(rows) == 0 {
		return nil, false
	}
	return rows[0], true
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

// ExtractPreviewJPEG extracts a decodable embedded preview image from path
// via exiftool, trying -PreviewImage, then -JpgFromRaw, then -ThumbnailImage
// in order and returning the bytes of the first one that both produces
// output and decodes successfully via Go's standard image library (a tag
// can be present but corrupt or empty for a given camera/format, so a
// decode failure falls through to the next tag rather than returning bad
// bytes). Returns nil, nil -- not an error -- when exiftool is unavailable
// or none of the three tags yield a decodable image; that is the normal
// case for a file with no embedded preview, not a failure.
func (p *Prober) ExtractPreviewJPEG(ctx context.Context, path string) ([]byte, error) {
	if !p.HasExiftool() {
		return nil, nil
	}
	for _, argsFn := range []func(string) []string{previewImageArgs, jpgFromRawArgs, thumbnailImageArgs} {
		cmd := exec.CommandContext(ctx, p.exiftoolPath, argsFn(path)...)
		var stdout bytes.Buffer
		cmd.Stdout = &stdout
		if err := cmd.Run(); err != nil || stdout.Len() == 0 {
			continue
		}
		if _, _, err := image.Decode(bytes.NewReader(stdout.Bytes())); err != nil {
			continue
		}
		return stdout.Bytes(), nil
	}
	return nil, nil
}

// ExtractVideoPoster extracts a single representative frame from a video at
// path via ffmpeg, encoded as a JPEG, trying videoPosterSeekOffsets in order
// (one second in, then the very first frame) and returning the bytes of the
// first attempt that both produces output and decodes successfully via Go's
// standard image library -- the same "try a few things, verify decodability,
// fall through" shape as ExtractPreviewJPEG above, and for the same reason: a
// seek target past a very short clip's end (or a corrupt/truncated file)
// yields nothing usable from that attempt without making the file itself an
// error.
//
// Returns nil, nil -- not an error -- when ffmpeg is unavailable or every
// seek attempt genuinely produced nothing usable; that is the normal case
// for a non-video/unreadable file, not a failure. This is a single-frame
// poster image only (#224's stated scope) -- no animated preview or contact
// sheet.
//
// A ctx cancellation or deadline is deliberately NOT folded into that
// nil-nil "nothing usable" case, unlike a plain ffmpeg failure (bad codec,
// truncated file, etc.) -- ctx.Err() is checked after every failed attempt
// and, if set, returned as a real error immediately rather than falling
// through to try the next seek offset (which would fail instantly anyway,
// against an already-dead context) or exhausting the loop silently. This
// matters to internal/thumbs.Cache.Generate's caller: a nil-nil result maps
// to the terminal, never-retried UNSUPPORTED thumb_state, while a real error
// maps to FAILED, which IS retried up to the worker's max-attempts bound --
// a shutdown or slow-storage timeout mid-video (the slowest file type this
// package handles, and thus the most likely to be mid-flight when either
// happens) must land on the latter, not silently and permanently give up on
// that node.
func (p *Prober) ExtractVideoPoster(ctx context.Context, path string) ([]byte, error) {
	if !p.HasFFmpeg() {
		return nil, nil
	}
	for _, seek := range videoPosterSeekOffsets {
		cmd := exec.CommandContext(ctx, p.ffmpegPath, videoPosterArgs(path, seek)...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		runErr := cmd.Run()
		if runErr != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, fmt.Errorf("probe: ffmpeg %s: %w (stderr: %s)", path, ctxErr, stderr.String())
			}
			continue
		}
		if stdout.Len() == 0 {
			continue
		}
		if _, _, err := image.Decode(bytes.NewReader(stdout.Bytes())); err != nil {
			continue
		}
		return stdout.Bytes(), nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return nil, fmt.Errorf("probe: ffmpeg %s: %w", path, ctxErr)
	}
	return nil, nil
}

// ExtractPHash attempts to compute a perceptual hash (pHash) for path.
// It first attempts direct image decoding using Go's standard image library.
// If direct decoding fails (e.g. for camera RAW files like .ARW, .CR2, .NEF, .DNG),
// it falls back to ExtractPreviewJPEG and computes pHash over the decoded
// preview image. Returns nil, nil if the file is not an image or if pHash
// extraction is unsupported.
//
// Note: prior to the ExtractPreviewJPEG extraction (lifted out for reuse by
// internal/thumbs), a failed hash computation after a successful decode
// would try the next exiftool tag; it no longer does, since retrying a
// different tag to work around a hashing-library failure on an already
// -- successfully decoded image is not something a shared preview extractor
// should encode. hashing.PerceptualHash failing on a validly decoded image
// is not observed in practice.
func (p *Prober) ExtractPHash(ctx context.Context, path string) (*int64, error) {
	if ph, err := p.decodeFileAndHash(path); err == nil {
		return ph, nil
	}

	preview, err := p.ExtractPreviewJPEG(ctx, path)
	if err != nil || len(preview) == 0 {
		return nil, nil
	}
	img, _, err := image.Decode(bytes.NewReader(preview))
	if err != nil {
		return nil, nil
	}
	hash, err := hashing.PerceptualHash(img)
	if err != nil {
		return nil, nil
	}
	return &hash, nil
}

func (p *Prober) decodeFileAndHash(path string) (*int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()

	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	hash, err := hashing.PerceptualHash(img)
	if err != nil {
		return nil, err
	}
	return &hash, nil
}
