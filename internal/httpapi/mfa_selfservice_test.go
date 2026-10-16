package httpapi

import (
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testTOTP computes the RFC 6238 code for secret at the given 30s step.
// A local copy: the mfa package's computeTOTP is unexported, and these
// tests drive the HTTP surface the way an authenticator app would.
func testTOTP(t *testing.T, secret string, step int64) string {
	t.Helper()
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	require.NoError(t, err)
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))
	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)
	offset := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	return fmt.Sprintf("%06d", code%1_000_000)
}

func decodeMe(t *testing.T, rr *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	require.Equal(t, http.StatusOK, rr.Code, "GET /me; body = %s", rr.Body.String())
	var me map[string]any
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &me))
	return me
}

// TestMFASelfService_NonAdminEnrolDisable walks a non-admin local user
// through the whole self-service lifecycle the account page drives:
//
//   - setup returns a QR SVG of the otpauth URI next to the URI itself;
//   - enable keeps the enrolling session usable (it is marked verified,
//     so MFAGate doesn't turn it half-authed and the SPA doesn't swap the
//     recovery codes for a challenge form);
//   - /me reports mfaEnabled before and after;
//   - disable (password + code) removes the factor again.
func TestMFASelfService_NonAdminEnrolDisable(t *testing.T) {
	srv, database, svc, _ := mfaGateTestServer(t)
	_, _ = mintSessionForUser(t, database, svc, "admin", true, "local")
	cookie, _ := mintSessionForUser(t, database, svc, "bob", false, "local")

	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodGet, "/api/v1/me", cookie, nil))
	me := decodeMe(t, rr)
	assert.Nil(t, me["mfaEnabled"], "not enrolled yet")

	// --- setup ---
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodPost, "/api/v1/mfa/setup", cookie, []byte(`{}`)))
	require.Equal(t, http.StatusOK, rr.Code, "setup; body = %s", rr.Body.String())
	var setup struct {
		OtpauthURI string `json:"otpauthURI"`
		QRSVG      string `json:"qrSvg"`
	}
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &setup))
	assert.Contains(t, setup.QRSVG, "<svg", "setup must return an SVG QR code of the otpauth URI")
	u, err := url.Parse(setup.OtpauthURI)
	require.NoError(t, err)
	secret := u.Query().Get("secret")
	require.NotEmpty(t, secret)

	// --- enable ---
	step := time.Now().Unix() / 30
	body := fmt.Sprintf(`{"code":%q}`, testTOTP(t, secret, step))
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodPost, "/api/v1/mfa/enable", cookie, []byte(body)))
	require.Equal(t, http.StatusOK, rr.Code, "enable; body = %s", rr.Body.String())

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodGet, "/api/v1/me", cookie, nil))
	me = decodeMe(t, rr)
	assert.Equal(t, true, me["mfaEnabled"], "/me must report the new factor")
	assert.Equal(t, true, me["mfaVerified"], "the enrolling session must count as verified")
	assert.Nil(t, me["mfaRequired"], "the enrolling session must not be sent to the challenge")

	// A non-allowlisted path is not intercepted by the MFA gate.
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodGet, "/api/v1/users", cookie, nil))
	assert.NotContains(t, rr.Body.String(), "mfaChallengeRequired", "code=%d", rr.Code)

	// --- disable ---
	// The next step's code: drift tolerance accepts it, and it can't
	// collide with replay protection on the code enable consumed.
	body = fmt.Sprintf(`{"password":"password123","code":%q}`, testTOTP(t, secret, step+1))
	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodPost, "/api/v1/mfa/disable", cookie, []byte(body)))
	require.Equal(t, http.StatusOK, rr.Code, "disable; body = %s", rr.Body.String())

	rr = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, withSessionCookie(http.MethodGet, "/api/v1/me", cookie, nil))
	me = decodeMe(t, rr)
	assert.Nil(t, me["mfaEnabled"], "disabled factor must no longer be reported")
}
