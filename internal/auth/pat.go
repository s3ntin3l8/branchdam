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
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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
	// never blocked on a write, but only after an in-process
	// per-token throttle (touchThrottleWindow) so a request burst
	// doesn't spawn a goroutine per request -- the SQL throttle in
	// TouchUserPAT remains as the backstop. A flush error is logged
	// but never propagated to the request. Nil = no-op.
	TouchLastUsed func(ctx context.Context, patHash string, at time.Time)
	// Pepper is used to HMAC-SHA256 hash the presented plaintext for
	// lookup and for the last_used_at touch key. Must be the same
	// pepper the db-backed Lookup uses, or the touch would key on a
	// hash that matches no row. Required.
	Pepper []byte
	// Scope is the scope the protected route requires. A token whose
	// scopes_json contains "*" satisfies every scope; otherwise the
	// requested scope must appear in the token's list (string-equal).
	// Empty requested scope ("any authenticated PAT") is always
	// satisfied -- the caller is opting out of scope enforcement.
	Scope string

	// touchMu/lastTouch back the in-process last_used_at throttle:
	// a per-hashed-key timestamp of the most recent async touch, so
	// the 60s SQL throttle doesn't cost a goroutine + writer-pool tx
	// per request. Only mutated when TouchLastUsed is non-nil.
	touchMu   sync.Mutex
	lastTouch map[string]time.Time
}

// touchThrottleWindow is the in-process minimum interval between
// async last_used_at touches for one token. Matches the SQL-level
// throttle in TouchUserPAT so the two never disagree about when a
// write is due.
const touchThrottleWindow = 60 * time.Second

// maxLastTouchEntries bounds the per-token last-touch map. It's a
// soft cap: when exceeded, stale entries (older than the throttle
// window, i.e. no longer suppressing anything) are evicted, and only
// genuinely active tokens refill the map. A long-lived server mints
// and retires tokens over months; without eviction the map would
// hold one entry per token ever seen.
const maxLastTouchEntries = 4096

// shouldTouch reports whether enough time has passed since the last
// async touch for this token to bother spawning the goroutine. Records
// "now" as the new last-touch when it returns true, so concurrent
// requests within the same window collapse to one touch.
func (m *PATMiddleware) shouldTouch(hash string, now time.Time) bool {
	m.touchMu.Lock()
	defer m.touchMu.Unlock()
	if m.lastTouch == nil {
		m.lastTouch = make(map[string]time.Time)
	}
	if len(m.lastTouch) >= maxLastTouchEntries {
		for k, v := range m.lastTouch {
			if now.Sub(v) >= touchThrottleWindow {
				delete(m.lastTouch, k)
			}
		}
	}
	if last, ok := m.lastTouch[hash]; ok && now.Sub(last) < touchThrottleWindow {
		return false
	}
	m.lastTouch[hash] = now
	return true
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
			// when LookupKey returns a non-NoRows error. Deliberately
			// no request-derived fields (method/path): CodeQL flags
			// log entries built from user input, and the lookup error
			// already carries the diagnostic value.
			slogDefault().Error("auth: PAT lookup failed", "err", err.Error())
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}

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
		// and never propagates a write failure. The in-process
		// throttle (touchThrottleWindow) runs before the goroutine so
		// a request burst doesn't spawn one goroutine + writer-pool tx
		// per request; the SQL throttle in TouchUserPAT remains as a
		// backstop for the same window.
		if m.TouchLastUsed != nil {
			at := m.Now()
			if m.shouldTouch(hash, at) {
				go func() {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					m.TouchLastUsed(ctx, hash, at)
				}()
			}
		}

		// Attach a LocalUserView alongside the Principal. Three
		// consumers depend on it:
		//
		//   - RequireAdmin/IsAdmin: the PAT principal carries no
		//     forward-auth Groups, so without the local-`is_admin`
		//     override every write route would 403 the moment
		//     authz.groups is non-empty. Lookup has already verified
		//     the token's owner is a live admin (is_admin=1,
		//     disabled_at IS NULL -- see GetUserPATByHash), so
		//     IsAdmin=true here reflects the owner's current
		//     authority, not authority frozen at mint time; a
		//     demoted or disabled owner's token fails Lookup instead.
		//     Route-level granularity comes from the middleware
		//     Scope, not from the admin flag.
		//   - MFAGate: MFAVerified=true so a PAT-authenticated
		//     request isn't mistaken for a half-authed password-only
		//     session -- the token itself is the second factor.
		//   - Handlers: LocalUserView.UserID is the token owner's
		//     users.id, letting /api/v1/users/me/pats resolve the
		//     owner without a second lookup.
		ctx := withPrincipal(r.Context(), principal)
		ctx = withPATScopes(ctx, result.Scopes)
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

