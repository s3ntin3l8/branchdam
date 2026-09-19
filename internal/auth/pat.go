package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// Compile-time check: *sql.DB satisfies sqlcgen.DBTX.
var _ sqlcgen.DBTX = (*sql.DB)(nil)

// PATPrefix is the literal "bdam_pat_" prefix every minted token
// carries. Grep-able in logs and rotation tooling: an operator can
// `grep bdam_pat_ operator.log` to find every place a token leaked.
// The plaintext-after-prefix is base64-url, no padding, length
// ceil(32*4/3) = 43 chars, total length 51 chars including prefix.
const PATPrefix = "bdam_pat_"

// PAT token format constants -- a freshly minted plaintext looks like:
//
//	bdam_pat_<43 base64-url chars>
//
// The 43 chars encode 32 random bytes (256 bits) -- enough that a
// brute-force search against the SHA-256 lookup hash is not feasible
// regardless of pepper compromise. base64-url no-padding avoids
// reserved chars in URL paths / Authorization headers.
const (
	patRandomBytes     = 32
	patB64URLNoPadding = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
)

// ErrPATInvalid is the typed error returned when a presented token is
// malformed, revoked, expired, or doesn't match a live row. The HTTP
// handler maps this to 401.
var ErrPATInvalid = errors.New("personal access token invalid")

// PATLookupResult is what the Lookup callback returns for an
// authenticated token -- parallel shape to pairing.KeyLookupResult
// (issue #453 PR C). Kept distinct because the auth and pairing
// packages both define it; if a future PR wants one shared
// "LookupResult" shape it can be unified, but two fields and two
// different lookup tables doesn't justify the indirection yet.
type PATLookupResult struct {
	UserID int64
	Scopes []string
}

// PATMiddleware validates an `Authorization: Bearer bdam_pat_...`
// header, attaches the token's owner as the request Principal, and
// enforces the requested scope. Tokens that aren't presented fall
// through to the next auth chain (forward-auth / local-cookie) so
// admin routes can be reached by either path.
//
// Constructor shape mirrors pairing's KeyLookup callback: a single
// lookup function (no db dependency here, so the httpapi package wires
// the db-backed lookup). Scopes are JSON-decoded once at lookup time
// and exposed via Scopes; the middleware compares the requested scope
// against the list on every request, so a token's scopes can be
// revoked by re-minting without touching the middleware.
type PATMiddleware struct {
	Lookup func(ctx context.Context, presented string) (PATLookupResult, error)
	// Now is overridable for tests. Defaults to time.Now.
	Now func() time.Time
	// TouchLastUsed is the optional callback for bumping
	// user_pats.last_used_at on every authenticated request. The
	// middleware calls this async (go func()) so the auth path is
	// never blocked on a write. A flush error is logged but never
	// propagated to the request. Nil = no-op.
	TouchLastUsed func(ctx context.Context, patHash string, at time.Time)
	// Pepper is used to HMAC-SHA256 hash the presented plaintext for
	// lookup. Same pepper as pairing uses for device-pairing key
	// hashes -- shared pepper means a DB-only compromise can't
	// reconstruct either kind of key. Required.
	Pepper []byte
	// Scope is the scope the protected route requires. A token whose
	// scopes_json contains "*" satisfies every scope; otherwise the
	// requested scope must appear in the token's list (string-equal).
	Scope string
}

