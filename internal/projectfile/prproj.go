package projectfile

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

var (
	// ErrMalformedPRPROJ is returned when a .prproj file is invalid or cannot be parsed.
	ErrMalformedPRPROJ = errors.New("malformed .prproj file")
)

func init() {
	Register(&PRPROJParser{})
}

// PRPROJParser parses Adobe Premiere Pro (.prproj) gzipped XML project files.
type PRPROJParser struct{}

func (p *PRPROJParser) Extensions() []string {
	return []string{"prproj", ".prproj"}
}

func (p *PRPROJParser) Parse(ctx context.Context, r io.Reader) ([]Reference, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if r == nil {
		return nil, fmt.Errorf("%w: reader is nil", ErrMalformedPRPROJ)
	}

	lr := io.LimitReader(r, MaxDRPArchiveSize+1)
	compressedData, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("%w: read failed: %v", ErrMalformedPRPROJ, err)
	}
	if len(compressedData) > MaxDRPArchiveSize {
		return nil, ErrArchiveTooLarge
	}
	if len(compressedData) == 0 {
		return nil, fmt.Errorf("%w: empty input", ErrMalformedPRPROJ)
	}

	gzReader, err := gzip.NewReader(bytes.NewReader(compressedData))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid gzip stream: %v", ErrMalformedPRPROJ, err)
	}
	defer func() { _ = gzReader.Close() }()

	uncompressedData, err := io.ReadAll(io.LimitReader(gzReader, MaxDRPUncompressedBytes+1))
	if err != nil {
		return nil, fmt.Errorf("%w: decompress failed: %v", ErrMalformedPRPROJ, err)
	}

	if len(uncompressedData) > MaxDRPUncompressedBytes {
		return nil, ErrZipBomb
	}

	if len(uncompressedData) > 5*1024*1024 && len(compressedData) > 0 && float64(len(uncompressedData))/float64(len(compressedData)) > MaxDRPExpansionRatio {
		return nil, ErrZipBomb
	}

	return parsePRPROJXML(uncompressedData)
}

func parsePRPROJXML(data []byte) ([]Reference, error) {
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
			return nil, fmt.Errorf("%w: invalid xml: %v", ErrMalformedPRPROJ, err)
		}

		switch t := token.(type) {
		case xml.StartElement:
			tagName := strings.ToLower(t.Name.Local)
			if isPRPROJPathTag(tagName) {
				inPathTag = true
				currentTag = tagName
				textBuf.Reset()
			}
			for _, attr := range t.Attr {
				attrName := strings.ToLower(attr.Name.Local)
				if (attrName == "src" || attrName == "path" || attrName == "filepath" || attrName == "url") && attr.Value != "" {
					addPRPROJRef(&refs, seen, attr.Value, "media")
				}
			}
		case xml.EndElement:
			tagName := strings.ToLower(t.Name.Local)
			if inPathTag && tagName == currentTag {
				val := strings.TrimSpace(textBuf.String())
				if val != "" {
					role := "media"
					if strings.Contains(currentTag, "proxy") {
						role = "proxy"
					}
					addPRPROJRef(&refs, seen, val, role)
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

func isPRPROJPathTag(tag string) bool {
	switch tag {
	case "filepath", "actualmediafilepath", "mediafilepath", "importpath", "relativepath", "proxypath", "proxyfile", "proxyfilepath", "path", "src", "location", "url":
		return true
	default:
		return false
	}
}

func addPRPROJRef(refs *[]Reference, seen map[string]bool, raw, role string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return
	}

	cleaned := raw
	if strings.HasPrefix(cleaned, "file://") {
		u, err := url.Parse(cleaned)
		if err == nil {
			cleaned = u.Path
			if len(cleaned) > 3 && cleaned[0] == '/' && cleaned[2] == ':' {
				cleaned = cleaned[1:]
			}
		}
	} else if unescaped, err := url.PathUnescape(cleaned); err == nil {
		cleaned = unescaped
	}

	if cleaned == "" || seen[cleaned] {
		return
	}
	seen[cleaned] = true

	if role == "media" && strings.Contains(strings.ToLower(raw), "proxy") {
		role = "proxy"
	}

	*refs = append(*refs, Reference{
		RawPath: cleaned,
		Role:    role,
	})
}
