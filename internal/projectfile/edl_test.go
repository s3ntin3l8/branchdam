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

func TestEDLParser_Parse_Valid(t *testing.T) {
	edlContent := `TITLE: SAMPLE_PROJECT
FCM: DROP FRAME

001  AX       V     C        00:00:00:00 00:00:05:00 01:00:00:00 01:00:05:00
* FROM CLIP WITH TRANSFER: D:\Footage\CameraA\Clip001.mov
* FROM CLIP NAME: Clip001_name.mov
* FROM CLIP: Clip001.mov
* SOURCE FILE: /storage/projects/video/B002_C010.ARW
* CLIP NAME: D:\Footage\Proxy_Clip.mp4
`

	parser, ok := projectfile.GetParser(".edl")
	if !ok {
		t.Fatalf("expected .edl parser to be registered")
	}

	refs, err := parser.Parse(context.Background(), strings.NewReader(edlContent))
	if err != nil {
		t.Fatalf("unexpected error parsing edl: %v", err)
	}

	if len(refs) != 5 {
		t.Fatalf("expected 5 references, got %d: %+v", len(refs), refs)
	}

	if refs[0].RawPath != `D:\Footage\CameraA\Clip001.mov` || refs[0].Role != "media" {
		t.Errorf("ref 0 mismatch: %+v", refs[0])
	}
	if refs[1].RawPath != `Clip001_name.mov` || refs[1].Role != "media" {
		t.Errorf("ref 1 mismatch: %+v", refs[1])
	}
	if refs[2].RawPath != `Clip001.mov` || refs[2].Role != "media" {
		t.Errorf("ref 2 mismatch: %+v", refs[2])
	}
	if refs[3].RawPath != `/storage/projects/video/B002_C010.ARW` || refs[3].Role != "media" {
		t.Errorf("ref 3 mismatch: %+v", refs[3])
	}
	if refs[4].RawPath != `D:\Footage\Proxy_Clip.mp4` || refs[4].Role != "proxy" {
		t.Errorf("ref 4 mismatch: %+v", refs[4])
	}
}

func TestEDLParser_Parse_Fixture(t *testing.T) {
	fixturePath := filepath.Join("testdata", "sample.edl")
	data, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("failed to read test fixture %s: %v", fixturePath, err)
	}

	parser := &projectfile.EDLParser{}
	refs, err := parser.Parse(context.Background(), strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("failed to parse edl fixture: %v", err)
	}
	if len(refs) == 0 {
		t.Fatalf("expected at least 1 reference from edl fixture, got 0")
	}
}

func TestEDLParser_Parse_Malformed(t *testing.T) {
	t.Run("empty input", func(t *testing.T) {
		parser := &projectfile.EDLParser{}
		_, err := parser.Parse(context.Background(), strings.NewReader(""))
		if err == nil || !errors.Is(err, projectfile.ErrMalformedEDL) {
			t.Errorf("expected ErrMalformedEDL, got %v", err)
		}
	})
}
