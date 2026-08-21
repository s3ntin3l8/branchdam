// Package djisrt parses just enough of a DJI drone's ".srt" flight-telemetry
// sidecar to pull out ONE representative GPS point (the first frame's fix).
// It is deliberately NOT a full flight-path/track parser -- issue #229 scopes
// it to a single point, matching the single geotag internal/probe already
// extracts from a still photo's EXIF. If a future issue wants the full
// per-second track (speed, heading, every fix), that is new scope, not an
// extension of this package's contract.
//
// DJI (and several similar consumer drones) write a subtitle-formatted
// sidecar alongside the flight video -- one .srt "cue" roughly per second,
// each cue's text line embedding flight telemetry (GPS, altitude, gimbal,
// camera settings) as plain text rather than as real SRT subtitle content.
// This is never muxed into the video container, so exiftool/ffprobe never
// see it when probing the .MP4 itself -- see internal/pipeline's videoExts,
// which deliberately does NOT include ".srt".
//
// Format assumption (researched, not guessed): current-generation DJI
// firmware (Mavic/Air/Mini class, roughly 2020+) embeds coordinates as
// bracketed key-value pairs within each cue's text, e.g.
//
//	[iso: 400] [shutter: 1/320.0] [fnum: 1.7] [ev: 2.0] [ct: 5695]
//	[latitude: 30.123456] [longitude: -81.123456] [rel_alt: 6.500 abs_alt: -32.309]
//
// (field set/order varies by model and firmware -- e.g. some place
// latitude/longitude before the altitude fields, some include a
// [focal_len: ...] entry, some add a trailing font-tag closer). Older
// Phantom/Inspire-generation firmware instead embeds a single
// "GPS(longitude,latitude,altitude)" triple (that ordering -- longitude
// first -- confirmed against a third-party DJI-SRT-to-GPX converter written
// specifically to consume it). This parser accepts either, and is
// deliberately tolerant of whitespace/case variance around the bracketed
// keys (a space before the colon, e.g. "[latitude : ...]", has been observed
// on some firmware) -- but does not attempt to enumerate every DJI model's
// exact field list, since it only needs the first fix.
package djisrt

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
)

// Point is a single geotagged fix extracted from a .srt sidecar -- one GPS
// point, not a flight path. Latitude/Longitude are signed decimal degrees,
// the same convention as probe.ExifResult.GPSLatitude/GPSLongitude
// (hemisphere-corrected, not an unsigned EXIF magnitude+ref pair).
type Point struct {
	Latitude  float64
	Longitude float64
}

// ErrNoGPSData is returned when no cue in the file carries a GPS point
// recognizable by either supported format (see package doc).
var ErrNoGPSData = errors.New("djisrt: no GPS coordinates found in file")

// latBracketRe/lonBracketRe match the current-generation bracketed
// key-value format, e.g. "[latitude: 30.123456]". Case-insensitive and
// tolerant of a space before the colon -- see package doc.
var (
	latBracketRe = regexp.MustCompile(`(?i)\[\s*latitude\s*:\s*(-?\d+(?:\.\d+)?)\s*\]`)
	lonBracketRe = regexp.MustCompile(`(?i)\[\s*longitude\s*:\s*(-?\d+(?:\.\d+)?)\s*\]`)

	// gpsParenRe matches the older "GPS(longitude,latitude,altitude)"
	// inline format -- note the longitude-first ordering, see package doc.
	gpsParenRe = regexp.MustCompile(`(?i)GPS\(\s*(-?\d+(?:\.\d+)?)\s*,\s*(-?\d+(?:\.\d+)?)\s*,\s*-?\d+(?:\.\d+)?\s*\)`)
)

// ParseFirstPoint reads a DJI .srt sidecar from r and returns the first GPS
// point found in file order. Returns ErrNoGPSData if the file parses (or at
// least reads) fine but no recognizable coordinate ever appears -- a
// distinct, non-fatal outcome from a read error, since a malformed or
// GPS-less .srt is an expected input, not a bug.
//
// r is read line-by-line rather than as one parsed SRT cue at a time: DJI's
// telemetry line is not standard subtitle prose, so there is no benefit to
// a real SRT cue parser here -- a single-pass regex search of the raw text
// is both simpler and more tolerant of the block-formatting variance
// (blank-line placement, a trailing <font> closing tag on its own line,
// CRLF vs LF) observed across DJI models/firmware.
func ParseFirstPoint(r io.Reader) (Point, error) {
	scanner := bufio.NewScanner(r)
	// DJI's real per-cue payload line is short, but some firmware wraps the
	// whole telemetry line (all bracketed fields together) well past
	// bufio.Scanner's 64KiB default -- give it generous headroom rather than
	// letting a long line silently fail the scan.
	buf := make([]byte, 0, 4096)
	scanner.Buffer(buf, 1<<20)

	for scanner.Scan() {
		line := scanner.Text()
		if p, ok := extractPoint(line); ok {
			return p, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return Point{}, fmt.Errorf("djisrt: read: %w", err)
	}
	return Point{}, ErrNoGPSData
}

// extractPoint tries both supported formats against a single line/cue of
// text and reports whether it found a usable point.
func extractPoint(text string) (Point, bool) {
	latM := latBracketRe.FindStringSubmatch(text)
	lonM := lonBracketRe.FindStringSubmatch(text)
	if latM != nil && lonM != nil {
		lat, errLat := strconv.ParseFloat(latM[1], 64)
		lon, errLon := strconv.ParseFloat(lonM[1], 64)
		if errLat == nil && errLon == nil {
			return Point{Latitude: lat, Longitude: lon}, true
		}
	}

	if m := gpsParenRe.FindStringSubmatch(text); m != nil {
		lon, errLon := strconv.ParseFloat(m[1], 64)
		lat, errLat := strconv.ParseFloat(m[2], 64)
		if errLat == nil && errLon == nil {
			return Point{Latitude: lat, Longitude: lon}, true
		}
	}

	return Point{}, false
}
