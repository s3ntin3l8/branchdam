package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/apierr"
	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// agentProblem is the agent API's error envelope as a client sees it.
type agentProblem struct {
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
	Code   string `json:"code"`
	// Error is the legacy upload field, kept alongside the envelope.
	Error string `json:"error"`
}

// requireAgentProblem asserts rec is a problem+json response with the
// given HTTP status and stable code, and returns the decoded body.
func requireAgentProblem(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) agentProblem {
	t.Helper()
	require.Equal(t, status, rec.Code, "body: %s", rec.Body.String())
	assert.Equal(t, "application/problem+json", rec.Header().Get("Content-Type"))
	var p agentProblem
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &p), "body: %s", rec.Body.String())
	assert.Equal(t, code, p.Code, "body: %s", rec.Body.String())
	assert.Equal(t, status, p.Status)
	assert.Equal(t, http.StatusText(status), p.Title)
	return p
}

func agentRequest(t *testing.T, h http.Handler, method, path, key string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var r *http.Request
	if body == nil {
		r = httptest.NewRequest(method, path, nil)
	} else {
		var raw []byte
		if s, ok := body.(string); ok {
			raw = []byte(s)
		} else {
			var err error
			raw, err = json.Marshal(body)
			require.NoError(t, err)
		}
		r = httptest.NewRequest(method, path, bytes.NewReader(raw))
		r.Header.Set("Content-Type", "application/json")
	}
	if key != "" {
		r.Header.Set("X-API-Key", key)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, r)
	return rec
}

// errorCodeServer has a writable staging location, a read-only non-Tier-3
// location and a Tier-3 archive (read-only when archiveReadOnly; uploads
// need it writable), and authenticates
// routeTestAgentKey as "test-device".
func errorCodeServer(t *testing.T, archiveReadOnly bool) (http.Handler, *db.DB, string, string, string) {
	t.Helper()
	root := t.TempDir()
	dirs := map[string]string{}
	for _, name := range []string{"staging", "readonly", "archive"} {
		d := filepath.Join(root, name)
		require.NoError(t, os.MkdirAll(d, 0o755))
		res, err := filepath.EvalSymlinks(d)
		require.NoError(t, err)
		dirs[name] = res
	}
	database, err := db.Open(context.Background(), filepath.Join(root, "codes.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()
	var locs []storage.Location
	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, spec := range []struct {
			name, tier string
			ro         bool
		}{
			{"staging", "TIER0_LOCAL_STAGING", false},
			{"readonly", "TIER2_EXPORTS", true},
			{"archive", "TIER3_MASTER_ARCHIVE", archiveReadOnly},
		} {
			ro := int64(0)
			if spec.ro {
				ro = 1
			}
			loc, err := q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
				Name: spec.name, RootPath: dirs[spec.name], Tier: spec.tier, ReadOnly: ro,
			})
			if err != nil {
				return err
			}
			locs = append(locs, storage.Location{ID: loc.ID, Name: spec.name, RootPath: dirs[spec.name], Tier: spec.tier, ReadOnly: spec.ro})
		}
		return nil
	}))
	srv := New(Deps{
		Config:         &config.Config{Agent: config.Agent{}},
		DB:             database,
		Guard:          storage.NewGuard(locs),
		Version:        "test",
		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey),
	})
	return srv.Handler(), database, dirs["staging"], dirs["readonly"], dirs["archive"]
}

func seedAgentNode(t *testing.T, database *db.DB, nodeUUID, path, lifecycle string, fullHash *string, owner string) {
	t.Helper()
	ctx := context.Background()
	require.NoError(t, database.InTx(ctx, func(q *sqlcgen.Queries) error {
		status := "INDEXED_SHALLOW"
		if fullHash != nil {
			status = "INDEXED_FULL"
		}
		if _, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: nodeUUID, StorageLocationID: 1, FilePath: path, FileName: filepath.Base(path),
			FullHash: fullHash, IndexingStatus: status, GraphStatus: "UNLINKED", LifecycleState: lifecycle,
		}); err != nil {
			return err
		}
		if owner == "" {
			return nil
		}
		return q.SetNodeCreator(ctx, sqlcgen.SetNodeCreatorParams{NodeUuid: nodeUUID, AgentID: owner})
	}))
}

