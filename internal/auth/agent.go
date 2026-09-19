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

// KeyLookupResult is what AgentConfig.LookupKey returns for an
// authenticated paired device: the server-minted agent_id that should
// be attached to the Principal, plus the per-device HMAC key material
// that issue #453 PR C wires into the signature validator so signed
// requests from paired devices validate against a key the client
// actually has (rather than the env-var shared secret, which
// pairing-only deployments don't set).
//
// Defined here (the consumer) rather than in internal/pairing to keep
// the import graph acyclic: internal/pairing imports internal/audit
// (for audit logging on key events) and internal/audit imports
// internal/auth (for auth.Principal types) -- so internal/auth cannot
// import internal/pairing. The pairing.Service.KeyLookup method
// constructs and returns this type directly.
type KeyLookupResult struct {
	AgentID string
	// SigningKey is the HMAC-SHA256 key bytes the validator uses to
	// verify X-Signature from this device. Derived per-lookup in
	// internal/pairing.Service.signingKeyFor from the presented
	// plaintext (HMAC-SHA256(pepper, "sign:" + plaintext) -- distinct
	// label from the lookup hash so the two outputs can't collide
	// under the same pepper). Never persisted: only the device_pairing_keys
	// row holds the lookup hash, so a database-only compromise can't
	// reconstruct the signing key without the pepper.
	SigningKey []byte
}

// MinAgentKeyLength matches config.example.yaml's documented requirement.
// A shorter (or unset) key fails every agent request closed rather than
// accepting a weak or empty secret. Exported so internal/settings can
// validate agent.apiKey at save time instead of letting a too-short value
// brick every agent route (including paired devices) at the next restart.
const MinAgentKeyLength = 32

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

	// LookupKey resolves a presented X-API-Key value to the agent_id of
	// the paired device it authenticates AND the per-device HMAC key
	// material that signed requests from that device validate against
	// (issue #453 PR C). Returns an empty KeyLookupResult + nil error
	// when no active key matches. Wired at server startup from
	// internal/pairing.Service's KeyLookup method -- see
	// cmd/branchdam/main.go. When LookupKey is nil, ONLY the env-var
	// APIKey authenticates agent routes (legacy behavior before
	// #companion-pairing). When LookupKey is non-nil, both paths
	// authenticate independently: env-var authenticates as
	// Principal{Name: "env-bootstrap"}; LookupKey hit authenticates as
	// Principal{Name: <agent_id>} and the returned SigningKey is the
	// HMAC key for that device's signed requests. Both attach
	// KindMachine.
	//
	// Contract: every LookupKey hit MUST return a non-nil SigningKey.
	// A nil SigningKey on a hit causes AgentChain to fail closed
	// (reject the request with 401) rather than silently fall back to
	// cfg.APIKey -- the fallback would re-open the env-key/paired-
	// device cross-signing authority issue #453 PR C removes. The
	// wired internal/pairing.Service.KeyLookup satisfies this contract
	// (always returns 32 bytes on a hit, nil on a miss).
	LookupKey func(ctx context.Context, presented string) (result KeyLookupResult, err error)
}

// AgentChain authenticates /api/v1/agent/* requests against a static
// shared secret (spec §5) and attaches a machine Principal.
func AgentChain(apiKey string, log *slog.Logger) func(http.Handler) http.Handler {
	return AgentChainWithConfig(AgentConfig{APIKey: apiKey}, log)
}