// RequirePAT returns an http.Handler middleware that authenticates a
// Bearer-token PAT and attaches the owner as the Principal, OR passes
// through (no rejection) to next if no token is presented. Routes that
// require a PAT to even consider the request should call RequirePAT
// *before* RequireAdmin in the route composition; a missing token
// passes through and the downstream auth chain handles the human/cookie
// path the same way it does today.
func (m *PATMiddleware) RequirePAT(next http.Handler) http.Handler {
	if m.Now == nil {
		m.Now = time.Now
	}
	if len(m.Pepper) == 0 {
		panic("auth: PATMiddleware.Pepper is required (use the pairing pepper)")
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		presented := extractBearerPAT(r)
		if presented == "" {
			next.ServeHTTP(w, r)
			return
		}

		hash := hashToken(m.Pepper, presented)
		result, err := m.Lookup(r.Context(), presented)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				http.Error(w, "invalid personal access token", http.StatusUnauthorized)
				return
			}
			// Real DB failure -- log and 500. Same posture as AgentChain
			// when LookupKey returns a non-NoRows error.
			slogDefault().Error("auth: PAT lookup failed", "method", r.Method, "path", r.URL.Path, "err", err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		_ = hash // hash is computed but only used if Lookup is db-backed; kept here for symmetry with pairing.

		// Build a Principal from the token's user. We don't have the
		// user row here (Lookup returns UserID + scopes), so the
		// Principal carries just enough for downstream RequireAdmin
		// and audit attribution. Display fields (Name, Email,
		// AuthProvider) are filled in by RequirePAT's caller if it
		// wants richer audit output -- the typical admin route just
		// needs is_admin=true and a non-empty ExternalUID.
		principal := Principal{
			Kind:          KindUser,
			Name:          fmt.Sprintf("pat:%d", result.UserID),
			ExternalUID:   fmt.Sprintf("pat:%s", hashPrefix(hash)),
			Authenticated: true,
		}
		if !scopeSatisfied(result.Scopes, m.Scope) {
			http.Error(w, fmt.Sprintf("token lacks scope %q", m.Scope), http.StatusForbidden)
			return
		}

		// Best-effort last_used_at bump -- never blocks the request
		// and never propagates a write failure.
		if m.TouchLastUsed != nil {
			at := m.Now()
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				m.TouchLastUsed(ctx, hash, at)
			}()
		}

		// Attach a LocalUserView alongside the Principal. Three
		// consumers depend on it:
		//
		//   - RequireAdmin/IsAdmin: the PAT principal carries no
		//     forward-auth Groups, so without the local-`is_admin`
		//     override every write route would 403 the moment
		//     authz.groups is non-empty. PATs minted by this PR are
		//     admin-only by design, so IsAdmin=true; route-level
		//     granularity comes from the middleware Scope, not from
		//     the admin flag.
		//   - MFAGate: MFAVerified=true so a PAT-authenticated
		//     request isn't mistaken for a half-authed password-only
		//     session -- the token itself is the second factor.
		//   - Handlers: LocalUserView.UserID is the token owner's
		//     users.id, letting /api/v1/users/me/pats resolve the
		//     owner without a second lookup.
		ctx := withPrincipal(r.Context(), principal)
		ctx = WithLocalUserView(ctx, LocalUserView{
			UserID:      result.UserID,
			IsAdmin:     true,
			MFAVerified: true,
		})
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// PATPresented reports whether the request carries a bdam_pat_ Bearer
// token. Exported for the HTTP layer's routing decision: when true
// (and the path isn't an agent route), the request is handed to the
// PAT-authenticated chain directly, bypassing auth.Route's
// forward/local identity extraction -- BrowserChain would otherwise
// overwrite the PAT principal with an unauthenticated empty one.
func PATPresented(r *http.Request) bool { return extractBearerPAT(r) != "" }

// extractBearerPAT pulls the bdam_pat_ token out of the Authorization
// header (if present). Returns empty string for any other auth scheme
// (basic auth, no header, malformed header) -- callers treat empty as
// "no PAT presented, pass through to next chain".
func extractBearerPAT(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if auth == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(auth) <= len(prefix) || !strings.EqualFold(auth[:len(prefix)], prefix) {
		return ""
	}
	token := strings.TrimSpace(auth[len(prefix):])
	if !strings.HasPrefix(token, PATPrefix) {
		return ""
	}
	return token
}

// hashToken returns the hex-encoded HMAC-SHA256 of plaintext under
// pepper. Shared with pairing.Service.hashKey (HMAC under the pairing
// pepper) -- different label prefix on the input side; same pepper
// means a DB-only compromise can't recover either without the pepper.
func hashToken(pepper []byte, plaintext string) string {
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte("pat:" + plaintext))
	return hex.EncodeToString(mac.Sum(nil))
}

// hashPrefix returns the first 8 hex chars of the hashed key -- enough
// to make two PATs distinguishable in logs and actor_audit
// details_json without leaking the full hash.
func hashPrefix(hash string) string {
	if len(hash) < 8 {
		return hash
	}
	return hash[:8]
}

// scopeSatisfied reports whether the token's scopes list covers the
// requested scope. The wildcard "*" satisfies every scope; otherwise
// the requested scope must appear as an exact match in the token's
// list. Empty requested scope ("any authenticated PAT") is always
// satisfied -- the caller is opting out of scope enforcement.
func scopeSatisfied(scopes []string, requested string) bool {
	if requested == "" {
		return true
	}
	for _, s := range scopes {
		if s == "*" || s == requested {
			return true
		}
	}
	return false
}

// slogDefault returns the package-default logger. Extracted so the
// middleware can be constructed without an explicit logger -- admin
// route wiring doesn't carry one through, and the auth-failure log
// is rare enough that the package default suffices. The AgentChain
// pattern (explicit *slog.Logger) would be a future refinement if
// operator-facing logs become a problem.
func slogDefault() *slog.Logger { return slog.Default() }

