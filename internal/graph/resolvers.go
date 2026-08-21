package graph

import (
	"context"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/naming"
	"github.com/s3ntin3l8/branchdam/internal/projectfile"
)

// rawExts are camera-native formats a Tier-2 resolver treats as a possible
// FINAL_EXPORT parent -- a JPEG/MP4 derived from one of these is the export,
// not a sibling raw.
var rawExts = map[string]bool{
	"arw": true, "cr2": true, "cr3": true, "nef": true,
	"raf": true, "dng": true, "orf": true, "rw2": true,
}

var exportExts = map[string]bool{
	"jpg": true, "jpeg": true, "mp4": true, "mov": true,
}

// rawExts, exportExts, and proxyExts must stay pairwise disjoint.
// inferRelationship checks the proxy branch (isProxyExt) before the
// FINAL_EXPORT branch (rawExts/exportExts) with no ordering guard between
// them -- an extension present in both proxyExts and exportExts (or
// rawExts) would silently pick whichever branch happens to be checked
// first, not a considered choice. Relevant for #229: adding an extension
// to proxyExts should not also live in exportExts/rawExts.

// proxyExts is the closed, package-level set of "known proxy" filename
// extensions -- a low-resolution companion file that shares its real
// sibling's filename stem (per internal/naming.Stem, which strips only the
// extension) but carries no useful lineage information of its own. Issue
// #228 seeds this with DJI's (and similar drones') ".lrf" low-res proxy;
// issue #229 -- blocked on this PR -- reuses this exact set verbatim for
// the identical-stem/arbitrary-direction PART of DJI's ".srt"
// flight-telemetry sidecar problem. Kept as its own named, package-level
// var (not inlined into FilenameStemResolver) specifically so extending
// it is a one-line change, not a rewrite of the gating logic.
//
// Caution for #229: ".srt" is not a video and must NOT also be added to
// internal/pipeline's videoExts (it isn't an ffprobe-parseable container
// the way .lrf is) -- adding it here only grants the direction gate.
// Whether PROXY_OF is even the right relationship_type for a telemetry
// sidecar (vs. introducing a new one) is #229's call, not assumed here.
var proxyExts = map[string]bool{
	"lrf": true,
}

func isProxyExt(ext string) bool {
	return proxyExts[strings.ToLower(strings.TrimPrefix(ext, "."))]
}

// inferRelationship picks the relationship_type both Tier-2 resolvers use,
// per the build plan's resolver table: PROXY_OF if the child's filename
// says so, or if the child's extension is a known proxy extension
// (proxyExts, #228) and the parent's isn't; FINAL_EXPORT for raw-to-export
// pairs (both exts checked -- a raw parent linked to a non-export child,
// e.g. a project-file sidecar, is not a final export); DERIVED_FROM
// otherwise. Callers that can propose a candidate in either direction for
// an identical-stem, proxy-extension pair (FilenameStemResolver) are
// responsible for always passing the proxy side as childExt/childFileName
// -- this function itself has no notion of "which side was originally the
// resolver's child."
func inferRelationship(childFileName, childExt, parentExt string) string {
	if strings.Contains(strings.ToLower(childFileName), "_proxy") {
		return "PROXY_OF"
	}
	if isProxyExt(childExt) && !isProxyExt(parentExt) {
		return "PROXY_OF"
	}
	if rawExts[strings.ToLower(parentExt)] && exportExts[strings.ToLower(childExt)] {
		return "FINAL_EXPORT"
	}
	return "DERIVED_FROM"
}

// XMPOriginalDocumentIDResolver matches a child's XMP:OriginalDocumentID
// against a candidate parent's document_id -- Tier 2, confidence 0.95 (the
// build plan's table). This is the strongest deterministic signal short of
// project-file introspection (Tier 1, not yet implemented).
type XMPOriginalDocumentIDResolver struct{}

