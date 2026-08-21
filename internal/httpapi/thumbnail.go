package httpapi

import (
	"net/http"
	"os"
	"strconv"
	"time"
)

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
