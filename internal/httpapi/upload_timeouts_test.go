package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/hashing"
)

func uploadTimeoutServer(t *testing.T, http2 bool) (*httptest.Server, string) {
	t.Helper()
	srv, database, _, _, _, _ := serverWithGuard(t)
	archive := t.TempDir()
	_, err := database.ExecInTx(context.Background(), `INSERT INTO storage_locations
		(name, root_path, tier, read_only, prunable) VALUES (?, ?, 'TIER3_MASTER_ARCHIVE', 0, 0)`, "UploadArchive", archive)
	require.NoError(t, err)
	syncGuardFromDB(t, srv, database)
	srv.cfg().HTTP.ReadTimeoutSecs = 1
	srv.cfg().HTTP.WriteTimeoutSecs = 1

	ts := httptest.NewUnstartedServer(srv.Handler())
	// Short whole-request deadlines reproduce the production 15-second
	// failure without uploading a large file or slowing down the test suite.
	ts.Config.ReadTimeout = 200 * time.Millisecond
	ts.Config.WriteTimeout = 200 * time.Millisecond
	ts.EnableHTTP2 = http2
	if http2 {
		ts.StartTLS()
	} else {
		ts.Start()
	}
	t.Cleanup(ts.Close)
	return ts, archive
}

func uploadTimeoutRequest(t *testing.T, web bool, content []byte) (*http.Request, []byte) {
	t.Helper()
	if web {
		req, _ := createMultipartUploadRequest(t, "vacation.mp4", content, nil)
		body, err := io.ReadAll(req.Body)
		require.NoError(t, err)
		require.NoError(t, req.Body.Close())
		return req, body
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", nil)
	req.Header.Set("X-API-Key", routeTestAgentKey)
	req.Header.Set("X-Filename", "vacation.mp4")
	req.Header.Set("X-Blake3-Hash", uploadTestHash(t, content))
	return req, content
}

// Use real TCP connections: ResponseRecorder cannot reproduce server deadlines.
func TestUploadsOutlastServerTimeouts(t *testing.T) {
	for _, protocol := range []string{"HTTP1", "HTTP2"} {
		for _, route := range []string{"agent", "web"} {
			t.Run(protocol+"/"+route, func(t *testing.T) {
				ts, archive := uploadTimeoutServer(t, protocol == "HTTP2")
				content := bytes.Repeat([]byte("vacation footage"), 1024)
				template, body := uploadTimeoutRequest(t, route == "web", content)
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, ts.URL+template.URL.Path,
					&pacedUploadReader{reader: bytes.NewReader(body)})
				require.NoError(t, err)
				req.Header = template.Header
				req.ContentLength = int64(len(body))
				resp, err := ts.Client().Do(req)
				require.NoError(t, err)
				defer resp.Body.Close() //nolint:errcheck // read-only response
				require.Equal(t, http.StatusCreated, resp.StatusCode)
				if protocol == "HTTP2" {
					require.Equal(t, 2, resp.ProtoMajor)
				}
				var result AgentUploadResponse // shared response fields
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
				require.Equal(t, uploadTestHash(t, content), result.Blake3Hash)
				require.Equal(t, int64(len(content)), result.BytesWritten)
				stored, err := os.ReadFile(filepath.Join(archive, result.RelativePath))
				require.NoError(t, err)
				require.Equal(t, content, stored)
			})
		}
	}
}

type pacedUploadReader struct {
	reader *bytes.Reader
}

func (r *pacedUploadReader) Read(p []byte) (int, error) {
	if r.reader.Len() > 0 {
		time.Sleep(100 * time.Millisecond)
	}
	return r.reader.Read(p[:min(len(p), 1024)])
}

