package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/pairing"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// newPairingUploadTestServer wires a Server with the pairing service AND a
// writable TIER3_MASTER_ARCHIVE location, so tests can both mint a paired
// device's API key and actually exercise /api/v1/agent/upload with it.
// Neither existing helper covers both: serverWithGuard has no pairing
// service wired (LookupKey is nil), and newPairingTestServer's Guard has no
// locations.
func newPairingUploadTestServer(t *testing.T) (*Server, *db.DB, *pairing.Service, string) {
	t.Helper()
	root := t.TempDir()
	archiveDir := filepath.Join(root, "archive")
	require.NoError(t, os.MkdirAll(archiveDir, 0o755))

	dbPath := filepath.Join(root, "pairing_upload.db")
	database, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	var loc sqlcgen.StorageLocation
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var err error
		loc, err = q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name:     "MasterArchive",
			RootPath: archiveDir,
			Tier:     "TIER3_MASTER_ARCHIVE",
			ReadOnly: 0,
			Prunable: 0,
		})
		return err
	})
	require.NoError(t, err)

	guard := storage.NewGuard([]storage.Location{
		{ID: loc.ID, Name: "MasterArchive", RootPath: archiveDir, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: false},
	})

	pairSvc := pairing.NewService(database, nil, nil)
	srv := New(Deps{
		Config:  &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:      database,
		Guard:   guard,
		Prober:  probe.New(),
		Engine:  graph.NewEngine(database, nil),
		Hub:     sse.New(),
		Version: "test",
		Pairing: pairSvc,
	})
	return srv, database, pairSvc, archiveDir
}

func stubQRPayloadForAttrTest(agentID, apiKey string) []byte {
	return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
}

func blake3Hex(t *testing.T, data []byte) string {
	t.Helper()
	h := blake3.New()
	_, err := h.Write(data)
	require.NoError(t, err)
	return hex.EncodeToString(h.Sum(nil))
}

// TestAgentUpload_PairedDeviceWithOwnerAttributesUpload backs Issue: a
// paired device whose device_pairings.user_id is set must have its upload
// attributed to that owner (agent_upload.go's pairing lookup path).
func TestAgentUpload_PairedDeviceWithOwnerAttributesUpload(t *testing.T) {
	srv, database, pairSvc, _ := newPairingUploadTestServer(t)
	ctx := context.Background()

	// Seed a users row so the FK (RESTRICT, non-CASCADE) is satisfiable.
	var ownerID int64
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		id, err := q.CreateAttributionUser(ctx, sqlcgen.CreateAttributionUserParams{
			AuthProvider: "authentik", ExternalUid: "owner-uid", Username: "owner",
		})
		ownerID = id
		return err
	})
	require.NoError(t, err)

	p, key, err := pairSvc.CreatePairing(ctx, "Owned Phone", "test-admin", ownerID, stubQRPayloadForAttrTest)
	require.NoError(t, err)
	require.True(t, p.UserID.Valid)
	require.Equal(t, ownerID, p.UserID.Int64)

	handler := srv.Handler()
	data := []byte("owned pairing upload bytes")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req.Header.Set("X-API-Key", key.Plaintext)
	req.Header.Set("X-Filename", "owned.jpg")
	req.Header.Set("X-Blake3-Hash", blake3Hex(t, data))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var resp AgentUploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	node, err := database.Reader.GetMediaNodeByUUID(ctx, resp.NodeUUID)
	require.NoError(t, err)
	require.True(t, node.UploadedByUserID.Valid, "uploaded_by_user_id must be set for a paired-with-owner upload")
	assert.Equal(t, ownerID, node.UploadedByUserID.Int64)
}

// TestAgentUpload_PairedDeviceWithoutOwnerLeavesAttributionNull backs cause
// (A) from the investigation: a pairing with a NULL user_id (never
// re-paired since migration 00020, or paired by a session with no
// resolvable attribution identity) must leave uploaded_by_user_id NULL --
// not error, not fall back to some other identity.
func TestAgentUpload_PairedDeviceWithoutOwnerLeavesAttributionNull(t *testing.T) {
	srv, database, pairSvc, _ := newPairingUploadTestServer(t)
	ctx := context.Background()

	// userID = 0 -> CreatePairing stores a NULL device_pairings.user_id,
	// exactly the state migration 00020's own doc comment describes for
	// legacy pairings.
	p, key, err := pairSvc.CreatePairing(ctx, "Ownerless Phone", "test-admin", 0, stubQRPayloadForAttrTest)
	require.NoError(t, err)
	require.False(t, p.UserID.Valid)

	handler := srv.Handler()
	data := []byte("ownerless pairing upload bytes")
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req.Header.Set("X-API-Key", key.Plaintext)
	req.Header.Set("X-Filename", "ownerless.jpg")
	req.Header.Set("X-Blake3-Hash", blake3Hex(t, data))

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusCreated, rec.Code, "body=%s", rec.Body.String())

	var resp AgentUploadResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &resp))

	node, err := database.Reader.GetMediaNodeByUUID(ctx, resp.NodeUUID)
	require.NoError(t, err)
	assert.False(t, node.UploadedByUserID.Valid, "uploaded_by_user_id must stay NULL when the pairing has no owner")
}