// PATService is the higher-level wrapper that mints, revokes, and
// looks up tokens. It lives here (not in httpapi) so the cmd/branchdam
// bootstrap mechanism can construct it directly without a circular
// import with the routes that consume it.
//
// Lifecycle: created at server startup with the *db.DB handle and
// the pairing pepper; passed to the HTTP layer (which wires
// RequirePAT) and to the cmd-layer bootstrap code.
//
// All writes route through db.InTx -- the service never exposes a
// write-capable queries handle, preserving the
// TestNoWriteCapableQueriesAccessorOutsideInTx invariant. Single-
// statement writes (mint, revoke, touch) get their own one-shot tx;
// that's slightly heavier than a bare exec but keeps every write on
// the single-connection writer pool's transaction path, identical to
// the rest of the codebase.
type PATService struct {
	db     *db.DB
	pepper []byte
	now    func() int64
}

// NewPATService constructs a PATService backed by database. pepper
// must be the same pepper pairing.Service uses for HMAC-SHA256 --
// shared pepper is the point: a DB-only compromise can't recover any
// token without it.
func NewPATService(database *db.DB, pepper []byte) *PATService {
	if database == nil {
		panic("auth: NewPATService requires non-nil db")
	}
	if len(pepper) == 0 {
		panic("auth: NewPATService requires non-empty pepper (same as pairing.Service)")
	}
	return &PATService{db: database, pepper: pepper, now: timeNowUnix}
}

// Middleware builds a PATMiddleware bound to this service's lookup,
// touch callback, and pepper. scope is the scope the protected route
// requires ("" = any authenticated PAT; "*" is satisfied by every
// token, including the bootstrap PAT).
func (s *PATService) Middleware(scope string) *PATMiddleware {
	return &PATMiddleware{
		Lookup:        s.Lookup,
		TouchLastUsed: s.TouchLastUsed,
		Pepper:        s.pepper,
		Scope:         scope,
	}
}

// timeNowUnix indirection so tests can override "now" without
// touching time.Now globally.
var timeNowUnix = func() int64 { return time.Now().Unix() }

// Mint creates a new PAT. Returns (plaintext, row, error). The
// plaintext is shown to the operator exactly once via the API
// response; the row carries the hashed_key. ExpiresAt is unix-seconds
// for expiry timestamps; pass 0 for a non-expiring token.
//
// Scopes is a JSON-encoded string of the form `["pairings:write",
// "settings:write"]`. Pass `["*"]` for the bootstrap PAT.
func (s *PATService) Mint(ctx context.Context, userID int64, name string, scopes []string, expiresAt int64) (string, *sqlcgen.UserPat, error) {
	plaintext, err := mintPATPlaintext()
	if err != nil {
		return "", nil, fmt.Errorf("mint token: %w", err)
	}
	hashed := hashToken(s.pepper, plaintext)
	scopesJSON, err := json.Marshal(scopes)
	if err != nil {
		return "", nil, fmt.Errorf("marshal scopes: %w", err)
	}
	var expiresAtArg sql.NullInt64
	if expiresAt > 0 {
		expiresAtArg = sql.NullInt64{Int64: expiresAt, Valid: true}
	}
	var row sqlcgen.UserPat
	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		r, err := q.CreateUserPAT(ctx, sqlcgen.CreateUserPATParams{
			UserID:     userID,
			Name:       name,
			HashedKey:  hashed,
			ScopesJson: string(scopesJSON),
			ExpiresAt:  expiresAtArg,
		})
		if err != nil {
			return fmt.Errorf("insert token: %w", err)
		}
		row = r
		return nil
	})
	if err != nil {
		return "", nil, err
	}
	return plaintext, &row, nil
}

// Revoke soft-deletes the token by setting revoked_at. Idempotent --
// revoking an already-revoked token is a no-op. The PAT row is
// matched on (id, user_id) so one admin can't revoke another's token.
func (s *PATService) Revoke(ctx context.Context, patID, userID int64) (bool, error) {
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.RevokeUserPAT(ctx, sqlcgen.RevokeUserPATParams{
			ID:     patID,
			UserID: userID,
		})
	})
	if err != nil {
		return false, fmt.Errorf("revoke token: %w", err)
	}
	// sqlc doesn't surface affected-row count for exec queries; the
	// caller distinguishes by re-fetching the row if it cares. For
	// the httpapi handler we just call Revoke and trust it -- a
	// duplicate revoke is fine.
	return true, nil
}

