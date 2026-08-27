package httpapi

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/secrets"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/sse"
)

func settingsTestKey(t *testing.T) *secrets.Box {
	t.Helper()
	box, err := secrets.NewBox("MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=") // 32 bytes
	if err != nil {
		t.Fatalf("NewBox: %v", err)
	}
	return box
}

// settingsTestServer builds a Server backed by a real *settings.Store (not
// the static Deps.Config fallback every other test in this package uses),
// so the GET/PUT /api/v1/settings routes have something to operate on.
//
// adminGroups is baked into the STORE'S BASE config, not set by mutating
// srv.cfg() after construction like every other test in this package does
// -- authz.groups is non-editable, but Store.Apply's reload() rebuilds
// Effective() fresh from base on every write (Resolve(&s.base, ...)), so a
// group list applied only to the pointer srv.cfg() happened to return at
// setup time would silently evaporate after the very first successful PUT,
// and every assertion after it would actually be running under the
// permit-all empty-allowedGroups path instead of real group gating.
func settingsTestServer(t *testing.T, box *secrets.Box, adminGroups []string) *Server {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "settings.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	base := config.Config{Authz: config.Authz{Groups: adminGroups}}
	store, err := settings.NewStore(context.Background(), database, base, box, nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}

	// Deps.Config is unread here: with Settings set, cfgProvider wraps the
	// store, so s.cfg() resolves through Store.Effective(), never &base.
	// adminGroups must be baked into the store's base config (above), not
	// set by mutating srv.cfg() after construction -- see
	// TestSettingsGroupGatingSurvivesAWrite for why that would silently
	// reset on the next Apply.
	return New(Deps{
		Settings: store,
		DB:       database,
		Hub:      sse.New(),
		Version:  "test",
	})
}

func settingsGetJSON(body map[string]any) []byte {
	b, _ := json.Marshal(body)
	return b
}

func TestSettingsGetRequiresAdmin(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})

	t.Run("unauthenticated forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusForbidden)
		}
	})

	t.Run("non-admin forbidden", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
		req.Header.Set("X-Authentik-Username", "alice")
		req.Header.Set("X-Authentik-Groups", "dam-users")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusForbidden {
			t.Errorf("status = %d, want %d", rr.Code, http.StatusForbidden)
		}
	})

	t.Run("admin allowed", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
		req.Header.Set("X-Authentik-Username", "bob")
		req.Header.Set("X-Authentik-Groups", "dam-admins")
		rr := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusOK, rr.Body.String())
		}
	})
}

// TestSettingsGroupGatingSurvivesAWrite pins the base-vs-effective-config
// bug class settingsTestServer's doc comment warns about: authz.groups
// lives on Store's base, but Effective() is rebuilt from base on every
// Apply. If group gating were (incorrectly) read off Effective() instead
// of the base config, a successful admin PUT earlier in the test would
// have already proven nothing -- this test performs one first, then
// re-asserts a non-admin is still forbidden afterward.
func TestSettingsGroupGatingSurvivesAWrite(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", settingsGetJSON(map[string]any{
		"set": map[string]any{"immich.apiUrl": "http://immich.example:2283"},
	})))
	if rr.Code != http.StatusOK {
		t.Fatalf("admin PUT status = %d, body=%s", rr.Code, rr.Body.String())
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/settings", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	req.Header.Set("X-Authentik-Groups", "dam-users")
	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, req)
	if rr2.Code != http.StatusForbidden {
		t.Errorf("non-admin GET after a prior write: status = %d, want %d", rr2.Code, http.StatusForbidden)
	}
}

func adminReq(method, path string, body []byte) *http.Request {
	var r *http.Request
	if body != nil {
		r = httptest.NewRequest(method, path, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r = httptest.NewRequest(method, path, nil)
	}
	r.Header.Set("X-Authentik-Username", "admin")
	r.Header.Set("X-Authentik-Groups", "dam-admins")
	return r
}

type settingsFieldsBody struct {
	Fields           []SettingsFieldDTO `json:"fields"`
	PendingRestart   []string           `json:"pendingRestart"`
	SecretsAvailable bool               `json:"secretsAvailable"`
}

func decodeSettingsBody(t *testing.T, rr *httptest.ResponseRecorder) settingsFieldsBody {
	t.Helper()
	var body settingsFieldsBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response body: %v (body=%s)", err, rr.Body.String())
	}
	return body
}