func (XMPOriginalDocumentIDResolver) Name() string { return "xmp_original_document_id" }
func (XMPOriginalDocumentIDResolver) Tier() int    { return 2 }

func (XMPOriginalDocumentIDResolver) Resolve(ctx context.Context, child Node, lookup Lookup) ([]Candidate, error) {
	if child.OriginalDocumentID == "" {
		return nil, nil
	}
	parents, err := lookup.ByOriginalDocumentID(ctx, child.OriginalDocumentID)
	if err != nil {
		return nil, err
	}

	var candidates []Candidate
	for _, parent := range parents {
		if parent.ID == child.ID {
			continue
		}
		candidates = append(candidates, Candidate{
			ParentID:   parent.ID,
			ChildID:    child.ID,
			Rel:        inferRelationship(child.FileName, child.FileExt, parent.FileExt),
			Confidence: 0.95,
			Tier:       2,
			Resolver:   "xmp_original_document_id",
			Evidence: map[string]any{
				"child_original_document_id": child.OriginalDocumentID,
				"parent_document_id":         parent.DocumentID,
			},
		})
	}
	return candidates, nil
}

// indexMatchConfidenceCap bounds a filename_stem match that involves an
// "index" suffix (naming.SuffixIndex -- "-N"/"(N)") strictly below
// AutoAcceptThresholdForTier(2) (0.90), so such a match can never reach
// AUTO_ACCEPTED on filename alone -- see FilenameStemResolver's doc
// comment and issue #132. mergeCandidates (engine.go) already takes the
// max confidence per (parent, child, rel), so a corroborating
// non-filename_stem resolver (xmp_original_document_id at 0.95,
// project_sidecar at 1.00, heuristic_spatial_temporal up to 0.89 against
// its own 0.85 Tier-3 threshold) still promotes the pair to AUTO_ACCEPTED
// -- no additional merge logic is needed for that.
const indexMatchConfidenceCap = 0.89

// FilenameStemResolver matches candidate parents sharing the child's
// normalized filename stem (computed once at index time by
// internal/naming.Stem, so both sides of the comparison are already
// normalized). Base confidence 0.60, +0.15 same capture day, +0.10 same
// camera_model, +0.10 same directory, clamped to 0.90 -- the build plan's
// table. That clamp is exactly AutoAcceptThresholdForTier(2): a stem match
// with all three boosts DOES reach AUTO_ACCEPTED on filename alone.
//
// Issue #132: a shared stem can come from two very different sources --
// naming.Analyze tells them apart. An "index" suffix ("-N", "(N)")
// asserts "I am a duplicate of some other file," which only makes sense
// if that other, bare file actually exists; a shared stem from stripping
// one is therefore gated on two rules before it is even scored:
//
//  1. Anchor rule: the parent's own filename must classify as
//     naming.SuffixNone -- i.e. the parent IS the bare "photo.jpg" a
//     "photo-2.jpg" child implies. A batch of "trip-1.jpg".."trip-45.jpg"
//     with no "trip.jpg" anchor present emits zero candidates among
//     the batch, closing the O(n^2) auto-accepted mesh PR #134 narrowed
//     but did not close. A SuffixRole parent ("trip_edit.jpg") is
//     deliberately NOT an acceptable anchor for an index child either --
//     it is not the bare file the index suffix implies exists.
//  2. Direction rule: only the child may carry the index suffix. A
//     derived duplicate is never treated as a parent.
//
// A pair that survives both rules is still capped at indexMatchConfidenceCap
// (0.89), so it always lands in the audit queue on filename_stem alone --
// see indexMatchConfidenceCap's doc comment for how corroboration still
// reaches AUTO_ACCEPTED.
//
// A shared stem from stripping a "role" suffix (_edit, _proxy, _vN,
// " copy") is unaffected by any of this: collapsing role-suffixed
// siblings to a shared stem is the resolver's original intended case, not
// something #132 changes.
//
// Issue #228: a shared stem can also come from two bare, identically-named
// files that differ only by extension, where one extension is a "known
// proxy" (proxyExts, e.g. DJI's .lrf) with no content of its own worth
// treating as authoritative. Neither side carries an index suffix here, so
// #132's gate above never fires for this case -- Engine still calls
// Resolve once per node as "child" (see Engine.ResolveAndCommit), so
// without an explicit rule a DJI_0001.MP4/DJI_0001.LRF pair would propose
// a candidate in BOTH directions, each independently scorable up to the
// 0.90 clamp with no principled tiebreak for which one "wins" as parent.
// Resolve forces the proxy side to always be the child (see the
// childNode/parentNode swap below) and inferRelationship labels such a
// pair PROXY_OF rather than the generic DERIVED_FROM.
type FilenameStemResolver struct{}

