package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/pairing"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// newPairingTestServer wires a Server with the pairing service enabled.
// Returns srv, database, pairing service. The pairing service is exposed
// so tests can seed state directly (faster than driving HTTP for setup).
func newPairingTestServer(t *testing.T) (*Server, *db.DB, *pairing.Service) {
	t.Helper()
	root := t.TempDir()
	dbPath := root + "/pairing_http.db"
	database, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	pairSvc := pairing.NewService(database, nil, nil)
	srv := New(Deps{
		Config:  &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
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
		QRSVG         string `json:"qrSvg"`
		CreatedAtUnix int64  `json:"createdAtUnix"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &created))
	assert.NotZero(t, created.PairingID)
	assert.NotEmpty(t, created.AgentID)
	assert.NotEmpty(t, created.APIKey)
	assert.Contains(t, created.QRSVG, "<svg")

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
		QRSVG                string `json:"qrSvg"`
		PreviousKeyExpiresAt int64  `json:"previousKeyExpiresAtUnix"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &rotated))
	assert.NotEqual(t, created.APIKey, rotated.APIKey)

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
	srv, _, _ := newPairingTestServer(t)

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

	// GET the qr.svg directly via the mux route
	req := httptest.NewRequest(http.MethodGet,
		"/api/v1/companion/pairings/"+pairingIDStr(created.PairingID)+"/qr.svg", nil)
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
	var audit struct {
		Events []struct {
			Actor string `json:"actor"`
			Event string `json:"event"`
		} `json:"events"`
		Total int64 `json:"total"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &audit))
	assert.Equal(t, int64(4), audit.Total, "PAIR_CREATED, KEY_MINTED, KEY_ROTATED, PAIR_REVOKED")
	gotEvents := make(map[string]bool, len(audit.Events))
	for _, e := range audit.Events {
		gotEvents[e.Event] = true
	}
	assert.True(t, gotEvents["PAIR_CREATED"])
	assert.True(t, gotEvents["KEY_MINTED"])
	assert.True(t, gotEvents["KEY_ROTATED"])
	assert.True(t, gotEvents["PAIR_REVOKED"])
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
