package projectfile_test

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/projectfile"
)

func createTestDRPZip(t *testing.T, xmlContent string) []byte {
	t.Helper()
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)

	w, err := zw.Create("project.xml")
	if err != nil {
		t.Fatalf("failed to create project.xml in zip: %v", err)
	}
	if _, err := w.Write([]byte(xmlContent)); err != nil {
		t.Fatalf("failed to write project.xml: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("failed to close zip writer: %v", err)
	}
	return buf.Bytes()
}

func TestDRPParser_Parse_Valid(t *testing.T) {
	xmlContent := `<?xml version="1.0" encoding="UTF-8"?>
<DaVinciResolveProject>
	<MediaPool>
		<Item>
			<FilePath>D:\Footage\CameraA\Clip &amp; Take 001.mov</FilePath>
			<ProxyPath>D:\Footage\CameraA\Clip001_proxy.mp4</ProxyPath>
		</Item>
		<Item>
			<MediaPath>/storage/projects/video/B002_C010.ARW</MediaPath>
		</Item>
	</MediaPool>
</DaVinciResolveProject>`

	drpBytes := createTestDRPZip(t, xmlContent)

	parser, ok := projectfile.GetParser("sample.drp")
	if !ok {
		t.Fatalf("expected .drp parser to be registered")
	}

	refs, err := parser.Parse(context.Background(), bytes.NewReader(drpBytes))
	if err != nil {
		t.Fatalf("unexpected error parsing drp: %v", err)
	}

	if len(refs) != 3 {
		t.Fatalf("expected 3 references, got %d: %+v", len(refs), refs)
	}

	if refs[0].RawPath != `D:\Footage\CameraA\Clip & Take 001.mov` || refs[0].Role != "media" {
		t.Errorf("ref 0 mismatch: %+v", refs[0])
	}
	if refs[1].RawPath != `D:\Footage\CameraA\Clip001_proxy.mp4` || refs[1].Role != "proxy" {
		t.Errorf("ref 1 mismatch: %+v", refs[1])
	}
	if refs[2].RawPath != `/storage/projects/video/B002_C010.ARW` || refs[2].Role != "media" {
		t.Errorf("ref 2 mismatch: %+v", refs[2])
	}
}

func TestDRPParser_Parse_Fixture(t *testing.T) {
	fixturePath := filepath.Join("testdata", "sample.drp")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("failed to read test fixture %s: %v", fixturePath, err)
	}

	parser := &projectfile.DRPParser{}
	refs, err := parser.Parse(context.Background(), bytes.NewReader(data))
	if err != nil {
		t.Fatalf("failed to parse drp fixture: %v", err)
	}
	if len(refs) == 0 {
		t.Fatalf("expected at least 1 reference from drp fixture, got 0")
	}
}

func TestDRPParser_Parse_MacOSSidecars(t *testing.T) {
	buf := new(bytes.Buffer)
	zw := zip.NewWriter(buf)

	// Add macOS sidecar first
	w1, _ := zw.Create("__MACOSX/._project.xml")
	_, _ = w1.Write([]byte("junk macos binary data"))

	// Add real project.xml
	w2, _ := zw.Create("project.xml")
	_, _ = w2.Write([]byte(`<project><filepath>D:\Real\Path.mov</filepath></project>`))
	_ = zw.Close()

	parser := &projectfile.DRPParser{}
	refs, err := parser.Parse(context.Background(), bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("unexpected error with macos sidecar: %v", err)
	}
	if len(refs) != 1 || refs[0].RawPath != `D:\Real\Path.mov` {
		t.Errorf("expected 1 reference matching real path, got %+v", refs)
	}
}

func TestDRPParser_Parse_MalformedAndLimits(t *testing.T) {
	t.Run("corrupt zip input", func(t *testing.T) {
		parser := &projectfile.DRPParser{}
		_, err := parser.Parse(context.Background(), strings.NewReader("not a zip file"))
		if err == nil || !errors.Is(err, projectfile.ErrMalformedDRP) {
			t.Errorf("expected ErrMalformedDRP, got %v", err)
		}
	})

	t.Run("zip missing project.xml", func(t *testing.T) {
		buf := new(bytes.Buffer)
		zw := zip.NewWriter(buf)
		w, _ := zw.Create("other.xml")
		_, _ = w.Write([]byte("<xml></xml>"))
		_ = zw.Close()

		parser := &projectfile.DRPParser{}
		_, err := parser.Parse(context.Background(), bytes.NewReader(buf.Bytes()))
		if err == nil || !errors.Is(err, projectfile.ErrMalformedDRP) {
			t.Errorf("expected ErrMalformedDRP for missing project.xml, got %v", err)
		}
	})

	t.Run("archive size cap exceeded", func(t *testing.T) {
		oversized := make([]byte, projectfile.MaxDRPArchiveSize+10)
		parser := &projectfile.DRPParser{}
		_, err := parser.Parse(context.Background(), bytes.NewReader(oversized))
		if err == nil || !errors.Is(err, projectfile.ErrArchiveTooLarge) {
			t.Errorf("expected ErrArchiveTooLarge, got %v", err)
		}
	})
}