// TestAgentErrorCodes_AuthMiddleware: every rejection by the agent auth
// chain is problem+json with a stable code, at the same HTTP status the
// earlier text/plain responses used.
func TestAgentErrorCodes_AuthMiddleware(t *testing.T) {
	t.Run("AUTH_NOT_CONFIGURED", func(t *testing.T) {
		database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "nolookup.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = database.Close() })
		h := New(Deps{Config: &config.Config{Agent: config.Agent{}}, DB: database, Version: "test"}).Handler()
		rec := agentRequest(t, h, http.MethodPost, "/api/v1/agent/hello", routeTestAgentKey, nil)
		p := requireAgentProblem(t, rec, http.StatusServiceUnavailable, apierr.CodeAuthNotConfigured)
		assert.Equal(t, "agent authentication is not configured", p.Detail)
	})

	h, _, _, _, _ := errorCodeServer(t, true)
	t.Run("AUTH_INVALID_KEY missing", func(t *testing.T) {
		rec := agentRequest(t, h, http.MethodPost, "/api/v1/agent/hello", "", nil)
		p := requireAgentProblem(t, rec, http.StatusUnauthorized, apierr.CodeAuthInvalidKey)
		assert.Equal(t, "invalid or missing X-API-Key", p.Detail)
	})
	t.Run("AUTH_INVALID_KEY unknown", func(t *testing.T) {
		rec := agentRequest(t, h, http.MethodPost, "/api/v1/agent/upload", "not-a-paired-key", nil)
		requireAgentProblem(t, rec, http.StatusUnauthorized, apierr.CodeAuthInvalidKey)
	})

	t.Run("INTERNAL_ERROR key lookup failure", func(t *testing.T) {
		database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "lookupfail.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = database.Close() })
		failing := func(context.Context, string) (auth.KeyLookupResult, error) {
			return auth.KeyLookupResult{}, errors.New("db down")
		}
		h := New(Deps{Config: &config.Config{Agent: config.Agent{}}, DB: database, Version: "test", agentKeyLookup: failing}).Handler()
		rec := agentRequest(t, h, http.MethodPost, "/api/v1/agent/hello", routeTestAgentKey, nil)
		requireAgentProblem(t, rec, http.StatusInternalServerError, apierr.CodeInternal)
	})

	signed := func(t *testing.T, maxBody int64) http.Handler {
		t.Helper()
		database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "signed.db"))
		require.NoError(t, err)
		t.Cleanup(func() { _ = database.Close() })
		cfg := &config.Config{Agent: config.Agent{SignedRequests: true, ReplayWindowSecs: 300, SignedMaxBodyBytes: maxBody}}
		return New(Deps{Config: cfg, DB: database, Version: "test", agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)}).Handler()
	}
	t.Run("AUTH_INVALID_SIGNATURE", func(t *testing.T) {
		rec := agentRequest(t, signed(t, 0), http.MethodPost, "/api/v1/agent/hello", routeTestAgentKey, nil)
		p := requireAgentProblem(t, rec, http.StatusUnauthorized, apierr.CodeAuthInvalidSignature)
		assert.Equal(t, "invalid or missing signature", p.Detail)
	})
	t.Run("PAYLOAD_TOO_LARGE signed body", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytes.NewReader(bytes.Repeat([]byte("x"), 64)))
		r.Header.Set("X-API-Key", routeTestAgentKey)
		r.Header.Set("X-Timestamp", strconv.FormatInt(time.Now().UnixNano(), 10))
		r.Header.Set("X-Nonce", "nonce-1")
		r.Header.Set("X-Signature", "00")
		rec := httptest.NewRecorder()
		signed(t, 16).ServeHTTP(rec, r)
		requireAgentProblem(t, rec, http.StatusRequestEntityTooLarge, apierr.CodePayloadTooLarge)
	})
}

