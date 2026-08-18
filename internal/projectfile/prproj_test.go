package projectfile_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/projectfile"
)

func createTestGzipPRPROJ(t *testing.T, xmlContent string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	gw := gzip.NewWriter(buf)
	if _, err := gw.Write([]byte(xmlContent)); err != nil {
		t.Fatalf("failed to write gzip data: %v", err)
	}
	if err := gw.Close(); err != nil {
		t.Fatalf("failed to close gzip writer: %v", err)
	}
	return buf.Bytes()
}

func TestPRPROJParser_Parse_Valid(t *testing.T) {
	xmlContent := `<?xml version="1.0" encoding="UTF-8"?>
<PremiereData>
    <Project>
        <Media>
            <FilePath>D:\Footage\CameraA\Clip &amp; Take 001.mov</FilePath>
            <ActualMediaFilePath>/storage/projects/video/B002_C010.ARW</ActualMediaFilePath>
            <ProxyPath>D:\Footage\Render001.mp4</ProxyPath>
        </Media>
    </Project>
</PremiereData>`

	gzData := createTestGzipPRPROJ(t, xmlContent)

	parser, ok := projectfile.GetParser("sample.prproj")
	if !ok {
		t.Fatalf("expected .prproj parser to be registered")
	}

	refs, err := parser.Parse(context.Background(), bytes.NewReader(gzData))
	if err != nil {
		t.Fatalf("unexpected error parsing prproj: %v", err)
	}

	if len(refs) != 3 {
		t.Fatalf("expected 3 references, got %d: %+v", len(refs), refs)
	}

	if refs[0].RawPath != `D:\Footage\CameraA\Clip & Take 001.mov` || refs[0].Role != "media" {
		t.Errorf("ref 0 mismatch: %+v", refs[0])
	}
	if refs[1].RawPath != `/storage/projects/video/B002_C010.ARW` || refs[1].Role != "media" {
		t.Errorf("ref 1 mismatch: %+v", refs[1])
	}
	if refs[2].RawPath != `D:\Footage\Render001.mp4` || refs[2].Role != "proxy" {
		t.Errorf("ref 2 mismatch: %+v", refs[2])
	}
}

func TestPRPROJParser_Parse_Fixture(t *testing.T) {
	fixturePath := filepath.Join("testdata", "sample.prproj")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("failed to read test fixture %s: %v", fixturePath, err)
	}

	parser := &projectfile.PRPROJParser{}
	refs, err := parser.Parse(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("failed to parse prproj fixture: %v", err)
	}
	if len(refs) == 0 {
		t.Fatalf("expected at least 1 reference from prproj fixture, got 0")
	}
}

func TestPRPROJParser_Parse_MalformedAndLimits(t *testing.T) {
	t.Run("nil reader", func(t *testing.T) {
		parser := &projectfile.PRPROJParser{}
		_, err := parser.Parse(context.Background(), nil)
		if err == nil || !errors.Is(err, projectfile.ErrMalformedPRPROJ) {
			t.Errorf("expected ErrMalformedPRPROJ for nil reader, got %v", err)
		}
	})

	t.Run("empty input", func(t *testing.T) {
		parser := &projectfile.PRPROJParser{}
		_, err := parser.Parse(context.Background(), strings.NewReader(""))
		if err == nil || !errors.Is(err, projectfile.ErrMalformedPRPROJ) {
			t.Errorf("expected ErrMalformedPRPROJ for empty input, got %v", err)
		}
	})

	t.Run("corrupt gzip input", func(t *testing.T) {
		parser := &projectfile.PRPROJParser{}
		_, err := parser.Parse(context.Background(), strings.NewReader("not a gzip stream"))
		if err == nil || !errors.Is(err, projectfile.ErrMalformedPRPROJ) {
			t.Errorf("expected ErrMalformedPRPROJ, got %v", err)
		}
	})

	t.Run("invalid xml in gzip stream", func(t *testing.T) {
		gzData := createTestGzipPRPROJ(t, "<invalid xml")
		parser := &projectfile.PRPROJParser{}
		_, err := parser.Parse(context.Background(), bytes.NewReader(gzData))
		if err == nil || !errors.Is(err, projectfile.ErrMalformedPRPROJ) {
			t.Errorf("expected ErrMalformedPRPROJ for invalid xml, got %v", err)
		}
	})

	t.Run("archive size cap exceeded", func(t *testing.T) {
		oversized := make([]byte, projectfile.MaxDRPArchiveSize+10)
		parser := &projectfile.PRPROJParser{}
		_, err := parser.Parse(context.Background(), bytes.NewReader(oversized))
		if err == nil || !errors.Is(err, projectfile.ErrArchiveTooLarge) {
			t.Errorf("expected ErrArchiveTooLarge, got %v", err)
		}
	})
}
