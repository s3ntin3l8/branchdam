package djisrt

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseFirstPoint_BracketFormat(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "sample.srt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	p, err := ParseFirstPoint(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("ParseFirstPoint: %v", err)
	}
	// The fixture's first cue (FrameCnt: 1) carries
	// [latitude: 30.335120] [longitude: -81.655480] -- the second and third
	// cues have slightly different values, so returning those instead would
	// mean this test caught ParseFirstPoint reading past the first match.
	if p.Latitude != 30.335120 {
		t.Errorf("Latitude = %v, want 30.335120 (first cue, not a later one)", p.Latitude)
	}
	if p.Longitude != -81.655480 {
		t.Errorf("Longitude = %v, want -81.655480 (first cue, not a later one)", p.Longitude)
	}
}

func TestParseFirstPoint_LegacyGPSParenFormat(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "sample_legacy_gps_paren.srt"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	p, err := ParseFirstPoint(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("ParseFirstPoint: %v", err)
	}
	// GPS(longitude,latitude,altitude) -- longitude first, see package doc.
	if p.Latitude != 30.335120 {
		t.Errorf("Latitude = %v, want 30.335120", p.Latitude)
	}
	if p.Longitude != -81.655480 {
		t.Errorf("Longitude = %v, want -81.655480", p.Longitude)
	}
}

func TestParseFirstPoint_NoGPSData(t *testing.T) {
	text := `1
00:00:00,000 --> 00:00:00,033
<font size="28">FrameCnt: 1, DiffTime: 33ms
2024-03-20 12:59:17,819
[iso: 400] [shutter: 1/320.0] [fnum: 1.7]</font>
`
	_, err := ParseFirstPoint(strings.NewReader(text))
	if !errors.Is(err, ErrNoGPSData) {
		t.Fatalf("err = %v, want ErrNoGPSData", err)
	}
}

func TestParseFirstPoint_EmptyInput(t *testing.T) {
	_, err := ParseFirstPoint(strings.NewReader(""))
	if !errors.Is(err, ErrNoGPSData) {
		t.Fatalf("err = %v, want ErrNoGPSData", err)
	}
}

func TestParseFirstPoint_CaseAndSpacingTolerance(t *testing.T) {
	// Observed firmware variance: uppercase keys, a space before the colon.
	text := `[LATITUDE : 12.5] [LONGITUDE : -45.25]`
	p, err := ParseFirstPoint(strings.NewReader(text))
	if err != nil {
		t.Fatalf("ParseFirstPoint: %v", err)
	}
	if p.Latitude != 12.5 || p.Longitude != -45.25 {
		t.Errorf("got (%v, %v), want (12.5, -45.25)", p.Latitude, p.Longitude)
	}
}

func TestParseFirstPoint_CRLF(t *testing.T) {
	text := "1\r\n00:00:00,000 --> 00:00:00,033\r\n[latitude: 1.0] [longitude: 2.0]\r\n"
	p, err := ParseFirstPoint(strings.NewReader(text))
	if err != nil {
		t.Fatalf("ParseFirstPoint: %v", err)
	}
	if p.Latitude != 1.0 || p.Longitude != 2.0 {
		t.Errorf("got (%v, %v), want (1.0, 2.0)", p.Latitude, p.Longitude)
	}
}