func (FilenameStemResolver) Name() string { return "filename_stem" }
func (FilenameStemResolver) Tier() int    { return 2 }

func (FilenameStemResolver) Resolve(ctx context.Context, child Node, lookup Lookup) ([]Candidate, error) {
	if child.FilenameStem == "" {
		return nil, nil
	}
	parents, err := lookup.ByFilenameStem(ctx, child.FilenameStem)
	if err != nil {
		return nil, err
	}

	childKind := naming.Kind(child.FileName)

	var candidates []Candidate
	for _, parent := range parents {
		if parent.ID == child.ID {
			continue
		}

		parentKind := naming.Kind(parent.FileName)
		indexGated := childKind == naming.SuffixIndex || parentKind == naming.SuffixIndex
		if indexGated && (childKind != naming.SuffixIndex || parentKind != naming.SuffixNone) {
			// Either side stripped an index suffix, but the pair doesn't
			// satisfy the anchor+direction rules above -- e.g. the parent
			// isn't a bare anchor, or the index-suffixed node is the
			// parent rather than the child. Emit nothing rather than a
			// candidate that would otherwise be scored and possibly
			// auto-accepted.
			continue
		}

		// #228: a bare, identically-stemmed pair where EXACTLY ONE side is
		// a known proxy extension (proxyExts) always resolves with the
		// proxy as the child -- regardless of which node THIS Resolve
		// call is evaluating as "child". (Two proxy-extension siblings
		// sharing a stem, e.g. two .lrf files, are NOT handled by this
		// swap -- isProxyExt is true on both sides so the condition below
		// never fires and they fall through to the resolver's ordinary,
		// direction-ambiguous behavior. Unchanged from before #228, not a
		// regression, just not something this fix covers.) Without this,
		// Engine calls Resolve once
		// per node as "child" (see this type's doc comment on Engine's
		// per-node resolve loop), so a DJI_0001.MP4/DJI_0001.LRF pair --
		// neither side carries an index suffix, so indexGated above never
		// fires for it -- would otherwise propose a candidate in BOTH
		// directions, each scorable up to the 0.90 clamp; worse, whichever
		// direction resolves SECOND (a real scan resolves every committed
		// node in a batch, so both directions do run) would propose the
		// reverse of whatever the first call already committed, and
		// Engine's cycle guard would silently refuse it. childNode/
		// parentNode below are what's actually emitted as ChildID/
		// ParentID and are what evidence below is built from -- NOT the
		// resolver's own pre-swap child/parent pairing, since fields like
		// anchor_file_name are direction-dependent (not every evidence
		// field is symmetric the way the confidence boosts below are).
		//
		// This swap can conflict with #132's own direction invariant
		// (an index-suffixed node may never be the parent): by this point
		// indexGated true guarantees childKind == SuffixIndex and
		// parentKind == SuffixNone (the only combination that survives
		// the gate above), so if THIS swap would additionally promote
		// that already-index-suffixed node (the pre-swap child) into the
		// parent role -- e.g. DJI_0001.LRF (bare anchor, proxy ext) paired
		// with DJI_0001-2.MP4 (index-suffixed, non-proxy real video) --
		// #228 must not silently override #132. Skip emitting any
		// candidate for this pair rather than picking a winner between
		// the two invariants, matching the "emit nothing" pattern used a
		// few lines above for other #132 violations.
		childNode, parentNode := child, parent
		proxySwapped := false
		if isProxyExt(parent.FileExt) && !isProxyExt(child.FileExt) {
			childNode, parentNode = parent, child
			proxySwapped = true
		}
		if proxySwapped && indexGated {
			continue
		}

		confidence := 0.60
		evidence := map[string]any{"filename_stem": childNode.FilenameStem}

		if sameCaptureDay(childNode.CapturedAt, parentNode.CapturedAt) {
			confidence += 0.15
			evidence["same_capture_day"] = true
		}
		if childNode.CameraModel != "" && childNode.CameraModel == parentNode.CameraModel {
			confidence += 0.10
			evidence["same_camera_model"] = childNode.CameraModel
		}
		if sameDirectory(childNode.FilePath, parentNode.FilePath) {
			confidence += 0.10
			evidence["same_directory"] = true
		}
		if confidence > 0.90 {
			confidence = 0.90
		}
		if indexGated {
			evidence["index_suffix_gated"] = true
			evidence["anchor_file_name"] = parentNode.FileName
			if confidence > indexMatchConfidenceCap {
				confidence = indexMatchConfidenceCap
			}
		}
		if proxySwapped {
			evidence["proxy_ext_direction_forced"] = true
		}

		candidates = append(candidates, Candidate{
			ParentID:   parentNode.ID,
			ChildID:    childNode.ID,
			Rel:        inferRelationship(childNode.FileName, childNode.FileExt, parentNode.FileExt),
			Confidence: confidence,
			Tier:       2,
			Resolver:   "filename_stem",
			Evidence:   evidence,
		})
	}
	return candidates, nil
}

