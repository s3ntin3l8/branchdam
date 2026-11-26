package httpapi

import (
	"container/heap"
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
)

const (
	// SimilarDefaultLimit and SimilarMaxLimit are the page size for
	// GET /api/v1/assets/{id}/similar. The huma tags on
	// SimilarAssetsInput repeat these numbers; TestSimilarBoundsMatchHandlerTags
	// fails if they drift. A struct tag cannot reference a const.
	SimilarDefaultLimit = 20
	SimilarMaxLimit     = 100
	// SimilarDefaultDistance and SimilarMaxDistance bound the Hamming
	// filter. 0 is a real value (identical hashes only), so the HTTP
	// default has to live in the tag, not in a zero-value check.
	SimilarDefaultDistance = 12
	SimilarMaxDistance     = 64

	noPerceptualHashReason = "no_perceptual_hash"
)

// SimilarAssetsInput is GET /api/v1/assets/{id}/similar.
type SimilarAssetsInput struct {
	ID          int64 `path:"id"`
	Limit       int64 `query:"limit" default:"20" minimum:"1" maximum:"100"`
	MaxDistance int64 `query:"maxDistance" default:"12" minimum:"0" maximum:"64"`
}

type similarAssetDTO struct {
	ID         int64  `json:"id"`
	NodeUUID   string `json:"nodeUuid"`
	FilePath   string `json:"filePath"`
	FileName   string `json:"fileName"`
	ThumbState string `json:"thumbState"`
	Distance   int    `json:"distance"`
}

type SimilarAssetsOutput struct {
	Body struct {
		Assets []similarAssetDTO `json:"assets"`
		Reason string            `json:"reason,omitempty"`
	}
}

func (s *Server) handleListSimilar(ctx context.Context, in *SimilarAssetsInput) (*SimilarAssetsOutput, error) {
	node, err := s.db.Reader.GetMediaNodeByID(ctx, in.ID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, huma.Error404NotFound("asset not found")
	}
	if err != nil {
		return nil, huma.Error500InternalServerError("get asset", err)
	}
	// Same as the other /assets/{id} sub-resources: an ARCHIVED node is
	// not a live asset to search from.
	if node.LifecycleState == "ARCHIVED" {
		return nil, huma.Error404NotFound("asset not found")
	}
	out := &SimilarAssetsOutput{}
	out.Body.Assets = []similarAssetDTO{}
	if !node.Phash.Valid {
		out.Body.Reason = noPerceptualHashReason
		return out, nil
	}
	best := &similarHeap{}
	heap.Init(best)
	err = s.db.ForEachLivePhash(ctx, func(row db.LivePhash) error {
		if row.ID == node.ID {
			return nil
		}
		dist := hashing.HammingDistance(node.Phash.Int64, row.Phash)
		if dist > int(in.MaxDistance) {
			return nil
		}
		considerSimilar(best, similarAssetDTO{
			ID:         row.ID,
			NodeUUID:   row.NodeUUID,
			FilePath:   row.FilePath,
			FileName:   row.FileName,
			ThumbState: row.ThumbState,
			Distance:   dist,
		}, int(in.Limit))
		return nil
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("list similar", err)
	}
	out.Body.Assets = best.sorted()
	return out, nil
}

// considerSimilar keeps at most limit nearest candidates. The heap's
// root is the worst of those, so a closer row replaces it.
func considerSimilar(h *similarHeap, row similarAssetDTO, limit int) {
	if limit < 1 {
		return
	}
	if h.Len() < limit {
		heap.Push(h, row)
		return
	}
	worst := (*h)[0]
	if row.Distance < worst.Distance || (row.Distance == worst.Distance && row.ID < worst.ID) {
		heap.Pop(h)
		heap.Push(h, row)
	}
}

// similarHeap is a max-heap of the nearest matches: higher distance,
// then higher id, is worse and sits at the root.
type similarHeap []similarAssetDTO

func (h similarHeap) Len() int { return len(h) }
func (h similarHeap) Less(i, j int) bool {
	if h[i].Distance != h[j].Distance {
		return h[i].Distance > h[j].Distance
	}
	return h[i].ID > h[j].ID
}
func (h similarHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h *similarHeap) Push(x any)   { *h = append(*h, x.(similarAssetDTO)) }
func (h *similarHeap) Pop() any {
	old := *h
	n := len(old)
	item := old[n-1]
	*h = old[:n-1]
	return item
}

func (h similarHeap) sorted() []similarAssetDTO {
	out := append([]similarAssetDTO(nil), h...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Distance != out[j].Distance {
			return out[i].Distance < out[j].Distance
		}
		return out[i].ID < out[j].ID
	})
	if out == nil {
		return []similarAssetDTO{}
	}
	return out
}
