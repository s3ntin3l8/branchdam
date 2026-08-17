package projectfile_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/projectfile"
)

func TestFCPXMLParser_Parse_Valid(t *testing.T) {
	xmlContent := `<?xml version="1.0" encoding="UTF-8"?>
<fcpxml version="1.8">
    <resources>
        <asset id="r1" name="Clip001" src="file:///D:/Footage/CameraA/Clip001.mov" />
        <asset id="r2" name="ProxyClip" src="file:///D:/Footage/CameraA/Proxy_Clip.mp4" />
        <asset id="r3" name="B002" src="/storage/projects/video/B002_C010.ARW" />
    </resources>
</fcpxml>`

	parser, ok := projectfile.GetParser(".fcpxml")
	if !ok {
		t.Fatalf("expected .fcpxml parser to be registered")
	}

	refs, err := parser.Parse(context.Background(), strings.NewReader(xmlContent))
	if err != nil {
		t.Fatalf("unexpected error parsing fcpxml: %v", err)
	}

	if len(refs) != 3 {
		t.Fatalf("expected 3 references, got %d: %+v", len(refs), refs)
	}

	if refs[0].RawPath != `D:/Footage/CameraA/Clip001.mov` || refs[0].Role != "media" {
		t.Errorf("ref 0 mismatch: %+v", refs[0])
	}
	if refs[1].RawPath != `D:/Footage/CameraA/Proxy_Clip.mp4` || refs[1].Role != "proxy" {
		t.Errorf("ref 1 mismatch: %+v", refs[1])
	}
	if refs[2].RawPath != `/storage/projects/video/B002_C010.ARW` || refs[2].Role != "media" {
		t.Errorf("ref 2 mismatch: %+v", refs[2])
	}
}

func TestFCPXMLParser_Parse_Fixture(t *testing.T) {
	fixturePath := filepath.Join("testdata", "sample.fcpxml")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("failed to read test fixture %s: %v", fixturePath, err)
	}

	parser := &projectfile.FCPXMLParser{}
	refs, err := parser.Parse(context.Background(), strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("failed to parse fcpxml fixture: %v", err)
	}
	if len(refs) == 0 {
		t.Fatalf("expected at least 1 reference from fcpxml fixture, got 0")
	}
}

func TestFCPXMLParser_Parse_Malformed(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		parser := &projectfile.FCPXMLParser{}
		_, err := parser.Parse(context.Background(), strings.NewReader(""))
		if err == nil || !errors.Is(err, projectfile.ErrMalformedFCPXML) {
			t.Errorf("expected ErrMalformedFCPXML, got %v", err)
		}
	})

	t.Run("invalid xml", func(t *testing.T) {
		parser := &projectfile.FCPXMLParser{}
		_, err := parser.Parse(context.Background(), strings.NewReader("<invalid xml"))
		if err == nil || !errors.Is(err, projectfile.ErrMalformedFCPXML) {
			t.Errorf("expected ErrMalformedFCPXML, got %v", err)
		}
	})
}
