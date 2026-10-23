package qr

import (
	"strings"
	"testing"

	"github.com/boombuler/barcode/qr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRenderSVG_ValidPayloadProducesSVG(t *testing.T) {
	out, err := RenderSVG("branchdam://server=http://x:8080&key=abc&agent=iphone", 0)
	require.NoError(t, err)
	s := string(out)
	assert.True(t, strings.HasPrefix(s, "<?xml"), "SVG output should start with the XML declaration")
	assert.Contains(t, s, "<svg")
	assert.Contains(t, s, "</svg>")
}

func TestRenderSVG_EmptyPayloadReturnsError(t *testing.T) {
	_, err := RenderSVG("", 0)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty payload")
}

func TestRenderSVG_DefaultSizeIsApplied(t *testing.T) {
	out, err := RenderSVG("hello world", 0)
	require.NoError(t, err)
	s := string(out)
	// The 256-pixel default produces a viewBox attribute of "0 0 256 256".
	assert.Contains(t, s, "viewBox=\"0 0 256 256\"")
}

func TestRenderSVG_CustomSizeRespected(t *testing.T) {
	out, err := RenderSVG("hello world", 512)
	require.NoError(t, err)
	s := string(out)
	assert.Contains(t, s, "viewBox=\"0 0 512 512\"")
}

func TestRenderSVG_ShortPayloadHasModules(t *testing.T) {
	// A minimal valid QR code is at least ~21 modules wide (Version 1).
	// Anything less means qrc.New silently failed or emitted a near-empty
	// SVG -- this test guards against that footgun.
	out, err := RenderSVG("a", 0)
	require.NoError(t, err)
	assert.Greater(t, len(out), 500, "a single-char QR should still produce ~600+ bytes of SVG")
}

// finderPatternDark reports whether the module at (i, j) inside a 7x7
// finder pattern is dark. A finder pattern is a 7x7 square: a dark outer
// ring, a light ring inside it, and a dark 3x3 core.
func finderPatternDark(i, j int) bool {
	r := i - 3
	if r < 0 {
		r = -r
	}
	c := j - 3
	if c < 0 {
		c = -c
	}
	d := r
	if c > d {
		d = c
	}
	return d == 3 || d <= 1
}

// TestRenderSVG_FinderPatternsAreDark: the rendered module grid must be a
// REAL QR code and not an inverted one.
//
// The encoder hands back an image.Image, so the grid is read by comparing
// each module's colour against the scheme's foreground. Getting that
// comparison backwards (foreground for background, or the other way round)
// inverts every module and still yields a well-formed SVG -- one that no
// phone can scan. The three finder patterns are the invariant that catches
// it: the same 7x7 square at the top-left, top-right and bottom-left
// corners of every QR code.
func TestRenderSVG_FinderPatternsAreDark(t *testing.T) {
	const payload = "branchdam://server=http://x:8080&key=abc&agent=iphone"
	code, err := qr.Encode(payload, RecoveryLevel, qr.Auto)
	require.NoError(t, err)

	grid := bitmapOf(code)
	require.NotEmpty(t, grid, "the encoder returned an empty module grid")
	width := len(grid[0])
	for _, row := range grid {
		require.Len(t, row, width, "the module grid is not rectangular")
	}
	// Version 1 is 21 modules wide; a longer payload only makes it bigger.
	require.GreaterOrEqual(t, width, 21, "a QR code is at least 21 modules wide")

	corners := []struct {
		name string
		ox   int
		oy   int
	}{
		{"top-left", 0, 0},
		{"top-right", width - 7, 0},
		{"bottom-left", 0, len(grid) - 7},
	}
	for _, c := range corners {
		for i := 0; i < 7; i++ {
			for j := 0; j < 7; j++ {
				got := grid[c.oy+i][c.ox+j]
				want := finderPatternDark(i, j)
				if got != want {
					t.Fatalf("%s finder pattern module (%d,%d) = %v, want %v -- the grid is inverted",
						c.name, i, j, got, want)
				}
			}
		}
	}
}
