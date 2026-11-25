package httpapi

import (
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/clock"
)

// uploadTimeouts replaces the ordinary whole-request deadlines with an idle
// deadline around each body read. Large media can take hours to arrive; time
// spent receiving earlier bytes or writing them to storage must not consume the
// next read's budget. Call only after authorization, and defer the returned
// function as a fallback. The returned writer restores ordinary deadlines
// before the first response write so flushing and net/http's unread-body
// drain are bounded again, including on pre-flight dedup and validation errors.
func (s *Server) uploadTimeouts(w http.ResponseWriter, r *http.Request) (http.ResponseWriter, func()) {
	var readTimeout, writeTimeout time.Duration
	if cfg := s.cfg(); cfg != nil {
		readTimeout = time.Duration(cfg.HTTP.ReadTimeoutSecs) * time.Second
		writeTimeout = time.Duration(cfg.HTTP.WriteTimeoutSecs) * time.Second
	}
	rc := http.NewResponseController(w)
	// Like the SSE handler, tolerate writers without deadline support (e.g.
	// httptest.ResponseRecorder). Real HTTP/1 and HTTP/2 writers support it.
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})
	r.Body = &uploadIdleBody{ReadCloser: r.Body, controller: rc, timeout: readTimeout}
	finish := sync.OnceFunc(func() {
		_ = rc.SetReadDeadline(uploadDeadline(readTimeout))
		_ = rc.SetWriteDeadline(uploadDeadline(writeTimeout))
	})
	return &uploadResponseWriter{ResponseWriter: w, finish: finish}, finish
}

func uploadDeadline(timeout time.Duration) time.Time {
	if timeout <= 0 {
		return time.Time{}
	}
	return clock.Real().Add(timeout)
}

type uploadIdleBody struct {
	io.ReadCloser
	controller *http.ResponseController
	timeout    time.Duration
}

func (b *uploadIdleBody) Read(p []byte) (int, error) {
	_ = b.controller.SetReadDeadline(uploadDeadline(b.timeout))
	n, err := b.ReadCloser.Read(p)
	// Do not time out the connection while hashing, syncing storage, or
	// committing the node after the read. The next read gets a fresh budget.
	_ = b.controller.SetReadDeadline(time.Time{})
	return n, err
}

// A response may flush during JSON encoding, before the handler returns. In
// HTTP/1 that flush can first drain an unread request body (e.g. on dedup).
// Restore deadlines before either operation, not only in deferred cleanup.
type uploadResponseWriter struct {
	http.ResponseWriter
	finish func()
}

func (w *uploadResponseWriter) WriteHeader(status int) {
	w.finish()
	w.ResponseWriter.WriteHeader(status)
}

func (w *uploadResponseWriter) Write(p []byte) (int, error) {
	w.finish()
	return w.ResponseWriter.Write(p)
}

func (w *uploadResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }
