package httpapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// seedCheckContentNode inserts one media node on the archive location of
// serverWithGuard (storage_location_id 3) with the given hashes, size and
// lifecycle state.
func seedCheckContentNode(t *testing.T, database *db.DB, archive, nodeUUID, fastHash, fullHash string, size int64, lifecycle string) {
	t.Helper()
	ctx := context.Background()
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		params := sqlcgen.InsertMediaNodeParams{
			NodeUuid:          nodeUUID,
			StorageLocationID: 3,
			FilePath:          filepath.Join(archive, nodeUUID+".raw"),
			FileName:          nodeUUID + ".raw",
			FileExt:           ".raw",
			SizeBytes:         size,
			IndexingStatus:    "INDEXED_FULL",
			GraphStatus:       "UNLINKED",
			LifecycleState:    lifecycle,
		}
		if fastHash != "" {
			params.FastHash = &fastHash
		}
		if fullHash != "" {
			params.FullHash = &fullHash
		}
		n, err := q.InsertMediaNode(ctx, params)
		if err != nil {
			return err
		}
		return db.RecordFullHashSource(ctx, q, n.ID, params.FullHash, db.HashSourceServer)
	})
	require.NoError(t, err)
}

// getCheckContent issues an authenticated check-content GET and returns the
// status code, the decoded result and the raw JSON object (to assert which
// keys are present at all).
func getCheckContent(t *testing.T, h http.Handler, q url.Values) (int, AgentCheckContentResult, map[string]any) {
	t.Helper()
	target := "/api/v1/agent/check-content"
	if enc := q.Encode(); enc != "" {
		target += "?" + enc
	}
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	var res AgentCheckContentResult
	var raw map[string]any
	if rec.Code == http.StatusOK {
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &res))
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &raw))
	}
	return rec.Code, res, raw
}

// assertNoNodeIdentity checks that a fast-only answer never discloses which
// node matched: a fast hash is not proof of content, so the path and UUID of
// another device's node must not leak to the caller.
func assertNoNodeIdentity(t *testing.T, raw map[string]any) {
	t.Helper()
	for _, k := range []string{"nodeUuid", "filePath", "lifecycleState", "indexingStatus"} {
		_, present := raw[k]
		assert.Falsef(t, present, "fast-only response must not carry %q: %v", k, raw)
	}
}

// TestAgentCheckContent_FastHashPreScreen backs the agent's phase-1 dedup
// pre-flight: it sends fastHash (plus sizeBytes) without a fullHash, which
// the server used to reject with 400 -- the agent then latched "dedup
// unavailable" and never deduplicated anything.
func TestAgentCheckContent_FastHashPreScreen(t *testing.T) {
	srv, database, _, _, _, archive := serverWithGuard(t)
	h := srv.Handler()

	const (
		activeFast  = "1111111111111111"
		hiddenFast  = "2222222222222222"
		trashedFast = "3333333333333333"
		archFast    = "4444444444444444"
		missingFast = "5555555555555555"
		zeroFast    = "6666666666666666"
	)
	seedCheckContentNode(t, database, archive, "018f0000-0000-7000-8000-00000000f001", activeFast, strings.Repeat("a1", 32), 4096, "ACTIVE")
	seedCheckContentNode(t, database, archive, "018f0000-0000-7000-8000-00000000f002", hiddenFast, "", 4096, "HIDDEN")
	seedCheckContentNode(t, database, archive, "018f0000-0000-7000-8000-00000000f003", trashedFast, "", 4096, "TRASHED")
	seedCheckContentNode(t, database, archive, "018f0000-0000-7000-8000-00000000f004", archFast, "", 4096, "ARCHIVED")
	seedCheckContentNode(t, database, archive, "018f0000-0000-7000-8000-00000000f005", missingFast, "", 4096, "MISSING")
	seedCheckContentNode(t, database, archive, "018f0000-0000-7000-8000-00000000f006", zeroFast, "", 0, "ACTIVE")

	fast := func(fh string, size int64) url.Values {
		q := url.Values{}
		q.Set("fastHash", fh)
		if size > 0 {
			q.Set("sizeBytes", strconv.FormatInt(size, 10))
		}
		return q
	}

	tests := []struct {
		name     string
		q        url.Values
		possible bool
	}{
		{"fast-only hit on ACTIVE node", fast(activeFast, 0), true},
		{"fast-only miss", fast("0123456789abcdef", 0), false},
		{"fast+size match", fast(activeFast, 4096), true},
		{"fast+size mismatch", fast(activeFast, 4097), false},
		{"HIDDEN counts as live", fast(hiddenFast, 4096), true},
		{"TRASHED is not live", fast(trashedFast, 4096), false},
		{"ARCHIVED is not live", fast(archFast, 4096), false},
		{"MISSING is not live", fast(missingFast, 4096), false},
		{"stored size 0 is unknown and does not hide a candidate", fast(zeroFast, 12345), true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			code, res, raw := getCheckContent(t, h, tc.q)
			require.Equal(t, http.StatusOK, code)
			assert.Equal(t, tc.possible, res.PossibleMatch)
			assert.False(t, res.Found, "a fast hash alone is never a definitive match")
			_, hasPossible := raw["possibleMatch"]
			assert.True(t, hasPossible, "possibleMatch must always be present: %v", raw)
			assertNoNodeIdentity(t, raw)
		})
	}

	t.Run("fast hash is matched case-insensitively", func(t *testing.T) {
		code, res, _ := getCheckContent(t, h, fast(strings.ToUpper("abcdefabcdefabcd"), 0))
		require.Equal(t, http.StatusOK, code)
		assert.False(t, res.PossibleMatch)

		seedCheckContentNode(t, database, archive, "018f0000-0000-7000-8000-00000000f007", "abcdefabcdefabcd", "", 10, "ACTIVE")
		code, res, _ = getCheckContent(t, h, fast(strings.ToUpper("abcdefabcdefabcd"), 0))
		require.Equal(t, http.StatusOK, code)
		assert.True(t, res.PossibleMatch)
	})
}

