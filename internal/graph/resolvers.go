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

// inferRelationship picks the relationship_type both Tier-2 resolvers use,
// per the build plan's resolver table: PROXY_OF if the child's filename
// says so, FINAL_EXPORT for raw-to-export pairs (both exts checked -- a raw
// parent linked to a non-export child, e.g. a project-file sidecar, is not
// a final export), DERIVED_FROM otherwise.
func inferRelationship(childFileName, childExt, parentExt string) string {
	if strings.Contains(strings.ToLower(childFileName), "_proxy") {
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

// FilenameStemResolver matches candidate parents sharing the child's
// normalized filename stem (computed once at index time by
// internal/pipeline's filenameStem, so both sides of the comparison are
// already normalized). Base confidence 0.60, +0.15 same capture day, +0.10
// same camera_model, +0.10 same directory, clamped to 0.90 -- the build
// plan's table. Never reaches AUTO_ACCEPTED's 0.90 threshold on filename
// alone; it always lands in the audit queue unless corroborated by at
// least two of the three boosts.
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

	var candidates []Candidate
	for _, parent := range parents {
		if parent.ID == child.ID {
			continue
		}

		confidence := 0.60
		evidence := map[string]any{"filename_stem": child.FilenameStem}

		if sameCaptureDay(child.CapturedAt, parent.CapturedAt) {
			confidence += 0.15
			evidence["same_capture_day"] = true
		}
		if child.CameraModel != "" && child.CameraModel == parent.CameraModel {
			confidence += 0.10
			evidence["same_camera_model"] = child.CameraModel
		}
		if sameDirectory(child.FilePath, parent.FilePath) {
			confidence += 0.10
			evidence["same_directory"] = true
		}
		if confidence > 0.90 {
			confidence = 0.90
		}

		candidates = append(candidates, Candidate{
			ParentID:   parent.ID,
			ChildID:    child.ID,
			Rel:        inferRelationship(child.FileName, child.FileExt, parent.FileExt),
			Confidence: confidence,
			Tier:       2,
			Resolver:   "filename_stem",
			Evidence:   evidence,
		})
	}
	return candidates, nil
}

// HeuristicSpatialTemporalResolver matches candidate parents sharing camera
// serial number within a ±2s capture timestamp window (Tier 3, confidence band
// 0.70–0.89). Score starts at 0.70 (serial + time match), +0.10 for lens model
// match, +0.09 for pHash Hamming distance <= 10. Hamming distance > 10 emits no
// candidate. A node with NULL phash on either side caps confidence below 0.80 (0.79).
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

	var candidates []Candidate
	for _, parent := range parents {
		if parent.ID == child.ID || parent.CapturedAt == nil {
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

		if child.PHash != nil && parent.PHash != nil {
			dist := hashing.HammingDistance(*child.PHash, *parent.PHash)
			evidence["hamming_distance"] = dist
			if dist > 10 {
				continue
			}
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

		candidates = append(candidates, Candidate{
			ParentID:   parent.ID,
			ChildID:    child.ID,
			Rel:        inferRelationship(child.FileName, child.FileExt, parent.FileExt),
			Confidence: confidence,
			Tier:       3,
			Resolver:   "heuristic_spatial_temporal",
			Evidence:   evidence,
		})
	}
	return candidates, nil
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
