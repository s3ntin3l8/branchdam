package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/pairing"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/secrets"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// newPairingTestServer wires a Server with the pairing service enabled.
// Returns srv, database, pairing service. The pairing service is exposed
// so tests can seed state directly (faster than driving HTTP for setup).
func newPairingTestServer(t *testing.T) (*Server, *db.DB, *pairing.Service) {
	t.Helper()
	return newPairingTestServerWithBox(t, nil)
}

// newPairingTestServerWithBox is newPairingTestServer with an explicit
// secrets.Box (nil = keyless: plaintext qr_svg, NULL pairing_url).
func newPairingTestServerWithBox(t *testing.T, box *secrets.Box) (*Server, *db.DB, *pairing.Service) {
	t.Helper()
	root := t.TempDir()
	dbPath := root + "/pairing_http.db"
	database, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	pairSvc := pairing.NewService(database, nil, nil, box)
	srv := New(Deps{
		Config:  &config.Config{Agent: config.Agent{}},
		DB:      database,
		Guard:   storage.NewGuard(nil),
		Prober:  probe.New(),
		Engine:  graph.NewEngine(database, nil),
		Hub:     sse.New(),
		Version: "test",
		Pairing: pairSvc,
	})
	return srv, database, pairSvc
}

// doAdmin runs an admin-only pairing route via the registered handler
// and returns the response. The routes are admin-only via the global
// auth.RequireAdmin middleware, so we inject an authenticated
// browser-side principal into the context before ServeHTTP -- this is
// the same trick every other admin-route test in this package uses
// (see settings_test.go's pattern).
func doAdmin(t *testing.T, srv *Server, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		require.NoError(t, err)
		bodyReader = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, bodyReader)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	// Inject an authenticated admin principal. The auth.Route middleware
	// (BrowserChain, called from Handler()) wraps BrowserChain, which
	// reads X-Authentik-Username -- setting it here populates the
	// principal accordingly. With an empty Authz.Groups list (default
	// for the test Config), every authenticated user is admin.
	req.Header.Set("X-Authentik-Username", "test-admin")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestCompanionPairings_NotConfiguredReturns503(t *testing.T) {
	// Server with no Pairing dependency -- existing serverWithGuard
	// helper still works because Pairing is a zero-value nil here.
	srv, _, _, _, _, _ := serverWithGuard(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/companion/pairings", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
}

func TestCompanionPairings_CreateListGetRevoke(t *testing.T) {
	srv, _, _ := newPairingTestServer(t)

	// Create
	rec := doAdmin(t, srv, http.MethodPost, "/api/v1/companion/pairings",
		map[string]string{"friendlyLabel": "Björn's iPhone"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var created struct {
		PairingID     int64  `json:"pairingId"`
		AgentID       string `json:"agentId"`
		APIKey        string `json:"apiKey"`
		KeyPreview    string `json:"keyPreview"`
		PairingURL    string `json:"pairingUrl"`
		QRSVG         string `json:"qrSvg"`
		CreatedAtUnix int64  `json:"createdAtUnix"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.NotZero(t, created.PairingID)
	assert.NotEmpty(t, created.AgentID)
	assert.NotEmpty(t, created.APIKey)
	assert.Contains(t, created.QRSVG, "<svg")
	assert.True(t, strings.HasPrefix(created.PairingURL, "branchdam://?"))
	assert.Contains(t, created.PairingURL, created.APIKey)

	// List
	rec = doAdmin(t, srv, http.MethodGet, "/api/v1/companion/pairings", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var listed struct {
		Pairings []struct {
			ID             int64  `json:"id"`
			AgentID        string `json:"agentId"`
			ActiveKeyCount int64  `json:"activeKeyCount"`
		} `json:"pairings"`
		Total int64 `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	assert.Equal(t, int64(1), listed.Total)
	assert.Equal(t, created.PairingID, listed.Pairings[0].ID)
	assert.Equal(t, int64(1), listed.Pairings[0].ActiveKeyCount)

	// Get
	rec = doAdmin(t, srv, http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID), nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// Rotate
	rec = doAdmin(t, srv, http.MethodPost,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/rotate",
		map[string]int{"graceMinutes": 60})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var rotated struct {
		KeyID                int64  `json:"keyId"`
		APIKey               string `json:"apiKey"`
		PairingURL           string `json:"pairingUrl"`
		QRSVG                string `json:"qrSvg"`
		PreviousKeyExpiresAt int64  `json:"previousKeyExpiresAtUnix"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rotated))
	assert.NotEqual(t, created.APIKey, rotated.APIKey)
	assert.True(t, strings.HasPrefix(rotated.PairingURL, "branchdam://?"))
	assert.Contains(t, rotated.PairingURL, rotated.APIKey)

	// Revoke
	rec = doAdmin(t, srv, http.MethodPost,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/revoke", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// Get-after-revoke still works but body shows revoked_at set
	rec = doAdmin(t, srv, http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID), nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var afterRevoke struct {
		RevokedAtUnix *int64 `json:"revokedAtUnix"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &afterRevoke))
	require.NotNil(t, afterRevoke.RevokedAtUnix)
}

func TestCompanionPairings_DeleteRevokedPairing(t *testing.T) {
	srv, database, _ := newPairingTestServer(t)

	// Create
	rec := doAdmin(t, srv, http.MethodPost, "/api/v1/companion/pairings",
		map[string]string{"friendlyLabel": "Delete-me iPhone"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var created struct {
		PairingID int64 `json:"pairingId"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotZero(t, created.PairingID)

	// Delete before revoke -- must fail with 409
	rec = doAdmin(t, srv, http.MethodDelete,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID), nil)
	assert.Equal(t, http.StatusConflict, rec.Code)

	// Revoke
	rec = doAdmin(t, srv, http.MethodPost,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/revoke", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	// Delete after revoke -- must succeed
	rec = doAdmin(t, srv, http.MethodDelete,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var deleted struct {
		OK bool `json:"ok"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &deleted))
	assert.True(t, deleted.OK)

	// Get-after-delete -- must 404
	rec = doAdmin(t, srv, http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID), nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)

	// List must no longer include the deleted pairing
	rec = doAdmin(t, srv, http.MethodGet, "/api/v1/companion/pairings", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var listed struct {
		Pairings []struct {
			ID int64 `json:"id"`
		} `json:"pairings"`
		Total int64 `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &listed))
	assert.Equal(t, int64(0), listed.Total)

	// actor_audit must contain a trace of the deletion that survives
	// the pairing removal (the pairing-scoped companion_pairing_audit
	// rows are gone, but the global actor_audit table is unbound).
	auditCount, err := database.Reader.CountActorAudit(context.Background(), sqlcgen.CountActorAuditParams{
		Event:        sql.NullString{String: audit.EventPairingDeleted, Valid: true},
		ResourceType: sql.NullString{String: "companion_pairing", Valid: true},
		ResourceID:   sql.NullString{String: pairingIDStr(created.PairingID), Valid: true},
	})
	require.NoError(t, err)
	assert.Equal(t, int64(1), auditCount, "actor_audit should have exactly one trace of the deletion")
}

func TestCompanionPairings_DeleteNotFound(t *testing.T) {
	srv, _, _ := newPairingTestServer(t)
	rec := doAdmin(t, srv, http.MethodDelete,
		"/api/v1/companion/pairings/99999", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCompanionPairings_DeleteAfterRotateAndRevoke(t *testing.T) {
	srv, _, pairSvc := newPairingTestServer(t)
	ctx := context.Background()

	// Create, rotate (so there are multiple keys), then revoke and delete.
	p, _, err := pairSvc.CreatePairing(ctx, "Rotate-delete iPhone", "test", 0, func(agentID, apiKey string) []byte {
		return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
	})
	require.NoError(t, err)
	_, _, err = pairSvc.RotateKey(ctx, p.ID, "test", 1440, func(agentID, apiKey string) []byte {
		return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
	})
	require.NoError(t, err)
	_, err = pairSvc.RevokePairing(ctx, p.ID, "test")
	require.NoError(t, err)

	// Delete must clean up keys and audit without error
	rec := doAdmin(t, srv, http.MethodDelete,
		"/api/v1/companion/pairings/"+pairingIDStr(p.ID), nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	// Verify keys are gone
	err = srv.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		keys, err := q.ListKeysByPairing(ctx, p.ID)
		if err != nil {
			return err
		}
		assert.Empty(t, keys, "all keys should be deleted")
		return nil
	})
	require.NoError(t, err)

	// Verify pairing-scoped audit rows are gone
	auditCount, err := srv.db.Reader.CountPairingAudit(ctx, p.ID)
	require.NoError(t, err)
	assert.Equal(t, int64(0), auditCount, "pairing-scoped audit rows should be deleted")
}

func TestCompanionPairings_QRSVGReturnsCachedSVG(t *testing.T) {
	srv, _, _ := newPairingTestServer(t)
	rec := doAdmin(t, srv, http.MethodPost, "/api/v1/companion/pairings",
		map[string]string{"friendlyLabel": "iPhone"})
	require.Equal(t, http.StatusOK, rec.Code)
	var created struct {
		PairingID int64  `json:"pairingId"`
		QRSVG     string `json:"qrSvg"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	// GET the qr.svg directly via the mux route. requireSettingsAdmin
	// gates this reveal surface, so the request needs the admin identity
	// headers doAdmin injects.
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/qr.svg", nil)
	req.Header.Set("X-Authentik-Username", "test-admin")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Equal(t, "image/svg+xml", rec.Header().Get("Content-Type"))
	assert.Contains(t, rec.Body.String(), "<svg")
}

func TestCompanionPairings_QRSVGReturns410WhenNoActiveKey(t *testing.T) {
	srv, _, pairSvc := newPairingTestServer(t)
	ctx := context.Background()

	// Create then revoke to ensure no active key remains.
	p, _, err := pairSvc.CreatePairing(ctx, "iPhone", "test", 0, func(agentID, apiKey string) []byte {
		return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
	})
	require.NoError(t, err)
	_, err = pairSvc.RevokePairing(ctx, p.ID, "test")
	require.NoError(t, err)

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(p.ID)+"/qr.svg", nil)
	req.Header.Set("X-Authentik-Username", "test-admin")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	// Revoked pairings get the no-active-key branch (ErrNoActiveKey → 410).
	assert.Equal(t, http.StatusGone, rec.Code, "revoked pairing QR should return 410 Gone")
}

func TestCompanionPairings_AuditLogsEveryLifecycleEvent(t *testing.T) {
	srv, _, _ := newPairingTestServer(t)

	rec := doAdmin(t, srv, http.MethodPost, "/api/v1/companion/pairings",
		map[string]string{"friendlyLabel": "Audit Test"})
	require.Equal(t, http.StatusOK, rec.Code)
	var created struct {
		PairingID int64 `json:"pairingId"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	rec = doAdmin(t, srv, http.MethodPost,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/rotate",
		map[string]int{})
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doAdmin(t, srv, http.MethodPost,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/revoke", nil)
	require.Equal(t, http.StatusOK, rec.Code)

	rec = doAdmin(t, srv, http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/audit", nil)
	require.Equal(t, http.StatusOK, rec.Code)
	var pairingAudit struct {
		Events []struct {
			Actor string `json:"actor"`
			Event string `json:"event"`
		} `json:"events"`
		Total int64 `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &pairingAudit))
	assert.Equal(t, int64(4), pairingAudit.Total, "PAIR_CREATED, KEY_MINTED, KEY_ROTATED, PAIR_REVOKED")
	gotEvents := make(map[string]bool, len(pairingAudit.Events))
	for _, e := range pairingAudit.Events {
		gotEvents[e.Event] = true
	}
	assert.True(t, gotEvents[audit.EventPairCreated])
	assert.True(t, gotEvents[audit.EventKeyMinted])
	assert.True(t, gotEvents[audit.EventKeyRotated])
	assert.True(t, gotEvents[audit.EventPairRevoked])
}

func TestCompanionPairings_QRPayloadEncodedCorrectly(t *testing.T) {
	// Direct unit test on the closure the HTTP layer uses. Easier than
	// extracting a QR from a real response.
	srv, _, _ := newPairingTestServer(t)

	// Build a request with X-Forwarded-* and run the middleware so the
	// returned context has the forwarded values injected.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/companion/pairings", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "dam.example.com")
	var capturedCtx = req.Context()
	wrapped := pairingForwardedMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		capturedCtx = r.Context()
	}))
	wrapped.ServeHTTP(httptest.NewRecorder(), req)

	factory := srv.qrPayloadFor(capturedCtx)
	payload := string(factory("iphone-abc", "secret-key-xyz"))
	assert.True(t, strings.HasPrefix(payload, "branchdam://"))
	assert.Contains(t, payload, "server=https%3A%2F%2Fdam.example.com")
	assert.Contains(t, payload, "key=secret-key-xyz")
	assert.Contains(t, payload, "agent=iphone-abc")
}

