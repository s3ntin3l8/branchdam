package graph

import (
	"context"
	"database/sql"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// querier is the subset of sqlcgen.Querier lookup.go needs -- narrowed so
// this package doesn't need the full generated interface just to read two
// query results.
type querier interface {
	ListLiveNodesByDocumentID(ctx context.Context, documentID sql.NullString) ([]sqlcgen.MediaNode, error)
	ListLiveNodesByFilenameStem(ctx context.Context, filenameStem sql.NullString) ([]sqlcgen.MediaNode, error)
}

// dbLookup implements Lookup against a real *sqlcgen.Queries (i.e.
// *db.DB.Reader).
type dbLookup struct {
	q querier
}

// NewLookup wraps reader (a *db.DB's Reader field) as a Lookup.
func NewLookup(reader querier) Lookup {
	return &dbLookup{q: reader}
}

func (l *dbLookup) ByOriginalDocumentID(ctx context.Context, documentID string) ([]Node, error) {
	rows, err := l.q.ListLiveNodesByDocumentID(ctx, sql.NullString{String: documentID, Valid: documentID != ""})
	if err != nil {
		return nil, err
	}
	return toNodes(rows), nil
}

func (l *dbLookup) ByFilenameStem(ctx context.Context, stem string) ([]Node, error) {
	rows, err := l.q.ListLiveNodesByFilenameStem(ctx, sql.NullString{String: stem, Valid: stem != ""})
	if err != nil {
		return nil, err
	}
	return toNodes(rows), nil
}

func toNodes(rows []sqlcgen.MediaNode) []Node {
	out := make([]Node, len(rows))
	for i, r := range rows {
		n := Node{
			ID:       r.ID,
			FilePath: r.FilePath,
			FileName: r.FileName,
			FileExt:  r.FileExt,
		}
		if r.OriginalDocumentID.Valid {
			n.OriginalDocumentID = r.OriginalDocumentID.String
		}
		if r.DocumentID.Valid {
			n.DocumentID = r.DocumentID.String
		}
		if r.CameraModel.Valid {
			n.CameraModel = r.CameraModel.String
		}
		if r.FilenameStem.Valid {
			n.FilenameStem = r.FilenameStem.String
		}
		if r.CapturedAtUnix.Valid {
			t := time.Unix(r.CapturedAtUnix.Int64, 0).UTC()
			n.CapturedAt = &t
		}
		out[i] = n
	}
	return out
}
