package httpapi

import (
	"errors"
	"io/fs"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// handleAssetStream serves an asset file with full HTTP Range request support
// (RFC 7233 byte ranges), allowing browser HTML5 video seeking, progressive streaming,
// and full-resolution image inspection. Gated identically to GET /thumbnail: any
// authenticated browser principal can stream it.
func (s *Server) handleAssetStream(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if s.guard == nil {
		http.NotFound(w, r)
		return
	}

	node, err := s.db.Reader.GetMediaNodeByID(r.Context(), id)
	if err != nil || node.LifecycleState == "ARCHIVED" || node.LifecycleState == "MISSING" {
		http.NotFound(w, r)
		return
	}

	f, err := s.guard.OpenRead(node.FilePath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			http.NotFound(w, r)
			return
		}
		s.log.Warn("stream open failed", "nodeID", node.ID, "path", node.FilePath, "err", err)
		http.Error(w, "failed to open media asset", http.StatusInternalServerError)
		return
	}
	defer func() { _ = f.Close() }()

	contentType := mime.TypeByExtension("." + strings.ToLower(node.FileExt))
	if contentType == "" {
		contentType = "application/octet-stream"
	}

	w.Header().Set("Content-Type", contentType)
	w.Header().Set("ETag", `"`+node.NodeUuid+"-"+strconv.FormatInt(node.UpdatedAt, 10)+`"`)
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("Accept-Ranges", "bytes")

	http.ServeContent(w, r, node.FileName, time.Unix(node.UpdatedAt, 0), f)
}
