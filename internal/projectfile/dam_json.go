package projectfile

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// ErrMalformedManifest is returned when a .dam.json manifest cannot be decoded.
var ErrMalformedManifest = errors.New("malformed .dam.json manifest")

func init() {
	Register(&DAMJSONParser{})
}

// DAMJSONParser parses .dam.json manifest files.
type DAMJSONParser struct{}

func (p *DAMJSONParser) Extensions() []string {
	return []string{"dam.json", ".dam.json"}
}

type damManifest struct {
	Version         string        `json:"version"`
	ProjectName     string        `json:"project_name"`
	MediaReferences []damMediaRef `json:"media_references"`
	Files           []string      `json:"files"`
}

type damMediaRef struct {
	RawPath string `json:"raw_path"`
	Path    string `json:"path"`
	Role    string `json:"role"`
}

func (p *DAMJSONParser) Parse(ctx context.Context, r io.Reader) ([]Reference, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: reader is nil", ErrMalformedManifest)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: read failed: %v", ErrMalformedManifest, err)
	}

	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty input", ErrMalformedManifest)
	}

	var manifest damManifest
	if err := json.Unmarshal(data, &manifest); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformedManifest, err)
	}

	if manifest.Version == "" || manifest.ProjectName == "" {
		return nil, fmt.Errorf("%w: missing required fields version/project_name", ErrMalformedManifest)
	}

	var refs []Reference
	for _, ref := range manifest.MediaReferences {
		raw := ref.RawPath
		if raw == "" {
			raw = ref.Path
		}
		if raw != "" {
			role := ref.Role
			if role == "" {
				role = "media"
			}
			refs = append(refs, Reference{
				RawPath: raw,
				Role:    role,
			})
		}
	}

	for _, pathStr := range manifest.Files {
		if pathStr != "" {
			refs = append(refs, Reference{
				RawPath: pathStr,
				Role:    "media",
			})
		}
	}

	return refs, nil
}