// TestAgentErrorCodes_Handlers covers the codes the Huma agent handlers
// raise themselves, plus Huma's own request validation (coded by
// agentErrorCodeTransformer).
func TestAgentErrorCodes_Handlers(t *testing.T) {
	h, database, staging, readonly, archive := errorCodeServer(t, true)

	t.Run("VALIDATION_FAILED handler check", func(t *testing.T) {
		rec := agentRequest(t, h, http.MethodPost, "/api/v1/agent/rebase", routeTestAgentKey, map[string]any{
			"nodeUuid": "not-a-uuid", "targetPath": filepath.Join(staging, "x.jpg"),
		})
		p := requireAgentProblem(t, rec, http.StatusBadRequest, apierr.CodeValidationFailed)
		assert.Equal(t, "nodeUuid must be a UUID", p.Detail)
	})
	t.Run("VALIDATION_FAILED huma schema 422", func(t *testing.T) {
		rec := agentRequest(t, h, http.MethodPost, "/api/v1/agent/events", routeTestAgentKey, map[string]any{
			"agentId": "test-device", "eventType": "EVENT_NOT_A_TYPE", "payload": "{}",
		})
		requireAgentProblem(t, rec, http.StatusUnprocessableEntity, apierr.CodeValidationFailed)
	})
	t.Run("VALIDATION_FAILED malformed JSON", func(t *testing.T) {
		rec := agentRequest(t, h, http.MethodPost, "/api/v1/agent/node-status", routeTestAgentKey, "{not json")
		require.GreaterOrEqual(t, rec.Code, 400)
		require.Less(t, rec.Code, 500)
		requireAgentProblem(t, rec, rec.Code, apierr.CodeValidationFailed)
	})
	t.Run("VALIDATION_FAILED query parameter pattern", func(t *testing.T) {
		rec := agentRequest(t, h, http.MethodGet, "/api/v1/agent/check-content?fullHash=xyz", routeTestAgentKey, nil)
		requireAgentProblem(t, rec, http.StatusUnprocessableEntity, apierr.CodeValidationFailed)
	})
	t.Run("AGENT_ID_MISMATCH", func(t *testing.T) {
		rec := agentRequest(t, h, http.MethodPost, "/api/v1/agent/handshake", routeTestAgentKey, map[string]any{"agentId": "someone-else"})
		p := requireAgentProblem(t, rec, http.StatusForbidden, apierr.CodeAgentIDMismatch)
		assert.Equal(t, "agent id mismatch", p.Detail)
	})
	t.Run("EVENT_UUID_CONFLICT", func(t *testing.T) {
		ev := map[string]any{
			"eventUuid": "018f0000-0000-7000-8000-0000000c0001", "agentId": "test-device",
			"eventType": "EVENT_NODE_DELETED", "payload": `{"nodeUuid":"018f0000-0000-7000-8000-0000000c0002"}`,
		}
		require.Equal(t, http.StatusAccepted, agentRequest(t, h, http.MethodPost, "/api/v1/agent/events", routeTestAgentKey, ev).Code)
		ev["eventType"] = "EVENT_NODE_MOVED"
		rec := agentRequest(t, h, http.MethodPost, "/api/v1/agent/events", routeTestAgentKey, ev)
		requireAgentProblem(t, rec, http.StatusConflict, apierr.CodeEventConflict)
	})

	t.Run("AGENT_ID_MISMATCH telemetry", func(t *testing.T) {
		rec := agentRequest(t, h, http.MethodPost, "/api/v1/agent/telemetry", routeTestAgentKey, map[string]any{
			"agentId": "someone-else", "timestampUnix": time.Now().Unix(), "scratchStorage": map[string]any{
				"mountPath": "/scratch", "totalBytes": 1, "freeBytes": 1, "usedBytes": 0,
				"mirrorsSizeBytes": 0, "renderCacheSizeBytes": 0, "proxiesSizeBytes": 0, "prunableBytes": 0,
			},
		})
		requireAgentProblem(t, rec, http.StatusForbidden, apierr.CodeAgentIDMismatch)
	})
	t.Run("VALIDATION_FAILED check-content without a hash", func(t *testing.T) {
		rec := agentRequest(t, h, http.MethodGet, "/api/v1/agent/check-content", routeTestAgentKey, nil)
		p := requireAgentProblem(t, rec, http.StatusBadRequest, apierr.CodeValidationFailed)
		assert.Equal(t, "fullHash or fastHash is required", p.Detail)
	})

	rebase := func(nodeUUID, target string, extra map[string]any) *httptest.ResponseRecorder {
		body := map[string]any{"nodeUuid": nodeUUID, "targetPath": target}
		for k, v := range extra {
			body[k] = v
		}
		return agentRequest(t, h, http.MethodPost, "/api/v1/agent/rebase", routeTestAgentKey, body)
	}
	t.Run("PATH_OUTSIDE_STORAGE", func(t *testing.T) {
		rec := rebase("018f0000-0000-7000-8000-0000000d0001", "/definitely/not/a/storage/location.jpg", nil)
		requireAgentProblem(t, rec, http.StatusBadRequest, apierr.CodePathOutsideStorage)
	})
	t.Run("READ_ONLY_TARGET", func(t *testing.T) {
		rec := rebase("018f0000-0000-7000-8000-0000000d0002", filepath.Join(readonly, "x.jpg"), nil)
		requireAgentProblem(t, rec, http.StatusBadRequest, apierr.CodeReadOnlyTarget)
	})
	t.Run("ARCHIVE_FILE_NOT_PRESENT", func(t *testing.T) {
		rec := rebase("018f0000-0000-7000-8000-0000000d0003", filepath.Join(archive, "not-copied-yet.mov"), nil)
		requireAgentProblem(t, rec, http.StatusBadRequest, apierr.CodeArchiveFileNotPresent)
	})
	t.Run("NODE_NOT_FOUND", func(t *testing.T) {
		u := "018f0000-0000-7000-8000-0000000d0004"
		seedAgentNode(t, database, u, filepath.Join(staging, "superseded.jpg"), "ARCHIVED", nil, "test-device")
		rec := rebase(u, filepath.Join(staging, "moved.jpg"), nil)
		requireAgentProblem(t, rec, http.StatusNotFound, apierr.CodeNodeNotFound)
	})
	t.Run("NODE_NOT_FOUND trashed", func(t *testing.T) {
		u := "018f0000-0000-7000-8000-0000000d0007"
		seedAgentNode(t, database, u, filepath.Join(staging, "trashed.jpg"), "TRASHED", nil, "test-device")
		rec := rebase(u, filepath.Join(staging, "restored.jpg"), nil)
		requireAgentProblem(t, rec, http.StatusNotFound, apierr.CodeNodeNotFound)
	})
	t.Run("NOT_NODE_OWNER", func(t *testing.T) {
		u := "018f0000-0000-7000-8000-0000000d0005"
		seedAgentNode(t, database, u, filepath.Join(staging, "theirs.jpg"), "ACTIVE", nil, "other-device")
		rec := rebase(u, filepath.Join(staging, "mine-now.jpg"), nil)
		requireAgentProblem(t, rec, http.StatusForbidden, apierr.CodeNotNodeOwner)
	})
	t.Run("HASH_CONFLICT", func(t *testing.T) {
		u := "018f0000-0000-7000-8000-0000000d0006"
		have := "1111111111111111111111111111111111111111111111111111111111111111"
		seedAgentNode(t, database, u, filepath.Join(staging, "hashed.jpg"), "ACTIVE", &have, "test-device")
		rec := rebase(u, filepath.Join(staging, "hashed2.jpg"), map[string]any{
			"fullHash": "2222222222222222222222222222222222222222222222222222222222222222",
		})
		requireAgentProblem(t, rec, http.StatusConflict, apierr.CodeHashConflict)
	})
}

