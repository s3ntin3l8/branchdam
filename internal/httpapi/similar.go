package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"sort"

	"github.com/danielgtaylor/huma/v2"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
)

const noPerceptualHashReason = "no_perceptual_hash"

// SimilarAssetsInput is GET /api/v1/assets/{id}/similar.
// Limit defaults to 20. MaxDistance defaults to 12; 0 means identical
// hashes only.
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
	out := &SimilarAssetsOutput{}
	out.Body.Assets = []similarAssetDTO{}
	if !node.Phash.Valid {
		out.Body.Reason = noPerceptualHashReason
		return out, nil
	}
	rows, err := s.db.Reader.ListLivePhashes(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("list similar", err)
	}
	out.Body.Assets = rankSimilar(node.ID, node.Phash.Int64, rows, int(in.MaxDistance), int(in.Limit))
	return out, nil
}

// rankSimilar keeps other live nodes within maxDistance of sourceHash,
// nearest first. Equal distances break ties by ascending id.
func rankSimilar(sourceID, sourceHash int64, rows []sqlcgen.ListLivePhashesRow, maxDistance, limit int) []similarAssetDTO {
	hits := make([]similarAssetDTO, 0)
	for _, row := range rows {
		if !row.Phash.Valid || row.ID == sourceID {
			continue
		}
		dist := hashing.HammingDistance(sourceHash, row.Phash.Int64)
		if dist > maxDistance {
			continue
		}
		hits = append(hits, similarAssetDTO{
			ID:         row.ID,
			NodeUUID:   row.NodeUuid,
			FilePath:   row.FilePath,
			FileName:   row.FileName,
			ThumbState: row.ThumbState,
			Distance:   dist,
		})
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Distance != hits[j].Distance {
			return hits[i].Distance < hits[j].Distance
		}
		return hits[i].ID < hits[j].ID
	})
	if limit < len(hits) {
		hits = hits[:limit]
	}
	return hits
}
