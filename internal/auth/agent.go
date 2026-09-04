package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
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

const (
	apiKeyHeader    = "X-API-Key"
	timestampHeader = "X-Timestamp"
	nonceHeader     = "X-Nonce"
	signatureHeader = "X-Signature"
)

// AgentConfig bundles the configuration options for the agent authentication chain.
type AgentConfig struct {
	APIKey         string
	SignedRequests bool
	ReplayWindow   time.Duration
	Now            func() time.Time // optional clock override for testing clock skew
	Cache          *ReplayCache     // optional in-memory replay cache
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

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stripAuthentikHeaders(r)

			if !keyConfigured {
				http.Error(w, "agent authentication is not configured", http.StatusServiceUnavailable)
				return
			}

			provided := r.Header.Get(apiKeyHeader)
			if provided == "" || !constantTimeEqual(provided, cfg.APIKey) {
				http.Error(w, "invalid or missing "+apiKeyHeader, http.StatusUnauthorized)
				return
			}

			if cfg.SignedRequests {
				tsStr := r.Header.Get(timestampHeader)
				nonce := r.Header.Get(nonceHeader)
				sig := r.Header.Get(signatureHeader)

				if tsStr == "" || nonce == "" || sig == "" {
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}

				tsNano, err := strconv.ParseInt(tsStr, 10, 64)
				if err != nil {
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}

				now := nowFn()
				reqTime := time.Unix(0, tsNano)
				skew := now.Sub(reqTime)
				if skew > window || skew < -window {
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}

				var bodyBytes []byte
				if r.Body != nil {
					var readErr error
					bodyBytes, readErr = io.ReadAll(r.Body)
					if readErr != nil {
						http.Error(w, "failed to read request body", http.StatusBadRequest)
						return
					}
					r.Body = io.NopCloser(bytes.NewReader(bodyBytes))
				}

				mac := hmac.New(sha256.New, []byte(cfg.APIKey))
				mac.Write([]byte(r.Method + "\n" + r.URL.RequestURI() + "\n" + nonce + "\n" + tsStr + "\n"))
				mac.Write(bodyBytes)
				expectedSig := hex.EncodeToString(mac.Sum(nil))

				if !constantTimeEqual(sig, expectedSig) {
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}

				expiresAt := reqTime.Add(window)
				if !cache.CheckAndRecord(nonce, expiresAt, now) {
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}
			}

			principal := Principal{Kind: KindMachine}
			next.ServeHTTP(w, r.WithContext(withPrincipal(r.Context(), principal)))
		})
	}
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