// TestCompanionPairings_QRPayloadRoundTripsViaStandardQueryParser pins
// the contract that the mobile app relies on: the QR payload emitted by
// qrPayloadFor() must be parseable by a standard `application/x-www-form
// -urlencoded` parser and yield the exact (unencoded) values that were
// passed in. Guards against accidental double-encoding in qrPayloadFor()
// and pins the format the branchdam-mobile clients (Android QrParser /
// iOS AppleQrParser, see s3ntin3l8/branchdam-mobile#141) decode against.
func TestCompanionPairings_QRPayloadRoundTripsViaStandardQueryParser(t *testing.T) {
	srv, _, _ := newPairingTestServer(t)

	req := httptest.NewRequest(http.MethodPost, "/api/v1/companion/pairings", nil)
	req.Header.Set("X-Forwarded-Proto", "https")
	req.Header.Set("X-Forwarded-Host", "dam.example.com:8443")
	var capturedCtx context.Context
	wrapped := pairingForwardedMiddleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		capturedCtx = r.Context()
	}))
	wrapped.ServeHTTP(httptest.NewRecorder(), req)

	factory := srv.qrPayloadFor(capturedCtx)

	// Case 1: ASCII-safe inputs exercise that server-with-port (`%3A`)
	// round-trips through the encoder/parser pair.
	t.Run("ascii", func(t *testing.T) {
		payload := string(factory("iphone-abc", "secret-key-xyz"))
		require.True(t, strings.HasPrefix(payload, "branchdam://?"), "payload must use query-style form: %q", payload)

		values, err := url.ParseQuery(strings.TrimPrefix(payload, "branchdam://?"))
		require.NoError(t, err, "payload must be standard url-encoded: %q", payload)
		assert.Equal(t, "https://dam.example.com:8443", values.Get("server"))
		assert.Equal(t, "secret-key-xyz", values.Get("key"))
		assert.Equal(t, "iphone-abc", values.Get("agent"))
	})

	// Case 2: friendly labels and API keys frequently contain reserved
	// characters (spaces, `+`, `&`, `=`). If qrPayloadFor() ever swapped
	// url.Values.Encode() for naive string concatenation, this case would
	// leak unescaped `&` / `=` into the body and ParseQuery would parse
	// them as additional parameters — a regression the simpler case
	// would silently miss.
	t.Run("reserved-chars", func(t *testing.T) {
		payload := string(factory("Björn's iPhone", "k&y=with+specials"))
		require.True(t, strings.HasPrefix(payload, "branchdam://?"), "payload must use query-style form: %q", payload)

		values, err := url.ParseQuery(strings.TrimPrefix(payload, "branchdam://?"))
		require.NoError(t, err, "payload must be standard url-encoded: %q", payload)
		assert.Equal(t, "https://dam.example.com:8443", values.Get("server"))
		assert.Equal(t, "k&y=with+specials", values.Get("key"))
		assert.Equal(t, "Björn's iPhone", values.Get("agent"))
	})
}

