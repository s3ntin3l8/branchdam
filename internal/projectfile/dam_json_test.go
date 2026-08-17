package projectfile_test

import (
	"context"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/projectfile"
)

func TestDAMJSONParser_Parse_Valid(t *testing.T) {
	jsonContent := `{
		"version": "1.0",
		"project_name": "Test Project",
		"media_references": [
			{
				"raw_path": "D:\\Footage\\Clip01.mov",
				"role": "media"
			},
			{
				"raw_path": "Z:/Exports/Render.mp4",
				"role": "export"
			}
		],
		"files": [
			"/storage/projects/audio/track01.wav"
		]
	}`

	parser, ok := projectfile.GetParser(".dam.json")
	if !ok {
		t.Fatalf("expected .dam.json parser to be registered")
	}

	refs, err := parser.Parse(context.Background(), strings.NewReader(jsonContent))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(refs) != 3 {
		t.Fatalf("expected 3 references, got %d", len(refs))
	}

	if refs[0].RawPath != `D:\Footage\Clip01.mov` || refs[0].Role != "media" {
		t.Errorf("ref 0 mismatch: %+v", refs[0])
	}
	if refs[1].RawPath != `Z:/Exports/Render.mp4` || refs[1].Role != "export" {
		t.Errorf("ref 1 mismatch: %+v", refs[1])
	}
	if refs[2].RawPath != `/storage/projects/audio/track01.wav` || refs[2].Role != "media" {
		t.Errorf("ref 2 mismatch: %+v", refs[2])
	}
}

func TestDAMJSONParser_Parse_Malformed(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		wantErr bool
	}{
		{"empty input", "", true},
		{"invalid json", "{bad json", true},
	}

	parser := &projectfile.DAMJSONParser{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := parser.Parse(context.Background(), strings.NewReader(tt.input))
			if (err != nil) != tt.wantErr {
				t.Errorf("expected err=%v, got %v", tt.wantErr, err)
			}
		})
	}
}