// TestAgentErrorCodes_StorageNotConfiguredAndUpload covers the codes that
// need a server without storage, and the raw upload endpoint, whose body
// keeps the legacy "error" field next to the envelope.
func TestAgentErrorCodes_StorageNotConfiguredAndUpload(t *testing.T) {
	srv, _ := fullTestServer(t) // no storage guard, no writable location
	h := srv.Handler()

	t.Run("STORAGE_NOT_CONFIGURED rebase", func(t *testing.T) {
		rec := agentRequest(t, h, http.MethodPost, "/api/v1/agent/rebase", routeTestAgentKey, map[string]any{
			"nodeUuid": "018f0000-0000-7000-8000-0000000e0001", "targetPath": "/media/x.jpg",
		})
		requireAgentProblem(t, rec, http.StatusInternalServerError, apierr.CodeStorageNotConfigured)
	})

	upload := func(h http.Handler, key string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader([]byte("payload bytes")))
		r.Header.Set("X-API-Key", key)
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, r)
		return rec
	}
	t.Run("STORAGE_NOT_CONFIGURED upload", func(t *testing.T) {
		rec := upload(h, routeTestAgentKey, map[string]string{"X-Filename": "a.jpg"})
		p := requireAgentProblem(t, rec, http.StatusServiceUnavailable, apierr.CodeStorageNotConfigured)
		assert.Equal(t, "no writable storage location configured", p.Detail)
		assert.Equal(t, p.Detail, p.Error, "legacy error field must be kept")
	})

	hw, _, _, _, _ := errorCodeServer(t, false)
	t.Run("VALIDATION_FAILED upload filename", func(t *testing.T) {
		rec := upload(hw, routeTestAgentKey, map[string]string{"X-Filename": "../escape.jpg"})
		requireAgentProblem(t, rec, http.StatusBadRequest, apierr.CodeValidationFailed)
	})
	t.Run("CHECKSUM_MISMATCH upload", func(t *testing.T) {
		rec := upload(hw, routeTestAgentKey, map[string]string{
			"X-Filename":    "b.jpg",
			"X-Blake3-Hash": "0000000000000000000000000000000000000000000000000000000000000000",
		})
		p := requireAgentProblem(t, rec, http.StatusBadRequest, apierr.CodeChecksumMismatch)
		assert.Equal(t, "checksum mismatch", p.Error)
	})
}

// TestAgentErrorCodes_BrowserRoutesUnchanged: the envelope's code is an
// agent-API contract; browser routes keep Huma's plain error model.
func TestAgentErrorCodes_BrowserRoutesUnchanged(t *testing.T) {
	srv, _ := fullTestServer(t)
	rec := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets?limit=-5", nil)
	require.Equal(t, http.StatusUnprocessableEntity, rec.Code, rec.Body.String())
	var body map[string]any
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	assert.NotContains(t, body, "code")
}