// Lookup is the lookup callback the middleware uses. Returns the
// user_id + scopes for the token's plaintext (after HMAC lookup);
// sql.ErrNoRows when no live row matches. Reads go through the
// reader pool -- the hot auth path never touches the writer.
func (s *PATService) Lookup(ctx context.Context, presented string) (PATLookupResult, error) {
	hashed := hashToken(s.pepper, presented)
	row, err := s.db.Reader.GetUserPATByHash(ctx, hashed)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return PATLookupResult{}, sql.ErrNoRows
		}
		return PATLookupResult{}, fmt.Errorf("lookup token: %w", err)
	}
	// Expiry check -- expired tokens fail closed (401 from the
	// middleware). We don't filter at the SQL level because most rows
	// have expires_at = NULL (non-expiring); an in-Go check on every
	// lookup is cheap.
	if row.ExpiresAt.Valid && row.ExpiresAt.Int64 < s.now() {
		return PATLookupResult{}, sql.ErrNoRows
	}
	scopes, err := decodeScopes(row.ScopesJson)
	if err != nil {
		return PATLookupResult{}, fmt.Errorf("decode scopes: %w", err)
	}
	return PATLookupResult{UserID: row.UserID, Scopes: scopes}, nil
}

// TouchLastUsed bumps the last_used_at column. Used as the
// PATMiddleware.TouchLastUsed callback. Throttled to once per 60s per
// token (the SQL query itself enforces this) so a flood of requests
// doesn't generate a flood of writes. Runs on the writer pool inside
// a one-shot tx; errors are discarded (best-effort bookkeeping).
func (s *PATService) TouchLastUsed(ctx context.Context, hashedKey string, at time.Time) {
	_ = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.TouchUserPAT(ctx, sqlcgen.TouchUserPATParams{
			HashedKey:  hashedKey,
			LastUsedAt: sql.NullInt64{Int64: at.Unix(), Valid: true},
		})
	})
}

// decodeScopes parses the JSON-encoded scopes array. Empty array is
// the no-scopes case -- tokens with [] satisfy no scope (so any
// scope-gated route returns 403), which is the safe default.
func decodeScopes(s string) ([]string, error) {
	if s == "" {
		return nil, nil
	}
	var scopes []string
	if err := json.Unmarshal([]byte(s), &scopes); err != nil {
		return nil, err
	}
	return scopes, nil
}

