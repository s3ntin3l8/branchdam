package projectfile

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// ErrMalformedEDL is returned when an .edl file cannot be parsed or is empty.
var ErrMalformedEDL = errors.New("malformed .edl file")

func init() {
	Register(&EDLParser{})
}

// EDLParser parses CMX3600 Edit Decision List (.edl) text files.
type EDLParser struct{}

func (p *EDLParser) Extensions() []string {
	return []string{"edl", ".edl"}
}

func (p *EDLParser) Parse(ctx context.Context, r io.Reader) ([]Reference, error) {
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	if r == nil {
		return nil, fmt.Errorf("%w: reader is nil", ErrMalformedEDL)
	}

	data, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("%w: read failed: %v", ErrMalformedEDL, err)
	}
	if len(data) == 0 {
		return nil, fmt.Errorf("%w: empty input", ErrMalformedEDL)
	}

	scanner := bufio.NewScanner(bytes.NewReader(data))
	var refs []Reference
	seen := make(map[string]bool)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		// Comment lines starting with * contain source clip details in CMX3600
		if strings.HasPrefix(line, "*") {
			path := extractEDLCommentPath(line)
			if path != "" && !seen[path] {
				seen[path] = true
				role := "media"
				if strings.Contains(strings.ToLower(path), "proxy") {
					role = "proxy"
				}
				refs = append(refs, Reference{
					RawPath: path,
					Role:    role,
				})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("%w: scanner error: %v", ErrMalformedEDL, err)
	}

	return refs, nil
}

func extractEDLCommentPath(line string) string {
	// Strip leading * and trim whitespace
	content := strings.TrimSpace(strings.TrimPrefix(line, "*"))

	// Known prefixes used by CMX3600 EDL generators (DaVinci Resolve, Premiere, Avid, FCP)
	prefixes := []string{
		"FROM CLIP WITH TRANSFER:",
		"FROM CLIP:",
		"SOURCE FILE:",
		"CLIP NAME:",
		"FINAL CUT PRO CLIP NAME:",
		"FILE NAME:",
	}

	for _, prefix := range prefixes {
		if strings.HasPrefix(strings.ToUpper(content), prefix) {
			val := strings.TrimSpace(content[len(prefix):])
			if val != "" {
				return val
			}
		}
	}

	return ""
}