// HeuristicSpatialTemporalResolver matches a candidate parent sharing camera
// serial number within a ±2s capture timestamp window (Tier 3, confidence band
// 0.70–0.89). Score starts at 0.70 (serial + time match), +0.10 for lens model
// match, +0.09 for pHash Hamming distance <= 10. Hamming distance > 10 emits no
// candidate. A node with NULL phash on either side caps confidence below 0.80 (0.79).
//
// #162: the underlying signals (serial + ±2s window + Hamming <= 10) can't
// distinguish direction on their own -- a continuous-drive burst produces
// near-identical images and near-identical pHashes, so every pair in the
// window would otherwise score identically regardless of which frame is
// "the parent." Ordering by captured_at_unix doesn't resolve this either:
// that column is truncated to whole seconds (internal/pipeline/commit.go),
// so a RAW+export pair from the same shutter release -- this resolver's
// actual target case -- shares one timestamp, same as same-second burst
// frames. Instead, a candidate is only emitted when the parent is a
// camera-native raw format and the child is an export format (rawExts/
// exportExts, the same maps inferRelationship already uses for
// FINAL_EXPORT): direction becomes a function of file role, not resolution
// order, and a same-format burst (all raw, or all export) now produces
// zero edges. Because a fast burst's ±2s window can still contain several
// raw candidates for one export child (or vice versa isn't possible: an
// export is never a parent under this rule), only the single best-scoring
// match is returned -- highest confidence, tie-broken by lowest Hamming
// distance, then smallest time delta, then lowest parent node ID -- so a
// mixed-format burst can't still produce a small cross-linked mesh. The
// final ID tie-break matters in practice, not just in theory: a burst's raw
// siblings can share identical lens, identical (bit-for-bit, after DCT
// quantization) pHash, and -- per the captured_at_unix truncation above --
// identical time delta too, so confidence/Hamming/deltaSec alone are not
// always a total order. Without a final deterministic key, that case would
// fall back to whatever order lookup.BySpatialTemporal's underlying SQL
// happens to return rows in (ListTier3Candidates has no ORDER BY) --
// reintroducing this issue's own "which frame wins is an accident of
// resolution/scan order" problem one layer down, in the tie-break instead
// of in Resolve's caller. Lowest parent ID is an arbitrary but explicit and
// stable choice, not a claim that an older node ID is a more likely parent.
type HeuristicSpatialTemporalResolver struct{}