// mintPATPlaintext generates the plaintext token shown to the
// operator. 32 random bytes from crypto/rand, encoded as
// base64-url-no-padding (43 chars). The bdam_pat_ prefix is added by
// the caller so the literal is grep-able.
func mintPATPlaintext() (string, error) {
	buf := make([]byte, patRandomBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	// base64.URLEncoding without padding produces 43 chars from 32
	// bytes (no '=' padding chars). URLEncoding uses '-' and '_' which
	// is what the server-side validator matches on.
	enc := base64.URLEncoding.WithPadding(base64.NoPadding)
	return PATPrefix + enc.EncodeToString(buf), nil
}

// BootstrapMintsPAT, when non-empty, is the env-var name the boot
// sequence reads to decide whether to mint a one-shot admin PAT.
// Exported so cmd/branchdam/main.go's bootstrap call site can read
// it without hardcoding the string in two places.
const BootstrapMintsPAT = "ADMIN_BOOTSTRAP_PAT"

// BootstrapPATFileName is the filename (within the data directory)
// the bootstrap plaintext is written to. Mode 0600 on creation; the
// file's existence is the "already minted" sentinel -- a second boot
// that finds the file present refuses to honor the env var again,
// matching the consume-once model documented on Config.Admin.BootstrapPAT.
const BootstrapPATFileName = "bootstrap-pat.txt"

// ErrBootstrapAlreadyMinted is returned by RunBootstrap when the
// plaintext file already exists on disk -- a sign the env var was
// honored once and the operator wants to re-bootstrap. The caller
// (cmd/branchdam/main.go) treats this as a soft skip: continuing to
// run with a stale env var is fine if the file is present (the
// operator already has the previous mint). The env var's "consume-once"
// lifetime is enforced by the file's presence, not by the var itself.
var ErrBootstrapAlreadyMinted = errors.New("bootstrap PAT already minted (file exists; clear the env var or delete the file to re-bootstrap)")

// RunBootstrapPAT runs the one-shot bootstrap mint if the env var is
// non-empty AND the plaintext file doesn't already exist. Idempotent:
// re-runs on an existing file return ErrBootstrapAlreadyMinted
// without minting or overwriting. The minted PAT carries scopes=["*"]
// so it satisfies every scope-gated admin route.
//
// Returns nil when:
//   - the env var is empty (operator doesn't use bootstrap), OR
//   - the file already exists (consume-once: previous run minted it).
//
// Returns the plaintext when a new PAT was minted (so the caller can
// log its hash prefix and the file path, never the plaintext itself).
//
// `dataDir` is the directory the bootstrap file is written under; the
// caller passes filepath.Dir(cfg.Database.Path) so the file lives
// alongside the SQLite db.
//
// Routes the writes through db.InTx rather than exposing a
// write-capable queries handle, preserving the
// TestNoWriteCapableQueriesAccessorOutsideInTx invariant. The whole
// "ensure user + promote to admin + mint PAT" sequence is one
// transaction; if any step fails the row work is rolled back and the
// file isn't written -- so a partial-failure scenario can't leave a
// half-minted PAT behind.
func RunBootstrapPAT(ctx context.Context, database *db.DB, pepper []byte, envValue, dataDir string, log *slog.Logger) (string, error) {
	if envValue == "" {
		return "", nil
	}
	if len(pepper) == 0 {
		return "", errors.New("auth: RunBootstrapPAT requires non-empty pepper")
	}
	if dataDir == "" {
		return "", errors.New("auth: RunBootstrapPAT requires non-empty dataDir")
	}
	if database == nil {
		return "", errors.New("auth: RunBootstrapPAT requires non-nil *db.DB")
	}

	path := filepath.Join(dataDir, BootstrapPATFileName)
	if _, err := os.Stat(path); err == nil {
		// File exists -- consume-once: refuse.
		return "", ErrBootstrapAlreadyMinted
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("stat bootstrap file: %w", err)
	}

	// One transaction wraps all three writes (ensure user, promote
	// to admin, insert PAT). If anything fails the work is rolled
	// back and the file isn't written, so a partial-failure scenario
	// can't leave a half-minted PAT behind.
	var plaintext string
	var userID int64
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		uid, err := q.EnsureBootstrapUser(ctx)
		if err != nil {
			return fmt.Errorf("ensure bootstrap user: %w", err)
		}
		if err := q.PromoteUserToAdmin(ctx, uid); err != nil {
			return fmt.Errorf("promote bootstrap user to admin: %w", err)
		}
		userID = uid
		plain, hashed, err := mintBootstrapToken(pepper)
		if err != nil {
			return err
		}
		scopes := []string{"*"}
		scopesJSON, err := json.Marshal(scopes)
		if err != nil {
			return fmt.Errorf("marshal scopes: %w", err)
		}
		_, err = q.CreateUserPAT(ctx, sqlcgen.CreateUserPATParams{
			UserID:     uid,
			Name:       "bootstrap",
			HashedKey:  hashed,
			ScopesJson: string(scopesJSON),
			ExpiresAt:  sql.NullInt64{},
		})
		if err != nil {
			return fmt.Errorf("insert bootstrap PAT: %w", err)
		}
		plaintext = plain
		return nil
	})
	if err != nil {
		return "", err
	}

	// File write is OUTSIDE the transaction. If it fails after a
	// successful commit, the PAT exists in the db but the operator
	// has no plaintext -- the next boot will refuse to re-mint
	// (file existence) and the operator must manually delete the
	// user_pats row OR set the file path writable. That's a worse
	// outcome than a clean DB write -- but the alternative (writing
	// the file first, then the DB) is worse still, because a failed
	// DB write after a successful file write leaves a plaintext
	// lying around with no corresponding server-side token.
	if err := os.WriteFile(path, []byte(plaintext+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write bootstrap file: %w", err)
	}

	log.Info("bootstrap PAT minted", "path", path, "user_id", userID, "scopes", []string{"*"})
	return plaintext, nil
}

// mintBootstrapToken generates the bootstrap PAT plaintext + its
// HMAC-SHA256(pepper, ...) hash. Standalone function (not a method
// on PATService) because the bootstrap path doesn't construct a
// PATService -- the service is constructed later in main.go and
// doesn't need to be on the bootstrap hot path.
func mintBootstrapToken(pepper []byte) (plaintext, hashed string, err error) {
	plaintext, err = mintPATPlaintext()
	if err != nil {
		return "", "", fmt.Errorf("mint token: %w", err)
	}
	mac := hmac.New(sha256.New, pepper)
	mac.Write([]byte("pat:" + plaintext))
	hashed = hex.EncodeToString(mac.Sum(nil))
	return plaintext, hashed, nil
}
