package projectfile

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// ErrMalformedFCPXML is returned when an .fcpxml file cannot be parsed.
var ErrMalformedFCPXML = errors.New("malformed .fcpxml file")

func init() {
	Register(&FCPXMLParser{})
}

// FCPXMLParser parses Final Cut Pro XML (.fcpxml) files.
type FCPXMLParser struct{}

func (p *FCPXMLParser) Extensions() []string {
	return []string{"fcpxml", ".fcpxml"}
}

func (p *FCPXMLParser) Parse(ctx context.Context, r io.Reader) ([]Reference, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if r == nil {
		return nil, fmt.Errorf("%w: reader is nil", ErrMalformedFCPXML)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: read failed: %v", ErrMalformedFCPXML, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty input", ErrMalformedFCPXML)
	}

	decoder := xml.NewDecoder(bytes.NewReader(data))
	var refs []Reference
	seen := make(map[string]bool)

	var inPathTag bool
	var currentTag string
	var textBuf strings.Builder

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: invalid xml: %v", ErrMalformedFCPXML, err)
		}

		switch t := token.(type) {
		case xml.StartElement:
			tagName := strings.ToLower(t.Name.Local)
			if isFCPXMLPathTag(tagName) {
				inPathTag = true
				currentTag = tagName
				textBuf.Reset()
			}
			for _, attr := range t.Attr {
				attrName := strings.ToLower(attr.Name.Local)
				if (attrName == "src" || attrName == "path" || attrName == "url" || attrName == "location") && attr.Value != "" {
					addFCPXMLRef(&refs, seen, attr.Value)
				}
			}
		case xml.EndElement:
			tagName := strings.ToLower(t.Name.Local)
			if inPathTag && tagName == currentTag {
				val := strings.TrimSpace(textBuf.String())
				if val != "" {
					addFCPXMLRef(&refs, seen, val)
				}
				inPathTag = false
				currentTag = ""
				textBuf.Reset()
			}
		case xml.CharData:
			if inPathTag {
				textBuf.Write(t)
			}
		}
	}

	return refs, nil
}

func isFCPXMLPathTag(tag string) bool {
	switch tag {
	case "asset", "file", "media-rep", "path", "src", "location", "url":
		return true
	default:
		return false
	}
}

func addFCPXMLRef(refs *[]Reference, seen map[string]bool, raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}

	// Handle file:// URIs common in FCPXML (e.g. file://localhost/D:/Footage/Clip01.mov or file:///storage/clip.mov)
	cleaned := raw
	if strings.HasPrefix(cleaned, "file://") {
		u, err := url.Parse(cleaned)
		if err == nil {
			cleaned = u.Path
			// On Windows-like URI file:///D:/Footage..., trim leading slash before drive letter
			if len(cleaned) > 3 && cleaned[0] == '/' && cleaned[2] == ':' {
				cleaned = cleaned[1:]
			}
		}
	}

	if cleaned == "" || seen[cleaned] {
		return
	}
	seen[cleaned] = true

	role := "media"
	if strings.Contains(strings.ToLower(raw), "proxy") {
		role = "proxy"
	}

	*refs = append(*refs, Reference{
		RawPath: cleaned,
		Role:    role,
	})
}