// patScopesKey is the context key under which RequirePAT stores the
// scopes the presented token carries. It lets scope-gated handlers
// (e.g. POST /api/v1/users/me/pats) cap what a narrower token may
// mint -- without it a "pats:write" token could mint itself a "*"
// token, making every scoped token a full-admin credential.
type patScopesKey struct{}

func withPATScopes(ctx context.Context, scopes []string) context.Context {
	return context.WithValue(ctx, patScopesKey{}, scopes)
}

// PATScopesFrom returns the scope list of the PAT that authenticated
// this request. The second return is false when the request was not
// PAT-authenticated (session cookie or forward-auth) -- those callers
// carry their authority in the Principal/LocalUserView instead, and
// handlers should treat them as unrestricted for scope-capping
// purposes.
func PATScopesFrom(ctx context.Context) ([]string, bool) {
	scopes, ok := ctx.Value(patScopesKey{}).([]string)
	return scopes, ok
}

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

// ErrPATOwnerNotAdmin is returned by Mint when the requested owner
// has no users row, isn't is_admin=1, or has disabled_at set. The
// PAT lookup query authenticates only live-admin-owned tokens, so
// minting for such an owner would return a token that 401s on first
// use -- a silent dead credential. The HTTP layer maps this to 403.
var ErrPATOwnerNotAdmin = errors.New("PAT owner is not a live admin")

// Mint creates a new PAT. Returns (plaintext, row, error). The
// plaintext is shown to the operator exactly once via the API
// response; the row carries the hashed_key. ExpiresAt is unix-seconds
// for expiry timestamps; pass 0 for a non-expiring token.
//
// Scopes is a JSON-encoded string of the form `["pairings:write",
// "settings:write"]`. Pass `["*"]` for the bootstrap PAT.
//
// The owner must be a live admin: the lookup query authenticates
// tokens only when the owner's users row has is_admin = 1 AND
// disabled_at IS NULL, so minting for anyone else would return a
// well-formed token that 401s on first use. Mint checks the owner's
// status in the SAME transaction as the insert -- a demotion racing
// the mint fails the mint, not the other way around. A failed check
// returns ErrPATOwnerNotAdmin; the HTTP layer maps it to 403.
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
		// Owner authority check, in-tx with the insert: the lookup
		// query requires a live admin owner, so minting for anyone
		// else would produce a token that can never authenticate
		// (silent dead credential). See ErrPATOwnerNotAdmin.
		status, err := q.GetUserAdminStatus(ctx, userID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPATOwnerNotAdmin
			}
			return fmt.Errorf("check owner admin status: %w", err)
		}
		if status.IsAdmin != 1 || status.DisabledAt.Valid {
			return ErrPATOwnerNotAdmin
		}
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

// Revoke soft-deletes the token by setting revoked_at. Returns
// revoked=false (nil error) when no live row matched (id, user_id) --
// the caller maps that to a 404 so a nonexistent or foreign token id
// can't present as a silent success. The (id, user_id) scoping means
// one admin can't revoke another's token.
func (s *PATService) Revoke(ctx context.Context, patID, userID int64) (bool, error) {
	var rows int64
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.RevokeUserPAT(ctx, sqlcgen.RevokeUserPATParams{
			ID:     patID,
			UserID: userID,
		})
		if err != nil {
			return err
		}
		rows = n
		return nil
	})
	if err != nil {
		return false, fmt.Errorf("revoke token: %w", err)
	}
	return rows > 0, nil
}

