// Package naming is the single owner of filename-stem normalization --
// shared by internal/pipeline (which computes and stores media_nodes.
// filename_stem at insert time) and internal/graph (whose FilenameStemResolver
// both compares stems and needs to know WHY a stem is shorter than the
// filename it came from). A leaf package is required here because
// internal/pipeline already imports internal/graph, so internal/graph
// cannot import back -- see hashing/probe/projectfile/prune for the same
// leaf-package idiom elsewhere in this repo.
package naming

import (
	"regexp"
	"strings"
)

// SuffixKind classifies the suffix Analyze stripped off a filename, if any.
// This is what internal/graph's FilenameStemResolver gates on: an "index"
// suffix asserts "I am a duplicate of some other file," which only makes
// sense if that other (bare) file actually exists; a "role" suffix asserts
// "I am a specific derivation of some other file" and carries no such
// requirement -- collapsing role-suffixed siblings to a shared stem is the
// resolver's whole point.
type SuffixKind int

const (
	// SuffixNone means fileName's stem is the filename itself (after
	// extension/case/whitespace normalization) -- nothing was stripped.
	SuffixNone SuffixKind = iota
	// SuffixIndex means the stripped suffix was a "-N" or "(N)" duplicate
	// index (OS auto-rename, or an unpadded camera/human counter).
	SuffixIndex
	// SuffixRole means the stripped suffix was a "_edit"/"_proxy"/"_vN"/
	// " copy" derivation-role marker.
	SuffixRole
)

// indexSuffixRe matches a single trailing duplicate-index marker: "-N"
// (bounded to 1-2 digits -- see the H3 fix, PR #134/commit bd0490c: an
// unbounded -\d+ also strips a camera's own hyphen-numbered default names,
// Sony's DSC-0001.JPG down to "dsc") or "(N)" (any digit count -- an OS
// duplicate-index suffix like "file (12).jpg" is not camera-generated, so
// there's no equivalent false-positive to guard against by bounding
// digits). Leaving "(N)" unbounded is only safe because what stops the
// over-collapse bug class now is internal/graph's anchor+direction gate
// on SuffixIndex, not a digit bound on either marker -- see
// FilenameStemResolver's doc comment.
var indexSuffixRe = regexp.MustCompile(`(?i)(-\d{1,2}|\(\d+\))$`)

// roleSuffixRe matches a single trailing derivation-role marker.
var roleSuffixRe = regexp.MustCompile(`(?i)(_edit|_proxy|_v\d+| copy)$`)

// Analyze normalizes fileName the same way Stem always has (strip the
// extension at the last '.', lowercase, trim whitespace) and repeatedly
// strips one trailing index-or-role marker at a time -- exactly the old
// versionSuffixRe's "(...)+ $" loop, just split into two alternatives so
// the caller can tell which kind fired. When a filename strips markers of
// both kinds ("photo-2_edit.jpg"), the returned SuffixKind is SuffixIndex:
// index ambiguity (does an anchor exist?) dominates over role ambiguity,
// since index is the one that changes resolver behavior.
//
// Issue #132: this is deliberately NOT a fix to what gets stripped -- the
// stored stem's content is unchanged from before (an unpadded 1-2 digit
// hyphen-numbering scheme still collapses to a shared stem here, exactly
// as it always has). What changed is that callers now get told WHICH kind
// of suffix produced that shorter stem, so internal/graph's
// FilenameStemResolver can require an anchor file (and cap confidence)
// before trusting an index-derived match, without needing a backfill
// migration or a scan-order-dependent commit-time check.
func Analyze(fileName string) (string, SuffixKind) {
	stem := fileName
	if i := strings.LastIndex(stem, "."); i > 0 {
		stem = stem[:i]
	}
	stem = strings.ToLower(strings.TrimSpace(stem))

	kind := SuffixNone
	for {
		if stripped := indexSuffixRe.ReplaceAllString(stem, ""); stripped != stem {
			stem = stripped
			kind = SuffixIndex
			continue
		}
		if stripped := roleSuffixRe.ReplaceAllString(stem, ""); stripped != stem {
			stem = stripped
			if kind == SuffixNone {
				kind = SuffixRole
			}
			continue
		}
		break
	}
	return stem, kind
}

// Stem returns fileName's normalized filename stem -- byte-identical to
// internal/pipeline's former filenameStem, now computed here so
// internal/pipeline and internal/graph share one definition.
func Stem(fileName string) string {
	stem, _ := Analyze(fileName)
	return stem
}

// Kind returns the SuffixKind Analyze classified fileName's stripped
// suffix as (SuffixNone if nothing was stripped).
func Kind(fileName string) SuffixKind {
	_, kind := Analyze(fileName)
	return kind
}