func (HeuristicSpatialTemporalResolver) Name() string { return "heuristic_spatial_temporal" }
func (HeuristicSpatialTemporalResolver) Tier() int    { return 3 }

func (HeuristicSpatialTemporalResolver) Resolve(ctx context.Context, child Node, lookup Lookup) ([]Candidate, error) {
	if child.CameraSerial == "" || child.CapturedAt == nil {
		return nil, nil
	}

	parents, err := lookup.BySpatialTemporal(ctx, child.CameraSerial, *child.CapturedAt, 2*time.Second, child.ID)
	if err != nil {
		return nil, err
	}

	var best *Candidate
	bestHamming := math.MaxInt
	bestDeltaSec := math.MaxFloat64
	for _, parent := range parents {
		if parent.ID == child.ID || parent.CapturedAt == nil {
			continue
		}
		if !rawExts[strings.ToLower(parent.FileExt)] || !exportExts[strings.ToLower(child.FileExt)] {
			continue
		}

		confidence := 0.70
		deltaSec := math.Abs(child.CapturedAt.Sub(*parent.CapturedAt).Seconds())
		evidence := map[string]any{
			"camera_serial":         child.CameraSerial,
			"capture_delta_seconds": deltaSec,
		}

		if child.LensModel != "" && child.LensModel == parent.LensModel {
			confidence += 0.10
			evidence["lens_model"] = child.LensModel
		}

		hamming := math.MaxInt
		if child.PHash != nil && parent.PHash != nil {
			dist := hashing.HammingDistance(*child.PHash, *parent.PHash)
			evidence["hamming_distance"] = dist
			if dist > 10 {
				continue
			}
			hamming = dist
			confidence += 0.09
		} else {
			evidence["phash_missing"] = true
			if confidence > 0.79 {
				confidence = 0.79
			}
		}

		confidence = math.Round(confidence*100) / 100
		if confidence > 0.89 {
			confidence = 0.89
		}

		candidate := Candidate{
			ParentID:   parent.ID,
			ChildID:    child.ID,
			Rel:        inferRelationship(child.FileName, child.FileExt, parent.FileExt),
			Confidence: confidence,
			Tier:       3,
			Resolver:   "heuristic_spatial_temporal",
			Evidence:   evidence,
		}

		betterThanBest := best == nil ||
			confidence > best.Confidence ||
			(confidence == best.Confidence && hamming < bestHamming) ||
			(confidence == best.Confidence && hamming == bestHamming && deltaSec < bestDeltaSec) ||
			(confidence == best.Confidence && hamming == bestHamming && deltaSec == bestDeltaSec && parent.ID < best.ParentID)
		if betterThanBest {
			best = &candidate
			bestHamming = hamming
			bestDeltaSec = deltaSec
		}
	}
	if best == nil {
		return nil, nil
	}
	return []Candidate{*best}, nil
}

func sameCaptureDay(a, b *time.Time) bool {
	if a == nil || b == nil {
		return false
	}
	ay, am, ad := a.Date()
	by, bm, bd := b.Date()
	return ay == by && am == bm && ad == bd
}

func sameDirectory(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	return filepath.Dir(a) == filepath.Dir(b)
}

// ProjectSidecarResolver parses project sidecar files (.dam.json, .drp, .fcpxml, .edl)
// and emits Tier-1 PROJECT_SIDECAR edges at confidence 1.00.
type ProjectSidecarResolver struct {
	rewrites []config.PathRewrite
}

