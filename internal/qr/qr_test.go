package qr

import (
	"strings"
	"testing"

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