func findField(t *testing.T, body settingsFieldsBody, key string) SettingsFieldDTO {
	t.Helper()
	for _, f := range body.Fields {
		if f.Key == key {
			return f
		}
	}
	t.Fatalf("field %q not found in response", key)
	return SettingsFieldDTO{}
}

func TestSettingsGetListsRegisteredFields(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodGet, "/api/v1/settings", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	body := decodeSettingsBody(t, rr)

	f := findField(t, body, "immich.apiUrl")
	if f.Source != "config" {
		t.Errorf("immich.apiUrl.Source = %q, want %q before any override", f.Source, "config")
	}
	if !f.Editable {
		t.Error("immich.apiUrl.Editable = false, want true")
	}

	groups := findField(t, body, "authz.groups")
	if groups.Editable {
		t.Error("authz.groups.Editable = true, want false")
	}
	if groups.ReadOnlyReason == "" {
		t.Error("authz.groups.ReadOnlyReason is empty, want an explanation")
	}

	if !body.SecretsAvailable {
		t.Error("SecretsAvailable = false, want true (test server has a key configured)")
	}
}

func TestSettingsPutThenGetReflectsOverride(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})

	putBody := settingsGetJSON(map[string]any{
		"set": map[string]any{"immich.apiUrl": "http://immich.example:2283"},
	})
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", putBody))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", rr.Code, rr.Body.String())
	}
	putResponse := decodeSettingsBody(t, rr)
	f := findField(t, putResponse, "immich.apiUrl")
	if f.Source != "override" {
		t.Errorf("immich.apiUrl.Source after PUT = %q, want %q", f.Source, "override")
	}
	if f.Value != "http://immich.example:2283" {
		t.Errorf("immich.apiUrl.Value after PUT = %v, want the new value", f.Value)
	}

	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, adminReq(http.MethodGet, "/api/v1/settings", nil))
	getResponse := decodeSettingsBody(t, rr2)
	f2 := findField(t, getResponse, "immich.apiUrl")
	if f2.Source != "override" || f2.Value != "http://immich.example:2283" {
		t.Errorf("GET after PUT: field = %+v, want override with the new value", f2)
	}
}

func TestSettingsPutUnsetReverts(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", settingsGetJSON(map[string]any{
		"set": map[string]any{"immich.apiUrl": "http://immich.example:2283"},
	})))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT set status = %d, body=%s", rr.Code, rr.Body.String())
	}

	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, adminReq(http.MethodPut, "/api/v1/settings", settingsGetJSON(map[string]any{
		"unset": []string{"immich.apiUrl"},
	})))
	if rr2.Code != http.StatusOK {
		t.Fatalf("PUT unset status = %d, body=%s", rr2.Code, rr2.Body.String())
	}
	body := decodeSettingsBody(t, rr2)
	f := findField(t, body, "immich.apiUrl")
	if f.Source != "config" {
		t.Errorf("immich.apiUrl.Source after revert = %q, want %q", f.Source, "config")
	}
}

func TestSettingsPutSecretNeverReturnsValue(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", settingsGetJSON(map[string]any{
		"set": map[string]any{"immich.apiKey": "top-secret-key"},
	})))
	if rr.Code != http.StatusOK {
		t.Fatalf("PUT status = %d, body=%s", rr.Code, rr.Body.String())
	}
	if bytes.Contains(rr.Body.Bytes(), []byte("top-secret-key")) {
		t.Fatal("PUT response body contains the plaintext secret value")
	}
	body := decodeSettingsBody(t, rr)
	f := findField(t, body, "immich.apiKey")
	if !f.HasValue {
		t.Error("immich.apiKey.HasValue = false, want true after setting it")
	}
	if f.Value != nil {
		t.Errorf("immich.apiKey.Value = %v, want omitted/nil for a secret field", f.Value)
	}

	rr2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr2, adminReq(http.MethodGet, "/api/v1/settings", nil))
	if bytes.Contains(rr2.Body.Bytes(), []byte("top-secret-key")) {
		t.Fatal("GET response body contains the plaintext secret value")
	}
}