// AgentChainWithConfig builds the agent auth middleware using the supplied AgentConfig.
// When SignedRequests is true, it verifies X-Timestamp, X-Nonce, and X-Signature
// (HMAC-SHA256 over method\npath\nnonce\ntimestamp\nbody) within the replay window.
//
// The 503 fail-closed gate is satisfied by EITHER a long-enough env-var key OR
// a wired LookupKey (Companion Pairing). Pairing-only deployments can leave
// agent.apiKey unset without bricking agent routes -- the runtime no longer
// requires the shared secret when an active per-device pairing service is
// wired through cfg.LookupKey (see internal/httpapi/server.go). The shared
// secret is still load-bearing for the env-bootstrap machine principal and
// for HMAC signing when paired clients don't have their own per-device key
// material; tracking the deprecation/removal of that role is issue #453.
func AgentChainWithConfig(cfg AgentConfig, log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	envKeyConfigured := len(cfg.APIKey) >= MinAgentKeyLength
	pairingConfigured := cfg.LookupKey != nil
	keyConfigured := envKeyConfigured || pairingConfigured
	if !keyConfigured {
		log.Warn("auth: no agent authentication configured -- set BRANCHDAM_AGENT_API_KEY or wire Companion Pairing (LookupKey); agent routes will fail closed with 503 until then", "minLength", MinAgentKeyLength)
	}
	// Defensive: signedRequests=true with an empty env-var key would HMAC
	// over []byte("") -- trivially satisfiable by any holder of a valid
	// paired key, so signing gives no defense for a pairing-only
	// deployment. Per-device signing (issue #453 PR C) is the real fix;
	// this warn ensures operators aren't told signing is active when the
	// shared key is empty.
	if cfg.SignedRequests && cfg.APIKey == "" {
		log.Warn("auth: signedRequests=true but agent.apiKey is empty -- signature validation will HMAC over an empty key, which is not a meaningful defense. Pairing-only deployments should leave SignedRequests=false until per-device signing (issue #453 PR C) lands.")
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
			// signingKey holds the HMAC key the signature validator uses
			// below. Populated in the lookup case from result.SigningKey
			// (per-device, derived in internal/pairing.Service.signingKeyFor);
			// in the env-bootstrap case it stays nil and the validator falls
			// back to cfg.APIKey, preserving the historical behavior for
			// any deployment that still uses the shared secret.
			//
			// isPaired distinguishes the two paths so the signature
			// validator can fail closed when a paired LookupKey impl
			// violates its non-nil-on-hit contract (issue #453 PR C
			// follow-up). Silently falling back to cfg.APIKey there would
			// re-open the env-key/paired-device cross-signing authority
			// this PR removes for any future LookupKey wiring.
			var principal Principal
			var signingKey []byte
			var isPaired bool
			switch {
			case provided == "":
				http.Error(w, "invalid or missing "+apiKeyHeader, http.StatusUnauthorized)
				return
			case len(cfg.APIKey) >= MinAgentKeyLength && constantTimeEqual(provided, cfg.APIKey):
				// Env-var bootstrap path (legacy and current operator-migrating
				// install). Authenticates as "env-bootstrap" so audit trails can
				// distinguish a key that was server-wide rotated from a
				// device-scoped key (the env-var key rotates every device at
				// once, by definition).
				//
				// Length-gated on MinAgentKeyLength (not just != "") so a
				// misconfigured-too-short env-var -- which the relaxed gate
				// now lets through when pairing is wired -- never becomes a
				// live machine principal. The settings validator (PR #454)
				// catches this at save time, but the runtime gate cannot
				// rely on the validator having run (config.yaml/env can be
				// edited around it).
				principal = Principal{Kind: KindMachine, Name: "env-bootstrap"}
			case cfg.LookupKey != nil:
				// Device-pairing path. The callback returns the agent_id
				// AND a per-device HMAC signing key derived from the
				// presented plaintext (issue #453 PR C). A non-nil error
				// is a genuine DB failure and propagates as 500.
				result, lookupErr := cfg.LookupKey(r.Context(), provided)
				if lookupErr != nil {
					log.Error("auth: agent key lookup failed", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", sanitizeForLog(r.URL.Path), "err", lookupErr.Error())
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				if result.AgentID == "" {
					http.Error(w, "invalid or missing "+apiKeyHeader, http.StatusUnauthorized)
					return
				}
				principal = Principal{Kind: KindMachine, Name: result.AgentID}
				signingKey = result.SigningKey
				isPaired = true
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
					log.Warn("auth: agent signature rejected", "reason", "missing_header", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", sanitizeForLog(r.URL.Path))
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}

				tsNano, err := strconv.ParseInt(tsStr, 10, 64)
				if err != nil {
					log.Warn("auth: agent signature rejected", "reason", "bad_timestamp_format", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", sanitizeForLog(r.URL.Path))
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}

				now := nowFn()
				reqTime := time.Unix(0, tsNano)
				skew := now.Sub(reqTime)
				if skew > window || skew < -window {
					log.Warn("auth: agent signature rejected", "reason", "clock_skew", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", sanitizeForLog(r.URL.Path), "skew", skew.String(), "window", window.String())
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
							log.Warn("auth: agent signature rejected", "reason", "body_too_large", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", sanitizeForLog(r.URL.Path), "limitBytes", maxBody)
							http.Error(w, "request body exceeds signed body limit", http.StatusRequestEntityTooLarge)
							return
						}
						log.Warn("auth: agent signature rejected", "reason", "body_read_error", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", sanitizeForLog(r.URL.Path), "err", readErr.Error())
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
				//
				// HMAC key selection (issue #453 PR C): paired clients sign
				// with their per-device key (signingKey, populated above);
				// env-bootstrap clients continue to sign with cfg.APIKey.
				// A paired-only deployment with cfg.APIKey == "" would have
				// signingKey != nil and the env-bootstrap fallback never
				// runs -- which is what makes signedRequests=true safe in
				// pairing-only deployments.
				//
				// Fail closed on a paired-path nil SigningKey: the
				// LookupKey contract requires a non-nil signing key on
				// every hit, and silently downgrading to cfg.APIKey here
				// would re-open the env-key/paired-device cross-signing
				// this PR removes for any future LookupKey wiring that
				// breaks the contract.
				hmacKey := signingKey
				if hmacKey == nil {
					if isPaired {
						log.Error("auth: agent signature rejected (paired-path lookup returned nil signing key)", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", sanitizeForLog(r.URL.Path))
						http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
						return
					}
					hmacKey = []byte(cfg.APIKey)
				}
				mac := hmac.New(sha256.New, hmacKey)
				mac.Write([]byte(r.Method + "\n" + r.URL.RequestURI() + "\n" + nonce + "\n" + tsStr + "\n"))
				mac.Write(bodyBytes)
				expectedSig := hex.EncodeToString(mac.Sum(nil))

				if !constantTimeEqual(sig, expectedSig) {
					log.Warn("auth: agent signature rejected", "reason", "signature_mismatch", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", sanitizeForLog(r.URL.Path))
					http.Error(w, "invalid or missing signature", http.StatusUnauthorized)
					return
				}

				expiresAt := reqTime.Add(window)
				if !cache.CheckAndRecord(nonce, expiresAt, now) {
					log.Warn("auth: agent signature rejected", "reason", "replayed_nonce", "remoteAddr", r.RemoteAddr, "method", r.Method, "path", sanitizeForLog(r.URL.Path))
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

// sanitizeForLog replaces CR/LF in a user-controlled value (here,
// r.URL.Path) with their visible escape sequences before it's written to a
// log record. A percent-encoded CR/LF (%0d%0a) is decoded back by
// net/url into r.URL.Path, so this is a real client-controlled
// forged-log-entry vector. Replacing rather than deleting keeps the
// injection visible as literal `\r`/`\n` instead of silently concatenating
// the forged suffix (CWE-117).
func sanitizeForLog(s string) string {
	s = strings.ReplaceAll(s, "\r", `\r`)
	return strings.ReplaceAll(s, "\n", `\n`)
}