func TestStalledUploadsTimeOutAndRemovePartialFiles(t *testing.T) {
	for _, route := range []string{"agent", "web"} {
		t.Run(route, func(t *testing.T) {
			ts, archive := uploadTimeoutServer(t, false)
			content := bytes.Repeat([]byte("footage"), 1024)
			template, body := uploadTimeoutRequest(t, route == "web", content)
			// A raw connection leaves the body unfinished without blocking a
			// client's body-copy goroutine while it receives the error response.
			conn, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), time.Second)
			require.NoError(t, err)
			defer conn.Close() //nolint:errcheck // test cleanup
			require.NoError(t, conn.SetDeadline(time.Now().Add(5*time.Second)))
			_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: %s\r\nContent-Length: %d\r\n", template.URL.Path, ts.Listener.Addr(), len(body))
			require.NoError(t, err)
			require.NoError(t, template.Header.Write(conn))
			_, err = conn.Write(append([]byte("\r\n"), body[:len(body)/2]...))
			require.NoError(t, err)
			started := time.Now()
			resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
			if err == nil {
				defer resp.Body.Close() //nolint:errcheck // read-only response
				require.Equal(t, http.StatusInternalServerError, resp.StatusCode)
			} else {
				// HTTP/1 may close rather than flush an error while the
				// unfinished request body is being discarded.
				require.True(t, errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF), "server must close before the client's deadline: %v", err)
			}
			require.GreaterOrEqual(t, time.Since(started), 900*time.Millisecond, "request must reach the upload idle deadline")
			require.NoError(t, filepath.WalkDir(archive, func(_ string, entry fs.DirEntry, err error) error {
				require.NoError(t, err)
				require.True(t, entry.IsDir(), "partial upload must be removed")
				return nil
			}))
		})
	}
}

// Processing after the body is read must not inherit its idle deadline.
func TestUploadProcessingOutlastsReadTimeout(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)
	srv.cfg().HTTP.ReadTimeoutSecs = 1
	srv.cfg().HTTP.WriteTimeoutSecs = 1
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w, finishUpload := srv.uploadTimeouts(w, r)
		defer finishUpload()
		_, err := io.Copy(io.Discard, r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		time.Sleep(1200 * time.Millisecond)
		_, _ = w.Write([]byte("stored"))
	}))
	ts.Config.ReadTimeout = 200 * time.Millisecond
	ts.Config.WriteTimeout = 200 * time.Millisecond
	ts.Start()
	defer ts.Close()
	client := ts.Client()
	client.Timeout = 5 * time.Second
	resp, err := client.Post(ts.URL, "application/octet-stream", bytes.NewReader([]byte("footage")))
	require.NoError(t, err)
	defer resp.Body.Close() //nolint:errcheck // read-only response
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, "stored", string(body))
}

func uploadTestHash(t *testing.T, content []byte) string {
	t.Helper()
	hash, err := hashing.FullHash(bytes.NewReader(content))
	require.NoError(t, err)
	return hash
}

func TestUploadEarlyResponseBoundsUnreadBody(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)
	srv.cfg().HTTP.ReadTimeoutSecs = 1
	srv.cfg().HTTP.WriteTimeoutSecs = 1
	done := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(done)
		w, finishUpload := srv.uploadTimeouts(w, r)
		defer finishUpload()
		// A large dedup response can flush during JSON encoding before the
		// handler returns, while the incoming body has not been consumed.
		_, _ = w.Write(bytes.Repeat([]byte("response"), 1024))
	}))
	defer ts.Close()
	conn, err := net.DialTimeout("tcp", ts.Listener.Addr().String(), time.Second)
	require.NoError(t, err)
	defer conn.Close() //nolint:errcheck // test cleanup
	require.NoError(t, conn.SetDeadline(time.Now().Add(4*time.Second)))
	_, err = fmt.Fprint(conn, "POST / HTTP/1.1\r\nHost: localhost\r\nContent-Length: 16\r\n\r\nx")
	require.NoError(t, err)
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err == nil {
		_ = resp.Body.Close()
	} else {
		require.True(t, errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF), "unread body must not hang the response: %v", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler still blocked draining an unread body")
	}
}