// Lookup is the lookup callback the middleware uses. Returns the
// user_id + scopes for the token's plaintext (after HMAC lookup);
// sql.ErrNoRows when no live row matches. Reads go through the
// reader pool -- the hot auth path never touches the writer.
//
// Authority is checked here, not frozen at mint: the query joins the
// owner row and filters on users.is_admin = 1 AND disabled_at IS
// NULL, so demoting or disabling the owner invalidates every live
// token of theirs on their next request (the same posture the session
// middleware takes). A demoted owner's token surfaces as
// sql.ErrNoRows -> 401, matching a revoked token.
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
// token (the middleware's in-process shouldTouch gate runs first; the
// SQL WHERE below is the backstop) so a flood of requests doesn't
// generate a flood of writes. Runs on the writer pool inside a
// one-shot tx. Errors are logged but never propagated -- best-effort
// bookkeeping, per the middleware contract.
func (s *PATService) TouchLastUsed(ctx context.Context, hashedKey string, at time.Time) {
	if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.TouchUserPAT(ctx, sqlcgen.TouchUserPATParams{
			HashedKey:  hashedKey,
			LastUsedAt: sql.NullInt64{Int64: at.Unix(), Valid: true},
		})
	}); err != nil {
		slogDefault().Error("auth: touch user_pats.last_used_at failed", "err", err.Error())
	}
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
// claimed (still empty) sentinel file is removed, so a
// partial-failure scenario can't leave a half-minted PAT behind or
// block re-bootstrap with an empty sentinel.
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
	// Claim the path BEFORE touching the DB: O_CREATE|O_EXCL atomically
	// fails with fs.ErrExist when anything (file, symlink -- dangling
	// or not) already sits at the path, and O_NOFOLLOW refuses to
	// traverse one, so a symlink planted at <dataDir>/bootstrap-pat.txt
	// can't redirect the (non-expiring, wildcard) plaintext elsewhere.
	// Two concurrent boots therefore can't both mint: exactly one open
	// succeeds, the other sees the EEXIST branch below.
	claim := func() (*os.File, error) {
		return os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL|syscall.O_NOFOLLOW, 0o600)
	}
	f, err := claim()
	if err != nil {
		if !errors.Is(err, fs.ErrExist) {
			return "", fmt.Errorf("open bootstrap file: %w", err)
		}
		// Something already sits at the path -- Lstat (never Stat, so
		// a symlink reports itself) distinguishes three cases:
		info, lerr := os.Lstat(path)
		if lerr != nil {
			return "", fmt.Errorf("lstat bootstrap file: %w", lerr)
		}
		switch {
		case info.Mode()&fs.ModeSymlink != 0:
			return "", fmt.Errorf("bootstrap file %s is a symlink; refusing to write the plaintext through it", path)
		case info.Mode().IsRegular() && info.Size() == 0:
			// A zero-byte sentinel is a crashed claim: SIGKILL or a
			// reboot between the exclusive create and the DB commit
			// skips the cleanup defer, and the empty file would
			// otherwise block every future bootstrap while holding
			// no token. Remove it and re-claim; if a second crashed
			// boot left a non-empty file, that's a real sentinel and
			// the next loop refusal applies.
			if rerr := os.Remove(path); rerr != nil {
				return "", fmt.Errorf("remove crashed bootstrap claim: %w", rerr)
			}
			f, err = claim()
			if err != nil {
				if errors.Is(err, fs.ErrExist) {
					return "", ErrBootstrapAlreadyMinted
				}
				return "", fmt.Errorf("open bootstrap file: %w", err)
			}
		default:
			// Non-empty regular file (or an unexpected non-symlink
			// type): a real sentinel -- consume-once, refuse.
			return "", ErrBootstrapAlreadyMinted
		}
	}
	// From here on, a failure must not leave the empty claim file
	// behind: its existence is the consume-once sentinel, so a
	// half-failed boot would otherwise block re-bootstrap forever.
	// (A crash, as opposed to a returned error, still can -- that's
	// the zero-byte case handled above on the next boot.)
	committed := false
	defer func() {
		_ = f.Close()
		if !committed {
			_ = os.Remove(path)
		}
	}()

	// One transaction wraps all three writes (ensure user, promote
	// to admin, insert PAT). If anything fails the work is rolled
	// back and the file isn't written, so a partial-failure scenario
	// can't leave a half-minted PAT behind.
	var plaintext string
	var userID int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
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

	// The plaintext is written to the already-claimed handle, so the
	// sentinel and the secret appear atomically from the operator's
	// point of view: no window where the file exists but is empty.
	// (The DB commit above is the other half of the ordering problem
	// -- see the comment before the tx.)
	if _, err := f.WriteString(plaintext + "\n"); err != nil {
		return "", fmt.Errorf("write bootstrap file: %w", err)
	}
	// Sync before Close: without it a power loss right after a
	// successful boot could leave a zero-byte or torn sentinel on
	// disk despite the DB commit having succeeded -- and the
	// zero-byte file is exactly the crash case the next boot now
	// recovers, at the cost of re-minting. Pushing the plaintext to
	// stable storage makes "file present" mean "token recoverable".
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync bootstrap file: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close bootstrap file: %w", err)
	}
	committed = true

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
