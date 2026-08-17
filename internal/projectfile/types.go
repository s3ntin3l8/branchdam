package projectfile

import (
	"context"
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
