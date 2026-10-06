package httpapi

import (
	"encoding/json"
	"net/http"
	"net/url"
	"strings"

	"github.com/s3ntin3l8/branchdam/internal/auth"
)

// crossSiteGuard rejects state-changing requests that a browser reports as
// cross-site. Credentials here are ambient (the local session cookie, or
// the identity-provider cookie behind ForwardAuth), so SameSite=Lax alone
// still lets a page on a sibling subdomain -- which is "same-site" -- fire
// body-less POSTs such as /restart, trash and revoke.
//
// Decision, for POST/PUT/PATCH/DELETE outside the machine /agent/ routes:
//   - Sec-Fetch-Site present: only "same-origin" and "none" (user-initiated)
//     pass. This header is set by every current browser and cannot be
//     forged by page script.
//   - Else Origin present: its host must equal the request's effective host
//     (X-Forwarded-Host honoured only from trusted proxies).
//   - Neither header (curl, native companion app, PAT tooling): allowed;
//     those clients don't carry browser-ambient credentials.
func (s *Server) crossSiteGuard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(r.URL.Path, auth.AgentPathPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if site := r.Header.Get("Sec-Fetch-Site"); site != "" {
			if site == "same-origin" || site == "none" {
				next.ServeHTTP(w, r)
				return
			}
			writeJSONError(w, http.StatusForbidden, "cross-site request refused")
			return
		}
		if origin := r.Header.Get("Origin"); origin != "" {
			u, err := url.Parse(origin)
			want, werr := url.Parse(requestOrigin(r, currentTrustedProxies(s)))
			if err != nil || werr != nil || !strings.EqualFold(u.Host, want.Host) {
				writeJSONError(w, http.StatusForbidden, "cross-origin request refused")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// maxSmallJSONBody caps the hand-rolled JSON endpoints (login, setup,
// password reset, MFA). They are reachable before authentication, and an
// unbounded json.Decoder would buffer whatever an anonymous client sends.
const maxSmallJSONBody = 64 << 10

// decodeSmallJSON decodes a size-limited JSON request body into v.
func decodeSmallJSON(w http.ResponseWriter, r *http.Request, v any) error {
	return json.NewDecoder(http.MaxBytesReader(w, r.Body, maxSmallJSONBody)).Decode(v)
}
