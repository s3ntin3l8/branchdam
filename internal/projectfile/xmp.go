package projectfile

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
)

func init() {
	Register(&XMPParser{})
}

// XMPParser parses .xmp sidecar files.
type XMPParser struct{}

func (p *XMPParser) Extensions() []string {
	return []string{"xmp", ".xmp"}
}

// Parse extracts any media references declared inside an XMP sidecar file.
// If the XMP document contains crs:RawFileName or other explicit referenced
// media paths, they are extracted as References. Otherwise, if the XMP is
// valid XML with no explicit path, it returns an empty slice of references.
func (p *XMPParser) Parse(ctx context.Context, r io.Reader) ([]Reference, error) {
	if r == nil {
		return nil, errors.New("projectfile: reader is nil")
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("projectfile: read xmp: %w", err)
	}

	if len(data) == 0 {
		return nil, nil
	}

	var refs []Reference
	decoder := xml.NewDecoder(bytes.NewReader(data))
	for {
		t, err := decoder.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("projectfile: malformed xmp: %w", err)
		}
		switch elem := t.(type) {
		case xml.StartElement:
			for _, attr := range elem.Attr {
				if attr.Name.Local == "RawFileName" && attr.Value != "" {
					refs = append(refs, Reference{RawPath: attr.Value, Role: "raw"})
				}
			}
		}
	}

	return refs, nil
}
