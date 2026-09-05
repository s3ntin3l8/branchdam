// Package qr is a thin wrapper around skip2/go-qrcode that emits SVG
// for the /api/v1/companion/pairings/{id}/qr.svg endpoint.
//
// skip2/go-qrcode itself only renders PNG; we read its Bitmap() output
// (a [][]bool where each cell is a module's on/off state) and rasterize
// it to an inline SVG path. SVG (not PNG) so the operator can resize
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
	"strings"

	qrc "github.com/skip2/go-qrcode"
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

// RecoveryLevel maps onto qrc's recovery level constants: L (~7%), M
// (~15%, default), Q (~25%), H (~30%) -- higher recovery means more
// error correction and a denser pattern. Medium is qrc.New's default
// when recovery isn't specified.
const RecoveryLevel = qrc.Medium

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
	if _, err := qrc.New(payload, RecoveryLevel); err != nil {
		return nil, fmt.Errorf("qr: new code: %w", err)
	}
	return renderSVGFromBitmap(payload, size)
}

// renderSVGFromBitmap constructs an SVG document from the QR bitmap.
// The version of skip2/go-qrcode vendored here predates the upstream
// SVG() method, so we read Bitmap() (a [][]bool of module on/off
// states) and rasterize it to an inline SVG <path>. See qr_test.go for
// the rendered-shape assertions.
func renderSVGFromBitmap(payload string, size int) ([]byte, error) {
	code, err := qrc.New(payload, RecoveryLevel)
	if err != nil {
		return nil, err
	}
	bitmap := code.Bitmap()

	// Each row of the bitmap is one side of the QR code; quiet zone
	// (the white border skip2's PNG render adds automatically) is faked
	// here by padding each side with `quietZoneModules` cells of false.
	n := len(bitmap)
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