func TestSettingsPutRejectsNonEditableField(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", settingsGetJSON(map[string]any{
		"set": map[string]any{"authz.groups": []string{"other-group"}},
	})))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusUnprocessableEntity, rr.Body.String())
	}
}

func TestSettingsPutRejectsInvalidValue(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", settingsGetJSON(map[string]any{
		"set": map[string]any{"logLevel": "verbose"},
	})))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusUnprocessableEntity, rr.Body.String())
	}
}

func TestSettingsPutSecretWithoutKeyReturns422(t *testing.T) {
	srv := settingsTestServer(t, nil, []string{"dam-admins"}) // no secret box configured

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", settingsGetJSON(map[string]any{
		"set": map[string]any{"immich.apiKey": "top-secret-key"},
	})))
	if rr.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want %d, body=%s", rr.Code, http.StatusUnprocessableEntity, rr.Body.String())
	}
	// Pins that the handler surfaces the actionable secrets.ErrUnavailable
	// message, not just settings.ErrInvalidInput's generic wrapper text --
	// the two branches are easy to check in the wrong order since Apply
	// wraps ErrUnavailable inside ErrInvalidInput.
	if !bytes.Contains(rr.Body.Bytes(), []byte("BRANCHDAM_SECRET_KEY")) {
		t.Errorf("body = %s, want it to mention BRANCHDAM_SECRET_KEY", rr.Body.String())
	}
}

func TestSettingsIntFieldAcceptsJSONNumber(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})

	// Marshaled through encoding/json, workers.hashWorkers arrives as a
	// JSON number (float64 once decoded into map[string]any) exactly like
	// a real browser request -- this is normalizeSettingValue's job to
	// convert back to int before it ever reaches internal/settings.
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", settingsGetJSON(map[string]any{
		"set": map[string]any{"workers.hashWorkers": 8},
	})))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}
	body := decodeSettingsBody(t, rr)
	f := findField(t, body, "workers.hashWorkers")
	if f.Value != float64(8) {
		t.Errorf("workers.hashWorkers.Value = %v (%T), want 8", f.Value, f.Value)
	}
	if !f.PendingRestart {
		t.Error("workers.hashWorkers.PendingRestart = false, want true (ApplyRestart field)")
	}
	found := false
	for _, k := range body.PendingRestart {
		if k == "workers.hashWorkers" {
			found = true
		}
	}
	if !found {
		t.Errorf("PendingRestart list = %v, want it to include workers.hashWorkers", body.PendingRestart)
	}
}

func TestSettingsPutBroadcastsOnSuccess(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})
	nudged, unsubscribe := srv.hub.Subscribe()
	defer unsubscribe()

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", settingsGetJSON(map[string]any{
		"set": map[string]any{"immich.apiUrl": "http://immich.example:2283"},
	})))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", rr.Code, rr.Body.String())
	}

	select {
	case <-nudged:
	default:
		t.Error("hub was not nudged after a successful PUT")
	}
}

// TestSettingsConcurrentGetAndPut is the race-detector proof the atomic
// s.cfgProvider/Store.effective refactor exists for: concurrent GETs
// reading s.cfg() must never race with a concurrent PUT rebuilding
// Effective() underneath them. Run with -race.
func TestSettingsConcurrentGetAndPut(t *testing.T) {
	srv := settingsTestServer(t, settingsTestKey(t), []string{"dam-admins"})
	handler := srv.Handler()

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 50; i++ {
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, adminReq(http.MethodGet, "/api/v1/settings", nil))
			if rr.Code != http.StatusOK {
				t.Errorf("concurrent GET status = %d", rr.Code)
			}
		}
	}()

	// Deliberately re-applies the same value on every iteration: Store.Apply
	// has no unchanged-value short-circuit, so each PUT still writes and
	// republishes Effective(), which is what this test needs to race against
	// the concurrent GETs above. If Apply ever grows such a short-circuit,
	// this loop stops exercising the writer path and needs revisiting.
	for i := 0; i < 50; i++ {
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, adminReq(http.MethodPut, "/api/v1/settings", settingsGetJSON(map[string]any{
			"set": map[string]any{"immich.apiUrl": "http://immich.example:2283"},
		})))
		if rr.Code != http.StatusOK {
			t.Errorf("concurrent PUT status = %d, body=%s", rr.Code, rr.Body.String())
		}
	}
	<-done
}