// NewProjectSidecarResolver creates a new Tier-1 ProjectSidecarResolver.
func NewProjectSidecarResolver(rewrites []config.PathRewrite) *ProjectSidecarResolver {
	return &ProjectSidecarResolver{rewrites: rewrites}
}

func (r *ProjectSidecarResolver) Name() string { return "project_sidecar" }
func (r *ProjectSidecarResolver) Tier() int    { return 1 }

func (r *ProjectSidecarResolver) Resolve(ctx context.Context, child Node, lookup Lookup) ([]Candidate, error) {
	parser, ok := projectfile.GetParser(child.FilePath)
	if !ok {
		return nil, nil
	}

	f, err := os.Open(child.FilePath)
	if err != nil {
		slog.Warn("project_sidecar resolver unable to open file", "path", child.FilePath, "err", err)
		return nil, nil
	}
	defer func() { _ = f.Close() }()

	refs, err := parser.Parse(ctx, f)
	if err != nil {
		slog.Warn("project_sidecar resolver failed to parse project file", "path", child.FilePath, "err", err)
		return nil, nil
	}

	var candidates []Candidate
	seenParents := make(map[int64]bool)

	for _, ref := range refs {
		if ref.RawPath == "" {
			continue
		}

		targetNode := r.resolveReference(ctx, ref.RawPath, child.FilePath, lookup)
		if targetNode == nil || targetNode.ID == child.ID || seenParents[targetNode.ID] {
			continue
		}

		seenParents[targetNode.ID] = true
		candidates = append(candidates, Candidate{
			ParentID:   targetNode.ID,
			ChildID:    child.ID,
			Rel:        "PROJECT_SIDECAR",
			Confidence: 1.00,
			Tier:       1,
			Resolver:   "project_sidecar",
			Evidence: map[string]any{
				"project_file_path": child.FilePath,
				"raw_reference":     ref.RawPath,
				"role":              ref.Role,
			},
		})
	}

	return candidates, nil
}

func (r *ProjectSidecarResolver) resolveReference(ctx context.Context, rawPath, childPath string, lookup Lookup) *Node {
	// 1. Try exact container path match
	if node, err := lookup.ByPath(ctx, rawPath); err == nil && node != nil {
		return node
	}

	// 2. Try operator prefix rewrites
	rewritten := applyPathRewrite(rawPath, r.rewrites)
	if rewritten != rawPath {
		if node, err := lookup.ByPath(ctx, rewritten); err == nil && node != nil {
			return node
		}
	}

	// 3. Fallback matching by filename (basename)
	base := filepath.Base(strings.ReplaceAll(rawPath, "\\", "/"))
	if base != "" && base != "." && base != "/" {
		nodes, err := lookup.ByFileName(ctx, base)
		if err == nil {
			if len(nodes) == 1 {
				return &nodes[0]
			}
			if len(nodes) > 1 {
				slog.Warn("project_sidecar ambiguous reference match skipped",
					"project_file", childPath,
					"raw_reference", rawPath,
					"candidate_count", len(nodes),
				)
			}
		}
	}

	return nil
}

func applyPathRewrite(rawPath string, rewrites []config.PathRewrite) string {
	normRaw := strings.ReplaceAll(rawPath, "\\", "/")
	for _, rw := range rewrites {
		normFrom := strings.ReplaceAll(rw.From, "\\", "/")
		if normFrom == "" {
			continue
		}
		if strings.HasPrefix(strings.ToLower(normRaw), strings.ToLower(normFrom)) {
			rel := normRaw[len(normFrom):]
			to := strings.ReplaceAll(rw.To, "\\", "/")
			if !strings.HasSuffix(to, "/") && !strings.HasPrefix(rel, "/") {
				to += "/"
			}
			return filepath.Clean(to + rel)
		}
	}
	return filepath.Clean(normRaw)
}