// TestAgentUpload_DedupBackfillsMissingAttribution backs cause (B): a
// pre-write dedup hit (X-Blake3-Hash matches an existing live node) must
// backfill uploaded_by_user_id when the existing row has none and this
// request carries a resolved owner -- upload_engine.go's
// backfillDedupUploaderIfMissing. Without this, a dedup'd re-upload of an
// already-indexed file would leave "Uploaded by" permanently blank even
// after re-pairing fixes attribution going forward.
func TestAgentUpload_DedupBackfillsMissingAttribution(t *testing.T) {
	srv, database, pairSvc, _ := newPairingUploadTestServer(t)
	ctx := context.Background()

	var ownerID int64
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		id, err := q.CreateAttributionUser(ctx, sqlcgen.CreateAttributionUserParams{
			AuthProvider: "authentik", ExternalUid: "dedup-owner-uid", Username: "dedup-owner",
		})
		ownerID = id
		return err
	})
	require.NoError(t, err)

	handler := srv.Handler()
	data := []byte("dedup backfill attribution test bytes")
	hash := blake3Hex(t, data)

	// First upload: unauthenticated-for-attribution-purposes env-bootstrap
	// key (no pairing row), so the node lands with uploaded_by_user_id
	// NULL -- simulating a scanner/watcher-indexed or pre-pairing upload.
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req1.Header.Set("X-API-Key", routeTestAgentKey)
	req1.Header.Set("X-Filename", "first.jpg")
	req1.Header.Set("X-Blake3-Hash", hash)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusCreated, rec1.Code, "body=%s", rec1.Body.String())

	var resp1 AgentUploadResponse
	require.NoError(t, json.Unmarshal(rec1.Body.Bytes(), &resp1))
	node1, err := database.Reader.GetMediaNodeByUUID(ctx, resp1.NodeUUID)
	require.NoError(t, err)
	require.False(t, node1.UploadedByUserID.Valid, "precondition: first upload must have no attribution")

	// Second upload of the SAME bytes from a paired-with-owner device:
	// this is the pre-write dedup path (upload_engine.go's ExpectedBlake3
	// check), which must backfill the existing row's attribution.
	p, key, err := pairSvc.CreatePairing(ctx, "Dedup Phone", "test-admin", ownerID, stubQRPayloadForAttrTest)
	require.NoError(t, err)
	require.True(t, p.UserID.Valid)

	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/upload", bytes.NewReader(data))
	req2.Header.Set("X-API-Key", key.Plaintext)
	req2.Header.Set("X-Filename", "second.jpg")
	req2.Header.Set("X-Blake3-Hash", hash)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusOK, rec2.Code, "body=%s", rec2.Body.String())
	require.Equal(t, "true", rec2.Header().Get("X-Dedup"))

	var resp2 AgentUploadResponse
	require.NoError(t, json.Unmarshal(rec2.Body.Bytes(), &resp2))
	require.Equal(t, resp1.NodeUUID, resp2.NodeUUID, "dedup must return the same node")

	nodeAfter, err := database.Reader.GetMediaNodeByUUID(ctx, resp1.NodeUUID)
	require.NoError(t, err)
	require.True(t, nodeAfter.UploadedByUserID.Valid, "dedup must backfill uploaded_by_user_id when it was NULL")
	assert.Equal(t, ownerID, nodeAfter.UploadedByUserID.Int64)
}

// TestResolveAgentUploadUserID_RevokedPairingStaysNull backs the
// pairing.RevokedAt.Valid branch in resolveAgentUploadUserID. That branch
// is unreachable through the normal HTTP path in this test server --
// GetDevicePairingKeyByHash (behind AgentConfig.LookupKey) already filters
// out a revoked pairing's keys, so a request authenticated against one
// never reaches the handler. It exists for the narrow TOCTOU window where a
// pairing is revoked between that auth check and this lookup, for a request
// already in flight. Revoking the pairing directly via SQL (rather than
// pairSvc.RevokePairing, which also revokes the pairing's keys) reproduces
// that state without going through the auth layer, so the branch is
// exercised deterministically instead of relying on a real race.
func TestResolveAgentUploadUserID_RevokedPairingStaysNull(t *testing.T) {
	srv, database, pairSvc, _ := newPairingUploadTestServer(t)
	ctx := context.Background()

	var ownerID int64
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		id, err := q.CreateAttributionUser(ctx, sqlcgen.CreateAttributionUserParams{
			AuthProvider: "authentik", ExternalUid: "revoked-owner-uid", Username: "revoked-owner",
		})
		ownerID = id
		return err
	})
	require.NoError(t, err)

	p, _, err := pairSvc.CreatePairing(ctx, "Revoked Phone", "test-admin", ownerID, stubQRPayloadForAttrTest)
	require.NoError(t, err)
	require.True(t, p.UserID.Valid)

	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.RevokeDevicePairing(ctx, sqlcgen.RevokeDevicePairingParams{
			ID:        p.ID,
			RevokedAt: sql.NullInt64{Int64: p.CreatedAt + 1, Valid: true},
		})
	})
	require.NoError(t, err)

	userID := srv.resolveAgentUploadUserID(ctx, p.AgentID)
	assert.Equal(t, int64(0), userID, "a revoked pairing must never attribute an upload to its former owner")
}
