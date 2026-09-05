package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// minAgentKeyLength matches config.example.yaml's documented requirement.
// A shorter (or unset) key fails every agent request closed rather than
// accepting a weak or empty secret.
const minAgentKeyLength = 32

// defaultSignedMaxBodyBytes caps the request body the signature validator is
// willing to buffer in memory. Every signed agent endpoint other than the
// upload stream is JSON; 16 MiB is comfortably above any realistic JSON
// payload (handshake + node-status are KiB-sized) and bounded well below
// the 50 GiB upload engine default that the streaming upload route streams
// through -- the upload route is exempt from signature validation entirely
// (see defaultSkipSignaturePaths) precisely because of this asymmetry.
const defaultSignedMaxBodyBytes int64 = 16 * 1024 * 1024

// defaultSkipSignaturePaths lists the agent endpoints that do NOT require
// request signature validation. /api/v1/agent/upload is exempt because the
// agent client (s3ntin3l8/branchdam-agent) never signs the streaming
// upload -- it fully buffers its JSON-bound post/get but the upload body
// is the binary stream itself, and the canonical-string contract
// (method\npath\nnonce\ntimestamp\nbody) was never defined for it.
// Forcing signature validation on upload would either regress the
// streaming optimization (PR #390) by demanding full in-memory buffering
// of up to 50 GiB, or fail every legitimate upload. Operators may add
// additional paths via agent.skipSignaturePaths in config.yaml; matching
// is by exact path or strings.HasPrefix when the entry ends in '/'.
var defaultSkipSignaturePaths = []string{
	"/api/v1/agent/upload",
}

const (
	apiKeyHeader    = "X-API-Key"
	timestampHeader = "X-Timestamp"
	nonceHeader     = "X-Nonce"
	signatureHeader = "X-Signature"
)

// AgentConfig bundles the configuration options for the agent authentication chain.
type AgentConfig struct {
	APIKey             string
	SignedRequests     bool
	ReplayWindow       time.Duration
	SignedMaxBodyBytes int64            // max body bytes the signature validator buffers (0 = defaultSignedMaxBodyBytes)
	SkipSignaturePaths []string         // paths that bypass signature validation (default defaultSkipSignaturePaths)
	Now                func() time.Time // optional clock override for testing clock skew
	Cache              *ReplayCache     // optional in-memory replay cache

	// LookupKey resolves a presented X-API-Key value to the agent_id of the
	// paired device it authenticates, or empty string + nil when no active
	// key matches. Wired at server startup from internal/pairing.Service's
	// KeyLookup method -- see cmd/branchdam/main.go. When LookupKey is nil,
	// ONLY the env-var APIKey authenticates agent routes (legacy behavior
	// before #companion-pairing). When LookupKey is non-nil, both paths
	// authenticate independently: env-var authenticates as
	// Principal{Name: "env-bootstrap"}; LookupKey hit authenticates as
	// Principal{Name: <agent_id>}. Both attach KindMachine.
	LookupKey func(ctx context.Context, presented string) (agentID string, err error)
}

// AgentChain authenticates /api/v1/agent/* requests against a static
// shared secret (spec §5) and attaches a machine Principal.
func AgentChain(apiKey string, log *slog.Logger) func(http.Handler) http.Handler {
	return AgentChainWithConfig(AgentConfig{APIKey: apiKey}, log)
}

