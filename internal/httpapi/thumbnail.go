package httpapi

import (
	"context"
	"errors"
	"io/fs"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// thumbnailInvalidateTimeout bounds the self-heal write handleThumbnail
// issues on a read miss (see below) -- short because it's a single-row
// UPDATE on the writer's sole connection, not a batch operation.
const thumbnailInvalidateTimeout = 5 * time.Second

// handleThumbnail serves a node's cached JPEG thumbnail. Registered
// directly on the mux (see server.go), not through Huma, since a raw
// image/jpeg byte stream doesn't fit Huma's JSON-body response model --
// same reasoning as the SSE handler. Not gated by auth.RequireAdmin (that
// gates mutating methods only), so any authenticated browser principal can
// read it, same as every other GET under /api/v1.
func (s *Server) handleThumbnail(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.thumbs == nil {
		http.NotFound(w, r)
		return
	}

	node, err := s.db.Reader.GetMediaNodeByID(r.Context(), id)
	if err != nil || node.ThumbState != "READY" {
		http.NotFound(w, r)
		return
	}

	f, err := os.Open(s.thumbs.Path(node.NodeUuid))
	if err != nil {
		// thumb_state says READY but the cached JPEG isn't on disk. Only
		// self-heal when the file is genuinely gone (fs.ErrNotExist) --
		// e.g. branchdam.db was restored from backup without /data/thumbs
		// alongside it (docs/operations.md's backup guidance covers the
		// whole /data volume, but a partial restore is an easy mistake).
		// A transient EACCES/EMFILE/EIO is not evidence the cached file is
		// missing and must not trigger a reset -> regenerate storm across
		// the library; same "gone on its own is not an error, anything
		// else is" distinction internal/prune.Execute draws around
		// os.Lstat (see AGENTS.md).
		//
		// Also gated on lifecycle_state IN ('ACTIVE', 'HIDDEN') to match
		// ListPendingThumbnails' own claim-query filter exactly
		// (internal/db/queries/media_nodes.sql): an ARCHIVED/MISSING node
		// invalidated here would never be reclaimed by
		// internal/thumbs.Worker.ProcessPending, so it would land on
		// thumb_state='PENDING' and 404 forever instead of self-healing --
		// worse than leaving it on its stale READY. There's no live file to
		// regenerate a thumbnail from for those nodes anyway, so leaving
		// thumb_state alone is correct, not merely a smaller bug.
		if errors.Is(err, fs.ErrNotExist) && (node.LifecycleState == "ACTIVE" || node.LifecycleState == "HIDDEN") {
			// Use a detached context with its own short timeout rather than
			// r.Context(): a browser aborting an in-flight thumbnail GET
			// (routine on a scrolled/lazy-loaded asset grid) must not
			// cancel the heal along with the response -- it should still
			// land so the node reaches PENDING on this first miss rather
			// than needing a second request to retry it.
			ictx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), thumbnailInvalidateTimeout)
			defer cancel()
			// Resets thumb_state back to PENDING via the same
			// InvalidateThumbnail query refreshNodeAfterInPlaceWrite uses
			// (see routes.go), so internal/thumbs.Worker.ProcessPending
			// reclaims and regenerates it on its next pass instead of
			// leaving it stuck READY forever with no automatic recovery.
			if invalidateErr := s.db.InTx(ictx, func(q *sqlcgen.Queries) error {
				return q.InvalidateThumbnail(ictx, node.ID)
			}); invalidateErr != nil {
				s.log.Warn("thumbnail read miss: invalidate thumb_state failed", "nodeID", node.ID, "err", invalidateErr)
			}
		}
		http.NotFound(w, r)
		return
	}
	defer func() { _ = f.Close() }()

	// ETag keys off node_uuid + updated_at rather than a file hash: cheap
	// (no re-read), and InvalidateThumbnail/SetThumbState both bump
	// updated_at whenever the cached JPEG could have changed, so a stale
	// client-cached copy is never served past the next thumb_state write.
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("ETag", `"`+node.NodeUuid+"-"+strconv.FormatInt(node.UpdatedAt, 10)+`"`)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	http.ServeContent(w, r, "", time.Unix(node.UpdatedAt, 0), f)
}