// TestAgentCheckContent_FullHashUnchanged pins that a fullHash request keeps
// its existing behaviour (definitive lookup reporting the node's identity),
// with possibleMatch mirroring found, regardless of an accompanying fastHash
// or sizeBytes.
func TestAgentCheckContent_FullHashUnchanged(t *testing.T) {
	srv, database, _, _, _, archive := serverWithGuard(t)
	h := srv.Handler()

	fullHash := strings.Repeat("c3", 32)
	nodeUUID := "018f0000-0000-7000-8000-00000000f101"
	seedCheckContentNode(t, database, archive, nodeUUID, "7777777777777777", fullHash, 2048, "ACTIVE")

	q := url.Values{}
	q.Set("fullHash", fullHash)
	code, res, _ := getCheckContent(t, h, q)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, res.Found)
	assert.True(t, res.PossibleMatch)
	assert.Equal(t, nodeUUID, res.NodeUUID)
	assert.Equal(t, filepath.Join(archive, nodeUUID+".raw"), res.FilePath)
	assert.Equal(t, "ACTIVE", res.LifecycleState)
	assert.Equal(t, "INDEXED_FULL", res.IndexingStatus)

	// A non-matching fastHash and a wrong size don't affect the full-hash
	// answer -- fullHash takes precedence.
	q.Set("fastHash", "0000000000000000")
	q.Set("sizeBytes", "1")
	code, res, _ = getCheckContent(t, h, q)
	require.Equal(t, http.StatusOK, code)
	assert.True(t, res.Found)
	assert.Equal(t, nodeUUID, res.NodeUUID)

	// Full-hash miss: found and possibleMatch both false, nothing else.
	miss := url.Values{}
	miss.Set("fullHash", strings.Repeat("d4", 32))
	code, res, raw := getCheckContent(t, h, miss)
	require.Equal(t, http.StatusOK, code)
	assert.False(t, res.Found)
	assert.False(t, res.PossibleMatch)
	assertNoNodeIdentity(t, raw)
}

func TestAgentCheckContent_Validation_FastOnly(t *testing.T) {
	srv, _, _, _, _, _ := serverWithGuard(t)
	h := srv.Handler()

	// Neither hash -> 400.
	code, _, _ := getCheckContent(t, h, url.Values{})
	assert.Equal(t, http.StatusBadRequest, code)

	// sizeBytes alone is not a query.
	onlySize := url.Values{}
	onlySize.Set("sizeBytes", "100")
	code, _, _ = getCheckContent(t, h, onlySize)
	assert.Equal(t, http.StatusBadRequest, code)

	// Malformed fastHash alone -> 400/422.
	bad := url.Values{}
	bad.Set("fastHash", "xyz")
	code, _, _ = getCheckContent(t, h, bad)
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusUnprocessableEntity}, code)

	// Negative size -> 400/422.
	neg := url.Values{}
	neg.Set("fastHash", "0123456789abcdef")
	neg.Set("sizeBytes", "-1")
	code, _, _ = getCheckContent(t, h, neg)
	assert.Contains(t, []int{http.StatusBadRequest, http.StatusUnprocessableEntity}, code)
}

// TestAgentNodeStatus_ReturnsFullHash pins the additive fullHash field: a
// client deciding whether to reclaim its local copy can compare the server's
// stored hash with the one it computed itself.
func TestAgentNodeStatus_ReturnsFullHash(t *testing.T) {
	srv, database, _, _, _, archive := serverWithGuard(t)

	withHash := "018f0000-0000-7000-8000-00000000f201"
	withoutHash := "018f0000-0000-7000-8000-00000000f202"
	unknown := "018f0000-0000-7000-8000-00000000f203"
	fullHash := strings.Repeat("e5", 32)
	seedCheckContentNode(t, database, archive, withHash, "", fullHash, 10, "ACTIVE")
	seedCheckContentNode(t, database, archive, withoutHash, "", "", 10, "ACTIVE")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/node-status", bytesOfJSON(t, map[string]any{
		"nodeUuids": []string{withHash, withoutHash, unknown},
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())

	var res struct {
		Statuses []map[string]any `json:"statuses"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &res))
	require.Len(t, res.Statuses, 3)
	byUUID := map[string]map[string]any{}
	for _, s := range res.Statuses {
		byUUID[s["nodeUuid"].(string)] = s
	}

	assert.Equal(t, fullHash, byUUID[withHash]["fullHash"])
	assert.Equal(t, true, byUUID[withHash]["verified"])
	_, present := byUUID[withoutHash]["fullHash"]
	assert.False(t, present, "fullHash must be omitted when the node has none: %v", byUUID[withoutHash])
	_, present = byUUID[unknown]["fullHash"]
	assert.False(t, present, "fullHash must be omitted for an unknown node: %v", byUUID[unknown])
}