// AgentChainWithConfig builds the agent auth middleware using the supplied AgentConfig.
// When SignedRequests is true, it verifies X-Timestamp, X-Nonce, and X-Signature
// (HMAC-SHA256 over method\npath\nnonce\ntimestamp\nbody) within the replay window.
func AgentChainWithConfig(cfg AgentConfig, log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	keyConfigured := len(cfg.APIKey) >= minAgentKeyLength
	if !keyConfigured {
		log.Warn("auth: BRANCHDAM_AGENT_API_KEY is unset or shorter than the minimum length -- agent routes will fail closed with 503 until it is fixed", "minLength", minAgentKeyLength)
	}

	window := cfg.ReplayWindow
	if window <= 0 {
		window = 5 * time.Minute
	}
	cache := cfg.Cache
	if cache == nil {
		cache = NewReplayCache()
	}
	nowFn := cfg.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	maxBody := cfg.SignedMaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultSignedMaxBodyBytes
	}
	skipPaths := cfg.SkipSignaturePaths
	if skipPaths == nil {
		skipPaths = defaultSkipSignaturePaths
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stripAuthentikHeaders(r)

			if !keyConfigured {
				http.Error(w, "agent authentication is not configured", http.StatusServiceUnavailable)
				return
			}

			provided := r.Header.Get(apiKeyHeader)
			var principal Principal
			switch {
			case provided == "":
				http.Error(w, "invalid or missing "+apiKeyHeader, http.StatusUnauthorized)
				return
			case cfg.APIKey != "" && constantTimeEqual(provided, cfg.APIKey):
				// Env-var bootstrap path (legacy and current operator-migrating
				// install). Authenticates as "env-bootstrap" so audit trails can
				// distinguish a key that was server-wide rotated from a
				// device-scoped key (the env-var key rotates every device at
				// once, by definition).
				principal = Principal{Kind: KindMachine, Name: "env-bootstrap"}
			case cfg.LookupKey != nil:
				// Device-pairing path. The callback returns the agent_id for
				// any active (non-expired, non-revoked, parent-pairing-non-revoked)
				// key, or empty string when no match. A non-nil error is a
				// genuine DB failure and propagates as 500.
				agentID, lookupErr := cfg.LookupKey(r.Context(), provided)
				if lookupErr != nil {
					log.Error("auth: agent key lookup failed", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "err", lookupErr.Error())
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				if agentID == "" {
					http.Error(w, "invalid or missing "+apiKeyHeader, http.StatusUnauthorized)
					return
				}
				principal = Principal{Kind: KindMachine, Name: agentID}
			default:
				http.Error(w, "invalid or missing "+apiKeyHeader, http.StatusUnauthorized)
				return
			}

			// The API key check above always runs. Signature validation is
			// skipped only for endpoints the agent client cannot satisfy the
			// canonical-string contract for (see defaultSkipSignaturePaths).
			requireSignature := cfg.SignedRequests && !matchesAnyPath(r.URL.Path, skipPaths)

			if requireSignature {
				tsStr := r.Header.Get(timestampHeader)
				nonce := r.Header.Get(nonceHeader)
				sig := r.Header.Get(signatureHeader)

				if tsStr == "" || nonce == "" || sig == "" {
					log.Warn("auth: agent signature rejected", "reason", "missing_header", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}

				tsNano, err := strconv.ParseInt(tsStr, 10, 64)
				if err != nil {
					log.Warn("auth: agent signature rejected", "reason", "bad_timestamp_format", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}

				now := nowFn()
				reqTime := time.Unix(0, tsNano)
				skew := now.Sub(reqTime)
				if skew > window || skew < -window {
					log.Warn("auth: agent signature rejected", "reason", "clock_skew", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "skew", skew.String(), "window", window.String())
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}

				var bodyBytes []byte
				if r.Body != nil {
					// http.MaxBytesReader caps the body the signature
					// validator is willing to buffer in memory; any read
					// past the cap returns *http.MaxBytesError so the
					// generic ReadAll error path below can return 413
					// instead of an arbitrary 400.
					r.Body = http.MaxBytesReader(w, r.Body, maxBody)
					var readErr error
					bodyBytes, readErr = io.ReadAll(r.Body)
					if readErr != nil {
						var maxBytesErr *http.MaxBytesError
						if errors.As(readErr, &maxBytesErr) {
							log.Warn("auth: agent signature rejected", "reason", "body_too_large", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "limitBytes", maxBody)
							http.Error(w, "request body exceeds signed body limit", http.StatusRequestEntityTooLarge)
							return
						}
						log.Warn("auth: agent signature rejected", "reason", "body_read_error", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path, "err", readErr.Error())
						http.Error(w, "failed to read request body", http.StatusBadRequest)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				}

				// r.URL.RequestURI() returns the raw path + query string as
				// it appeared on the wire (decoded by Go's HTTP parser but
				// not re-encoded), matching the agent client's signing
				// contract in internal/branchdam.Client.signRequest. Using
				// r.URL.Path here would silently drop the query and break
				// signed-query endpoints (e.g. /agent/check-content?fastHash=...)
				// without any error.
				mac := hmac.New(sha256.New, []byte(cfg.APIKey))
				mac.Write([]byte(r.Method + "\n" + r.URL.RequestURI() + "\n" + nonce + "\n" + tsStr + "\n"))
				mac.Write(bodyBytes)
				expectedSig := hex.EncodeToString(mac.Sum(nil))

				if !constantTimeEqual(sig, expectedSig) {
					log.Warn("auth: agent signature rejected", "reason", "signature_mismatch", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}

				expiresAt := reqTime.Add(window)
				if !cache.CheckAndRecord(nonce, expiresAt, now) {
					log.Warn("auth: agent signature rejected", "reason", "replayed_nonce", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", r.URL.Path)
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}
			}

			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), principal)))
		})
	}
}

// matchesAnyPath reports whether path matches any prefix in patterns. An
// entry ending in '/' is treated as a path prefix (e.g. "/api/v1/agent/"
// matches "/api/v1/agent/upload/foo"); all other entries are matched by
// exact equality. This is intentionally simpler than http.ServeMux's
// pattern grammar -- the agent endpoint set is small and adding an exotic
// glob language is more risk than reward.
func matchesAnyPath(path string, patterns []string) bool {
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if strings.HasSuffix(p, "/") {
			if strings.HasPrefix(path, p) {
				return true
			}
			continue
		}
		if path == p {
			return true
		}
	}
	return false
}

// stripAuthentikHeaders deletes every header whose canonical name starts
// with "X-Authentik-", regardless of how many were sent or what they
// contained. Canonical form (http.CanonicalHeaderKey, which r.Header.Get/
// Del already apply) means this catches "x-authentik-username",
// "X-AUTHENTIK-USERNAME", etc. -- HTTP header names are case-insensitive on
// the wire, and Go's http.Header always stores them canonicalized.
func stripAuthentikHeaders(r *http.Request) {
	for name := range r.Header {
		if strings.HasPrefix(strings.ToLower(name), "x-authentik-") {
			r.Header.Del(name)
		}
	}
}

// constantTimeEqual reports whether a and b are equal without leaking
// timing information about *where* they first differ. subtle.
// ConstantTimeCompare requires equal-length inputs to make that guarantee;
// the length check itself is not constant-time, but leaking "the provided
// key's length doesn't match" is not a meaningful side channel for a fixed
// shared secret an attacker cannot otherwise probe the length of.
func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
