package projectfile

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	// MaxDRPArchiveSize is the maximum size (50MB) allowed for a .drp archive.
	MaxDRPArchiveSize = 50 * 1024 * 1024
	// MaxDRPUncompressedBytes is the total uncompressed size limit (100MB).
	MaxDRPUncompressedBytes = 100 * 1024 * 1024
	// MaxDRPExpansionRatio is the maximum allowed compression expansion ratio (20:1).
	MaxDRPExpansionRatio = 20
)

var (
	// ErrMalformedDRP is returned when a .drp archive is invalid or missing project.xml.
	ErrMalformedDRP = errors.New("malformed .drp archive")
	// ErrArchiveTooLarge is returned when a .drp archive exceeds the 50MB size limit.
	ErrArchiveTooLarge = errors.New(".drp archive exceeds size limit")
	// ErrZipBomb is returned when uncompressed data exceeds expansion safety limits.
	ErrZipBomb = errors.New(".drp archive exceeds safety expansion ratio")
)

func init() {
	Register(&DRPParser{})
}

// DRPParser parses DaVinci Resolve .drp zip archives.
type DRPParser struct{}

func (p *DRPParser) Extensions() []string {
	return []string{"drp", ".drp"}
}

func (p *DRPParser) Parse(ctx context.Context, r io.Reader) ([]Reference, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: reader is nil", ErrMalformedDRP)
	}

	// Read archive into memory up to MaxDRPArchiveSize + 1 to detect size cap violations.
	lr := io.LimitReader(r, MaxDRPArchiveSize+1)
	data, err := io.ReadAll(lr)
	if err != nil {
		return nil, fmt.Errorf("%w: read failed: %v", ErrMalformedDRP, err)
	}
	if len(data) > MaxDRPArchiveSize {
		return nil, ErrArchiveTooLarge
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty input", ErrMalformedDRP)
	}

	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return nil, fmt.Errorf("%w: invalid zip: %v", ErrMalformedDRP, err)
	}

	var totalUncompressed uint64
	var projectXmlFile *zip.File

	for _, file := range zr.File {
		totalUncompressed += file.UncompressedSize64
		if strings.HasSuffix(strings.ToLower(file.Name), "project.xml") {
			projectXmlFile = file
		}
	}

	if totalUncompressed > MaxDRPUncompressedBytes {
		return nil, ErrZipBomb
	}
	if len(data) > 0 && float64(totalUncompressed)/float64(len(data)) > MaxDRPExpansionRatio {
		return nil, ErrZipBomb
	}

	if projectXmlFile == nil {
		return nil, fmt.Errorf("%w: project.xml not found in archive", ErrMalformedDRP)
	}

	rc, err := projectXmlFile.Open()
	if err != nil {
		return nil, fmt.Errorf("%w: failed to open project.xml: %v", ErrMalformedDRP, err)
	}
	defer func() { _ = rc.Close() }()

	xmlData, err := io.ReadAll(io.LimitReader(rc, MaxDRPUncompressedBytes))
	if err != nil {
		return nil, fmt.Errorf("%w: failed to read project.xml: %v", ErrMalformedDRP, err)
	}

	return parseDRPXML(xmlData)
}

func parseDRPXML(data []byte) ([]Reference, error) {
	decoder := xml.NewDecoder(bytes.NewReader(data))
	var refs []Reference
	seen := make(map[string]bool)

	var inPathTag bool
	var currentTag string

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("%w: invalid xml: %v", ErrMalformedDRP, err)
		}

		switch t := token.(type) {
		case xml.StartElement:
			tagName := strings.ToLower(t.Name.Local)
			if isDRPPathTag(tagName) {
				inPathTag = true
				currentTag = tagName
			}
			// Check element attributes for src or path attributes
			for _, attr := range t.Attr {
				attrName := strings.ToLower(attr.Name.Local)
				if (attrName == "src" || attrName == "path" || attrName == "filepath") && attr.Value != "" {
					addRef(&refs, seen, attr.Value)
				}
			}
		case xml.EndElement:
			inPathTag = false
			currentTag = ""
		case xml.CharData:
			if inPathTag {
				val := strings.TrimSpace(string(t))
				if val != "" {
					role := "media"
					if strings.Contains(currentTag, "proxy") {
						role = "proxy"
					}
					addRefWithRole(&refs, seen, val, role)
				}
			}
		}
	}

	return refs, nil
}

func isDRPPathTag(tag string) bool {
	switch tag {
	case "filepath", "mediapath", "masterpath", "path", "src", "clipfile", "proxyfile", "proxypath":
		return true
	default:
		return false
	}
}

func addRef(refs *[]Reference, seen map[string]bool, path string) {
	addRefWithRole(refs, seen, path, "media")
}

func addRefWithRole(refs *[]Reference, seen map[string]bool, path, role string) {
	path = strings.TrimSpace(path)
	if path == "" || seen[path] {
		return
	}
	seen[path] = true
	*refs = append(*refs, Reference{
		RawPath: path,
		Role:    role,
	})
}
