// Package qr emits an SVG QR code for the
// /api/v1/companion/pairings/{id}/qr.svg endpoint.
//
// The encoder is github.com/boombuler/barcode/qr. We rasterize its module
// grid to an inline SVG path. SVG (not PNG) so the operator can resize
// the browser's QR panel without pixelation, the file is text and
// diffable for security review, and it inlines cleanly into React via
// dangerouslySetInnerHTML if a future SPA revision prefers that over
// an <img src=...>.
//
// The package is intentionally small: one entry point (RenderSVG) that
// accepts the raw payload string. Higher-level encoding (the
// branchdam://?server=…&key=…&agent=… URL format) lives in
// internal/httpapi/companion_pairings.go where it can pull the server URL
// from the request context.
package qr

import (
	"errors"
	"fmt"
	"image/color"
	"strings"

	"github.com/boombuler/barcode"
	qrc "github.com/boombuler/barcode/qr"
)

// DefaultSize is the SVG viewBox edge length. 256 is the smallest size
// at which every modern phone camera reliably resolves the modules in
// indoor lighting; the SPA renders it at 100% of a 320px panel and a
// typical phone screen has ~3x device pixel ratio, so the rendered
// QR is ~960 device px wide.
const DefaultSize = 256

// modulePixels is the rendered size of each QR module in the SVG. With
// DefaultSize=256 and a typical Version-4 QR (~33 modules + quiet zone),
// each module renders at ~7px which is the smallest size most phone
// cameras reliably resolve. The exact module count depends on the
// payload length; we let the SVG scale freely rather than fixing a
// per-module size, so the viewBox attribute is the source of truth.
const quietZoneModules = 4

// RecoveryLevel maps onto the encoder's error-correction levels: L (~7%),
// M (~15%, default), Q (~25%), H (~30%) -- higher recovery means more
// error correction and a denser pattern. Medium is the encoder's default
// when recovery isn't specified.
const RecoveryLevel = qrc.M

// RenderSVG returns the QR code for payload as an SVG document. The
// returned bytes are a complete SVG (`<?xml ...><svg ...>...</svg>`)
// suitable for serving as image/svg+xml.
//
// size <= 0 falls back to DefaultSize. An empty payload returns an
// error rather than emitting a blank SVG -- the caller should refuse
// to mint an unpairable QR.
func RenderSVG(payload string, size int) ([]byte, error) {
	if payload == "" {
		return nil, errors.New("qr: empty payload")
	}
	if size <= 0 {
		size = DefaultSize
	}
	code, err := qrc.Encode(payload, RecoveryLevel, qrc.Auto)
	if err != nil {
		return nil, fmt.Errorf("qr: new code: %w", err)
	}
	return renderSVGFromBitmap(bitmapOf(code), size)
}

// bitmapOf reads the encoder's module grid as a [][]bool of module
// on/off states, the shape the SVG rasterizer below expects.
//
// The encoder hands back an image.Image rather than a boolean grid, so
// the grid is read one module at a time: a module is dark when its
// colour is the scheme's foreground. The scheme is taken from the
// barcode itself (the encoder may be asked for a custom one) and falls
// back to black, which is what qr.Encode's default scheme uses.
func bitmapOf(code barcode.Barcode) [][]bool {
	bounds := code.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return nil
	}
	// The variable must be the color.Color INTERFACE: color.Black is a
	// color.Gray16 value, and a color.Gray16-typed variable cannot hold
	// ColorScheme().Foreground, which is a color.Color.
	var foreground color.Color = color.Black
	if coloured, ok := code.(barcode.BarcodeColor); ok {
		foreground = coloured.ColorScheme().Foreground
	}
	bitmap := make([][]bool, height)
	for y := 0; y < height; y++ {
		row := make([]bool, width)
		for x := 0; x < width; x++ {
			row[x] = code.At(x, y) == foreground
		}
		bitmap[y] = row
	}
	return bitmap
}

// renderSVGFromBitmap constructs an SVG document from a pre-built QR code.
// The encoder renders to an image rather than to SVG, so we read the
// module grid and rasterize it to an inline SVG <path>. See qr_test.go for
// the rendered-shape assertions.
func renderSVGFromBitmap(bitmap [][]bool, size int) ([]byte, error) {
	// Each row of the bitmap is one side of the QR code; the quiet zone
	// (the white border the encoder's own rasterizers add) is faked
	// here by padding each side with `quietZoneModules` cells of false.
	n := len(bitmap)
	if n == 0 {
		return nil, errors.New("qr: empty code")
	}
	padded := n + 2*quietZoneModules
	cellSize := float64(size) / float64(padded)

	var sb strings.Builder
	fmt.Fprintf(&sb, `<?xml version="1.0" encoding="UTF-8"?>`)
	fmt.Fprintf(&sb, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" width="%d" height="%d">`, size, size, size, size)
	fmt.Fprintf(&sb, `<rect width="100%%" height="100%%" fill="white"/>`)
	fmt.Fprintf(&sb, `<path d="`)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			if !bitmap[y][x] {
				continue
			}
			px := float64(x+quietZoneModules) * cellSize
			py := float64(y+quietZoneModules) * cellSize
			fmt.Fprintf(&sb, `M%.3f %.3fh%.3fv%.3fh%.3fz`, px, py, cellSize, cellSize, -cellSize)
		}
	}
	fmt.Fprintf(&sb, `" fill="black"/>`)
	fmt.Fprintf(&sb, `</svg>`)
	return []byte(sb.String()), nil
}