// pairingIDStr formats a pairing id for use in URL paths without
// pulling in strconv just for this test file's helper.
func pairingIDStr(i int64) string {
	const digits = "0123456789"
	if i == 0 {
		return "0"
	}
	var buf [20]byte
	pos := len(buf)
	for i > 0 {
		pos--
		buf[pos] = digits[i%10]
		i /= 10
	}
	return string(buf[pos:])
}

// ensure sqlcgen is used (some test helpers import it indirectly)
var _ = sqlcgen.DevicePairing{}

func TestCompanionPairings_CredentialsEndpoint(t *testing.T) {
	// Keyless server: pairing_url stays NULL (never plaintext), so the
	// credentials endpoint re-serves only the QR SVG -- apiKey/pairingUrl
	// stay empty (QR-only fallback). keyPreview still comes from the
	// active key row so the SPA can mask instead of claiming Unavailable;
	// secretsConfigured=false labels it "Keyless server" (Hermes r3).
	srv, _, _ := newPairingTestServer(t)

	rec := doAdmin(t, srv, http.MethodPost, "/api/v1/companion/pairings",
		map[string]string{"friendlyLabel": "Credentials iPhone"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var created struct {
		PairingID  int64  `json:"pairingId"`
		APIKey     string `json:"apiKey"`
		KeyPreview string `json:"keyPreview"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	rec = doAdmin(t, srv, http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/credentials", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "private, no-store, max-age=0", rec.Header().Get("Cache-Control"))

	var creds struct {
		APIKey            string `json:"apiKey"`
		KeyPreview        string `json:"keyPreview"`
		PairingURL        string `json:"pairingUrl"`
		QRSVG             string `json:"qrSvg"`
		SecretsConfigured bool   `json:"secretsConfigured"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &creds))
	assert.Empty(t, creds.APIKey, "keyless server must not expose apiKey")
	assert.Equal(t, created.KeyPreview, creds.KeyPreview, "keyPreview must come from the active key row")
	assert.NotEmpty(t, creds.KeyPreview, "keyless row still has a last-4 (Hermes r3)")
	assert.Empty(t, creds.PairingURL)
	assert.False(t, creds.SecretsConfigured, "keyless server has no BRANCHDAM_SECRET_KEY")
	assert.Contains(t, creds.QRSVG, "<svg")
	assert.NotEqual(t, created.APIKey, creds.APIKey, "create-time apiKey must not be re-served keyless")
}

func TestCompanionPairings_CredentialsEndpointWithBoxReturnsAPIKey(t *testing.T) {
	// Sealed box: pairing_url is stored sealed and reopened so apiKey /
	// keyPreview / pairingUrl round-trip the create-time credential.
	// secretsConfigured=true so an empty pairingUrl on a keyed server
	// would label as legacy rather than keyless (Hermes r3).
	box, err := secrets.NewBox("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	require.NoError(t, err)
	srv, _, _ := newPairingTestServerWithBox(t, box)

	rec := doAdmin(t, srv, http.MethodPost, "/api/v1/companion/pairings",
		map[string]string{"friendlyLabel": "Sealed credentials"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var created struct {
		PairingID int64  `json:"pairingId"`
		APIKey    string `json:"apiKey"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotEmpty(t, created.APIKey)

	rec = doAdmin(t, srv, http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/credentials", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	assert.Equal(t, "private, no-store, max-age=0", rec.Header().Get("Cache-Control"))

	var creds struct {
		APIKey            string `json:"apiKey"`
		KeyPreview        string `json:"keyPreview"`
		PairingURL        string `json:"pairingUrl"`
		QRSVG             string `json:"qrSvg"`
		SecretsConfigured bool   `json:"secretsConfigured"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &creds))
	assert.Equal(t, created.APIKey, creds.APIKey)
	assert.Equal(t, created.APIKey[len(created.APIKey)-4:], creds.KeyPreview)
	assert.Contains(t, creds.PairingURL, created.APIKey)
	assert.True(t, creds.SecretsConfigured, "keyed server reports SecretsConfigured")
	assert.Contains(t, creds.QRSVG, "<svg")
}

func TestCompanionPairings_CredentialsEndpointLegacyNullURL(t *testing.T) {
	// Keyed server, pre-00034 legacy row: pairing_url is still NULL even
	// though the box is set. apiKey/pairingUrl stay empty, but keyPreview
	// is populated and secretsConfigured=true so the SPA labels
	// "Legacy row -- rotate to seal" instead of Unavailable (Hermes r3).
	box, err := secrets.NewBox("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=")
	require.NoError(t, err)
	srv, database, _ := newPairingTestServerWithBox(t, box)

	rec := doAdmin(t, srv, http.MethodPost, "/api/v1/companion/pairings",
		map[string]string{"friendlyLabel": "Legacy row"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var created struct {
		PairingID  int64  `json:"pairingId"`
		APIKey     string `json:"apiKey"`
		KeyPreview string `json:"keyPreview"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	require.NotEmpty(t, created.APIKey)

	// Simulate a pre-migration row: NULL out pairing_url.
	_, err = database.ExecInTx(context.Background(),
		"UPDATE device_pairings SET pairing_url = NULL WHERE id = ?", created.PairingID)
	require.NoError(t, err)

	rec = doAdmin(t, srv, http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/credentials", nil)
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())

	var creds struct {
		APIKey            string `json:"apiKey"`
		KeyPreview        string `json:"keyPreview"`
		PairingURL        string `json:"pairingUrl"`
		QRSVG             string `json:"qrSvg"`
		SecretsConfigured bool   `json:"secretsConfigured"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &creds))
	assert.Empty(t, creds.APIKey, "legacy NULL pairing_url cannot re-serve apiKey")
	assert.Equal(t, created.KeyPreview, creds.KeyPreview, "legacy row still has a last-4 from the key row")
	assert.Empty(t, creds.PairingURL)
	assert.True(t, creds.SecretsConfigured, "keyed server with NULL url is legacy, not keyless")
	assert.Contains(t, creds.QRSVG, "<svg")
}

func TestCompanionPairings_CredentialsNotFound(t *testing.T) {
	srv, _, _ := newPairingTestServer(t)
	rec := doAdmin(t, srv, http.MethodGet, "/api/v1/companion/pairings/99999/credentials", nil)
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCompanionPairings_CredentialsRevokedReturns410(t *testing.T) {
	srv, _, pairSvc := newPairingTestServer(t)
	ctx := context.Background()
	p, _, err := pairSvc.CreatePairing(ctx, "Revoked", "test", 0, func(agentID, apiKey string) []byte {
		return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
	})
	require.NoError(t, err)
	_, err = pairSvc.RevokePairing(ctx, p.ID, "test")
	require.NoError(t, err)

	rec := doAdmin(t, srv, http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(p.ID)+"/credentials", nil)
	assert.Equal(t, http.StatusGone, rec.Code)
}

func TestCompanionPairings_RenamePairing(t *testing.T) {
	srv, _, _ := newPairingTestServer(t)

	rec := doAdmin(t, srv, http.MethodPost, "/api/v1/companion/pairings",
		map[string]string{"friendlyLabel": "Before"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var created struct {
		PairingID int64 `json:"pairingId"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	rec = doAdmin(t, srv, http.MethodPost,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/rename",
		map[string]string{"friendlyLabel": "After"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var renamed struct {
		FriendlyLabel string `json:"friendlyLabel"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &renamed))
	assert.Equal(t, "After", renamed.FriendlyLabel)

	// Empty label rejected by Huma minLength (422) before the handler.
	rec = doAdmin(t, srv, http.MethodPost,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/rename",
		map[string]string{"friendlyLabel": ""})
	assert.Equal(t, http.StatusUnprocessableEntity, rec.Code)
}

func TestCompanionPairings_RenameNotFound(t *testing.T) {
	srv, _, _ := newPairingTestServer(t)
	rec := doAdmin(t, srv, http.MethodPost, "/api/v1/companion/pairings/99999/rename",
		map[string]string{"friendlyLabel": "x"})
	assert.Equal(t, http.StatusNotFound, rec.Code)
}

func TestCompanionPairings_QRSVGRecordsRevealAudit(t *testing.T) {
	srv, database, _ := newPairingTestServer(t)

	rec := doAdmin(t, srv, http.MethodPost, "/api/v1/companion/pairings",
		map[string]string{"friendlyLabel": "Reveal audit"})
	require.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	var created struct {
		PairingID int64 `json:"pairingId"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))

	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/qr.svg", nil)
	req.Header.Set("X-Authentik-Username", "test-admin")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req)
	require.Equal(t, http.StatusOK, rec2.Code)

	count, err := database.Reader.CountPairingAudit(context.Background(), created.PairingID)
	require.NoError(t, err)
	// PAIR_CREATED + KEY_MINTED + CREDENTIALS_REVEALED
	assert.Equal(t, int64(3), count)
}

// TestCompanionPairings_CredentialsRequiresAdmin pins requireSettingsAdmin
// on both credential-reveal routes. RequireAdmin's global GET bypass used
// to leave GET /credentials (and GET qr.svg) wide open -- probed 200 with
// no identity headers while POST /rename correctly 403'd. Hermes review
// on PR #479.
func TestCompanionPairings_CredentialsRequiresAdmin(t *testing.T) {
	srv, _, pairSvc := newPairingTestServer(t)
	ctx := context.Background()
	p, _, err := pairSvc.CreatePairing(ctx, "Gated", "test", 0, func(agentID, apiKey string) []byte {
		return []byte("branchdam://server=http://test&key=" + apiKey + "&agent=" + agentID)
	})
	require.NoError(t, err)

	credsPath := "/api/v1/companion/pairings/" + pairingIDStr(p.ID) + "/credentials"
	qrPath := "/api/v1/companion/pairings/" + pairingIDStr(p.ID) + "/qr.svg"

	t.Run("credentials no identity headers forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, credsPath, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	})

	t.Run("qr.svg no identity headers forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, qrPath, nil)
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	})

	t.Run("credentials unauthenticated browser principal forbidden", func(t *testing.T) {
		// BrowserChain attaches a principal even with no Authentik
		// headers; Authenticated stays false, which IsAdmin rejects.
		req := httptest.NewRequest(http.MethodGet, credsPath, nil)
		req.Header.Set("X-Authentik-Groups", "dam-users")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	})

	t.Run("credentials non-admin forbidden", func(t *testing.T) {
		srv.cfg().Authz.Groups = []string{"dam-admins"}
		t.Cleanup(func() { srv.cfg().Authz.Groups = nil })

		req := httptest.NewRequest(http.MethodGet, credsPath, nil)
		req.Header.Set("X-Authentik-Username", "alice")
		req.Header.Set("X-Authentik-Groups", "dam-users")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusForbidden, rec.Code, "body=%s", rec.Body.String())
	})

	t.Run("credentials machine principal forbidden", func(t *testing.T) {
		// Direct handler call: companion routes run BrowserChain, which
		// never produces KindMachine from X-API-Key alone. This pins
		// requireSettingsAdmin's KindMachine branch for this surface.
		machineCtx := auth.WithPrincipal(ctx, auth.Principal{
			Kind: auth.KindMachine, Name: "agent-1", Authenticated: true,
		})
		_, err := srv.handlePairingCredentials(machineCtx, &GetPairingInput{ID: p.ID})
		var statusErr huma.StatusError
		require.ErrorAs(t, err, &statusErr)
		assert.Equal(t, http.StatusForbidden, statusErr.GetStatus())
	})

	t.Run("credentials admin still allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, credsPath, nil)
		req.Header.Set("X-Authentik-Username", "test-admin")
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		assert.Equal(t, http.StatusOK, rec.Code, "body=%s", rec.Body.String())
	})
}

// TestPairingCredentialError pins pairingCredentialError's status map
// (Hermes: docs mis-attributed 409 to revoked/expired; the two secrets.*
// arms had no coverage at any layer).
func TestPairingCredentialError(t *testing.T) {
	cases := []struct {
		name   string
		err    error
		status int
	}{
		{"not found", pairing.ErrPairingNotFound, http.StatusNotFound},
		{"no active key", pairing.ErrNoActiveKey, http.StatusGone},
		{"decrypt failed", secrets.ErrDecryptFailed, http.StatusConflict},
		{"key unavailable", secrets.ErrUnavailable, http.StatusInternalServerError},
		{"default", errors.New("boom"), http.StatusInternalServerError},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pairingCredentialError(tc.err)
			var statusErr huma.StatusError
			require.ErrorAs(t, got, &statusErr)
			assert.Equal(t, tc.status, statusErr.GetStatus())
		})
	}
}
