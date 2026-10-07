package projectfile

import (
	"context"
	"fmt"
	"io"
)

// Reference represents a media file referenced inside a project file.
type Reference struct {
	RawPath string `json:"raw_path"`
	Role    string `json:"role,omitempty"`
}

// Parser parses a project file from an io.Reader and extracts referenced media paths.
type Parser interface {
	Extensions() []string
	Parse(ctx context.Context, r io.Reader) ([]Reference, error)
}

// MaxTextProjectBytes caps how much of a text-based project file (EDL,
// FCPXML, XMP, .dam.json) a parser will buffer. Real files are KBs to a few
// MB; the cap only stops a huge or hostile file from being slurped into RAM.
const MaxTextProjectBytes = 64 * 1024 * 1024

// readBounded reads all of r but fails once it exceeds MaxTextProjectBytes.
func readBounded(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, MaxTextProjectBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > MaxTextProjectBytes {
		return nil, fmt.Errorf("input exceeds %d bytes", MaxTextProjectBytes)
	}
	return data, nil
}
