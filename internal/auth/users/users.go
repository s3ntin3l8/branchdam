// Package users owns the local-auth identity layer: argon2id password
// hashing/verification, user CRUD, server-side session storage, and
// login-audit writes. Everything in this package operates through the
// sqlc-generated queries in internal/db/sqlcgen/local_auth.sql.go (the
// project's no-raw-SQL convention, per CONTRIBUTING.md).
//
// Public surface consumed by HTTP handlers and the session middleware:
//
//   - Service: a thin wrapper bundling db + cookie HMAC key + audit
//     settings. Built once in cmd/branchdam, passed via httpapi.Deps.
//   - Password hash / verify: HashPassword / VerifyPassword.
//   - User CRUD: CreateLocalUser / CreateForwardJITUser / GetUserByID /
//     GetUserByUsername / GetUserByEmailSource / DisableUser / ListUsers.
//   - Session lifecycle: CreateSession / GetSessionByCookieID /
//     TouchSession / RevokeSession / RevokeAllUserSessions.
//   - Cookie mint/verify: MintCookieValue / VerifyCookieValue.
//   - Audit: WriteLoginAudit.
//
// Design rationale lives in PLAN.md; per-table invariants in the migration
// 00018_local_auth.sql table comments. The hand-maintained sqlcgen
// additions carry the same "why not regenerated" caveat.
package users

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"golang.org/x/crypto/argon2"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// Argon2idParameters is the parameter set used to derive a password hash.
// Defaults are OWASP-recommended baseline (2024): m=19 MiB, t=2, p=1,
// saltLen=16, keyLen=32. Tunable via internal/config.Auth.Local.Argon2
// for operators on constrained hardware, but the defaults are safe for
// any deployment where the server has a few MiB of headroom.
type Argon2idParameters struct {
	MemoryKB    uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultArgon2idParameters is the OWASP-recommended baseline.
func DefaultArgon2idParameters() Argon2idParameters {
	return Argon2idParameters{
		MemoryKB:    19456, // 19 MiB
		Iterations:  2,
		Parallelism: 1,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// ErrPasswordMismatch is returned by VerifyPassword when the presented
// password does not match the stored hash.
var ErrPasswordMismatch = errors.New("users: password does not match")

// ErrUserDisabled is returned by VerifyPassword when the matched user
// has disabled_at set.
var ErrUserDisabled = errors.New("users: account is disabled")

// ErrUserNotFound is returned when no user row matches the lookup key.
var ErrUserNotFound = errors.New("users: not found")

// ErrSessionNotFound is returned by GetSessionByCookieID on a miss.
var ErrSessionNotFound = errors.New("users: session not found")

// CookieKey derives the HMAC-SHA256 key used to tag session cookies from
// BRANCHDAM_SECRET_KEY-derived material. The cookie value itself is
// opaque (32 random bytes hex-encoded); the HMAC exists so a tampered
// cookie -- or one signed by a different deployment's secret -- is
// detected before any DB lookup.
//
// Returns nil when the input is missing/empty/not a 32-byte base64
// value. Callers (cmd/branchdam/main.go at startup) MUST check for
// nil and refuse to enable local auth if so -- falling back to a
// public constant here would mean every session cookie in production
// is signed with a known key. Tests that need a dev-only key should
// pass opts.CookieKey explicitly via ServiceOptions, NOT rely on a
// fallback that no longer exists.
func CookieKey(secretKeyBase64 string) []byte {
	if secretKeyBase64 == "" {
		return nil
	}
	k, err := base64.StdEncoding.DecodeString(secretKeyBase64)
	if err != nil || len(k) != 32 {
		return nil
	}
	// codeql[go/weak-sensitive-data-hashing]
	// codeql[go/weak-cryptographic-algorithm]
	// SHA-256 here is used to derive a 32-byte HMAC key from the
	// operator-supplied BRANCHDAM_SECRET_KEY (a 32-byte secret input
	// is already uniformly distributed; SHA-256 acts as a domain
	// separator). This is NOT password hashing -- the password hash
	// is argon2id (see HashPassword above). The cookie HMAC itself
	// (mac := hmac.New(sha256.New, ...) further down) is a keyed MAC,
	// not a bare hash.
	h := sha256.Sum256(k)
	return h[:]
}

// Service is the only externally-constructed type in this package.
type Service struct {
	db        *db.DB
	log       *slog.Logger
	argon     Argon2idParameters
	cookieKey []byte
	nowFn     func() time.Time
}

// ServiceOptions is the optional-arg bundle for NewService.
type ServiceOptions struct {
	Argon     Argon2idParameters
	CookieKey []byte
	Log       *slog.Logger
	Now       func() time.Time
}

// NewService constructs a Service. secretKeyBase64 is BRANCHDAM_SECRET_KEY.
//
// Panics when the caller did NOT supply opts.CookieKey AND the supplied
// secretKeyBase64 doesn't decode to 32 bytes -- the previous silent
// fallback to a public constant was a known-key HMAC vulnerability
// (Hermes #407 review on PR #407). Callers at startup (cmd/branchdam)
// must check for this case before calling NewService; this panic is
// the second line of defense for misconfigured callers and for tests
// that forgot to wire opts.CookieKey.
func NewService(database *db.DB, secretKeyBase64 string, opts ServiceOptions) *Service {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	argon := opts.Argon
	if argon == (Argon2idParameters{}) {
		argon = DefaultArgon2idParameters()
	}
	cookieKey := opts.CookieKey
	if len(cookieKey) == 0 {
		cookieKey = CookieKey(secretKeyBase64)
		if len(cookieKey) == 0 {
			panic("users.NewService: BRANCHDAM_SECRET_KEY is missing/invalid (must be 32-byte base64) and opts.CookieKey was not supplied. Refusing to construct a Service with a guessable HMAC key.")
		}
	}
	nowFn := opts.Now
	if nowFn == nil {
		nowFn = time.Now
	}
	return &Service{
		db:        database,
		log:       opts.Log,
		argon:     argon,
		cookieKey: cookieKey,
		nowFn:     nowFn,
	}
}

// Now is the service's clock, exposed so the HTTP layer can stamp cookie
// expiry using the same time source.
func (s *Service) Now() time.Time { return s.nowFn() }

// Argon returns the active parameter set.
func (s *Service) Argon() Argon2idParameters { return s.argon }

// CookieKey exposes the derived HMAC key for tests that need to forge
// cookie values.
func (s *Service) CookieKey() []byte { return s.cookieKey }

// withTx is the internal write-path seam: every write goes through the
// writer pool's single connection via DB.InTx (AGENTS.md invariant #2).
func (s *Service) withTx(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	return s.db.InTx(ctx, fn)
}

// CountUsers returns the total row count of the users table.
func (s *Service) CountUsers(ctx context.Context) (int64, error) {
	return s.db.Reader.CountUsers(ctx)
}

// CreateLocalUser inserts a source='local' user with the given password.
func (s *Service) CreateLocalUser(ctx context.Context, username, email, password string, isAdmin bool, createdAt int64, createdBy string) (sqlcgen.User, error) {
	if username == "" {
		return sqlcgen.User{}, errors.New("users: username is required")
	}
	if password == "" {
		return sqlcgen.User{}, errors.New("users: password is required")
	}
	hash, err := s.HashPassword(password)
	if err != nil {
		return sqlcgen.User{}, fmt.Errorf("hash password: %w", err)
	}
	var emailNS sql.NullString
	if email != "" {
		emailNS = sql.NullString{String: email, Valid: true}
	}
	var row sqlcgen.User
	err = s.withTx(ctx, func(q *sqlcgen.Queries) error {
		var txErr error
		row, txErr = q.CreateLocalUser(ctx, sqlcgen.CreateLocalUserParams{
			Username:     username,
			Email:        emailNS,
			PasswordHash: hash,
			IsAdmin:      boolToInt(isAdmin),
			CreatedAt:    createdAt,
			CreatedBy:    createdBy,
		})
		return txErr
	})
	if err != nil {
		return sqlcgen.User{}, err
	}
	return row, nil
}

// CreateForwardJITUser inserts a source='forward-jit' user. email MAY
// be empty when requireEmail=false in the JIT config -- the user is
// then keyed by username. The callers (JITProvisioner in jit.go)
// enforce the require-email gate; this function just persists what
// the caller gave it.
func (s *Service) CreateForwardJITUser(ctx context.Context, username, email string, isAdmin bool, createdAt int64, createdBy string) (sqlcgen.User, error) {
	if email == "" && username == "" {
		return sqlcgen.User{}, errors.New("users: forward-jit requires either email or username")
	}
	if username == "" {
		at := strings.IndexByte(email, '@')
		if at < 0 {
			username = email
		} else {
			username = email[:at]
		}
	}
	var emailNS sql.NullString
	if email != "" {
		emailNS = sql.NullString{String: email, Valid: true}
	}
	var row sqlcgen.User
	err := s.withTx(ctx, func(q *sqlcgen.Queries) error {
		var txErr error
		row, txErr = q.CreateForwardJITUser(ctx, sqlcgen.CreateForwardJITUserParams{
			Username:  username,
			Email:     emailNS,
			IsAdmin:   boolToInt(isAdmin),
			CreatedAt: createdAt,
			CreatedBy: createdBy,
		})
		return txErr
	})
	if err != nil {
		return sqlcgen.User{}, err
	}
	return row, nil
}

// GetUserByID returns ErrUserNotFound on sql.ErrNoRows.
func (s *Service) GetUserByID(ctx context.Context, id int64) (sqlcgen.User, error) {
	row, err := s.db.Reader.GetUserByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlcgen.User{}, ErrUserNotFound
		}
		return sqlcgen.User{}, err
	}
	return row, nil
}

// GetUserByUsername wraps the sqlc query.
func (s *Service) GetUserByUsername(ctx context.Context, username string) (sqlcgen.User, error) {
	row, err := s.db.Reader.GetUserByUsername(ctx, username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlcgen.User{}, ErrUserNotFound
		}
		return sqlcgen.User{}, err
	}
	return row, nil
}

// GetUserByEmailSource wraps the sqlc query.
func (s *Service) GetUserByEmailSource(ctx context.Context, email, source string) (sqlcgen.User, error) {
	row, err := s.db.Reader.GetUserByEmailSource(ctx, sqlcgen.GetUserByEmailSourceParams{
		Email:  sql.NullString{String: email, Valid: true},
		Source: source,
	})
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlcgen.User{}, ErrUserNotFound
		}
		return sqlcgen.User{}, err
	}
	return row, nil
}

// DisableUser sets disabled_at. Idempotent.
func (s *Service) DisableUser(ctx context.Context, id int64, when time.Time) error {
	return s.withTx(ctx, func(q *sqlcgen.Queries) error {
		return q.DisableUser(ctx, sqlcgen.DisableUserParams{
			ID:         id,
			DisabledAt: sql.NullInt64{Int64: when.Unix(), Valid: true},
		})
	})
}

// ListUsers is a paginated admin listing.
func (s *Service) ListUsers(ctx context.Context, limit, offset int64) ([]sqlcgen.User, error) {
	return s.db.Reader.ListUsers(ctx, sqlcgen.ListUsersParams{Limit: limit, Offset: offset})
}

// HashPassword derives an argon2id-encoded string.
func (s *Service) HashPassword(plaintext string) (string, error) {
	salt, err := randomBytes(s.argon.SaltLength)
	if err != nil {
		return "", fmt.Errorf("salt: %w", err)
	}
	key := argon2.IDKey([]byte(plaintext), salt, s.argon.Iterations, s.argon.MemoryKB, s.argon.Parallelism, s.argon.KeyLength)
	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		s.argon.MemoryKB, s.argon.Iterations, s.argon.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key),
	), nil
}

// VerifyPassword checks plaintext against a stored argon2id hash. Returns
// nil on success, ErrPasswordMismatch on a mismatch, or another error
// for malformed hashes.
func (s *Service) VerifyPassword(plaintext, stored string) error {
	memoryKB, iterations, parallelism, salt, key, err := parseArgon2idHash(stored)
	if err != nil {
		return fmt.Errorf("users: parse stored hash: %w", err)
	}
	candidate := argon2.IDKey([]byte(plaintext), salt, iterations, memoryKB, parallelism, uint32(len(key)))
	if subtle.ConstantTimeCompare(candidate, key) != 1 {
		return ErrPasswordMismatch
	}
	return nil
}

func parseArgon2idHash(stored string) (memoryKB, iterations uint32, parallelism uint8, salt, key []byte, err error) {
	const prefix = "$argon2id$v="
	if !strings.HasPrefix(stored, prefix) {
		return 0, 0, 0, nil, nil, errors.New("not an argon2id hash")
	}
	rest := stored[len(prefix):]
	parts := strings.Split(rest, "$")
	if len(parts) != 4 {
		return 0, 0, 0, nil, nil, fmt.Errorf("expected 4 $-separated parts, got %d", len(parts))
	}
	var version int
	if _, scanErr := fmt.Sscanf(parts[0], "%d", &version); scanErr != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("parse version: %w", scanErr)
	}
	if version != argon2.Version {
		return 0, 0, 0, nil, nil, fmt.Errorf("unsupported argon2id version %d", version)
	}
	if _, scanErr := fmt.Sscanf(parts[1], "m=%d,t=%d,p=%d", &memoryKB, &iterations, &parallelism); scanErr != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("parse params: %w", scanErr)
	}
	salt, decErr := base64.RawStdEncoding.DecodeString(parts[2])
	if decErr != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("decode salt: %w", decErr)
	}
	key, decErr = base64.RawStdEncoding.DecodeString(parts[3])
	if decErr != nil {
		return 0, 0, 0, nil, nil, fmt.Errorf("decode key: %w", decErr)
	}
	return memoryKB, iterations, parallelism, salt, key, nil
}

// MintCookieValue returns (cookieID, cookieValue). cookieID is the 32-byte
// random identifier stored in sessions.cookie_id. cookieValue is the
// form written into the HTTP cookie: cookieID hex + "." + hmac hex.
func (s *Service) MintCookieValue() (cookieID, cookieValue string, err error) {
	id, err := randomBytes(32)
	if err != nil {
		return "", "", fmt.Errorf("mint cookie id: %w", err)
	}
	cookieID = fmt.Sprintf("%x", id)
	// codeql[go/weak-cryptographic-algorithm]
	// HMAC-SHA-256 is the right primitive here: a KEYED MAC over a
	// 32-byte random secret. Not a password hash. HMAC-SHA-256 is
	// codeql[go/weak-sensitive-data-hashing]
	// codeql[go/weak-cryptographic-algorithm]
	// explicitly in NIST SP 800-107's recommended MAC algorithms
	// (FIPS 198-1). The pre-image resistance that "use a slow hash"
	// guidance targets does not apply -- there is no human-typed
	// secret to brute-force, only the 256-bit random cookie_id.
	mac := hmac.New(sha256.New, s.cookieKey)
	mac.Write([]byte(cookieID))
	tag := mac.Sum(nil)
	cookieValue = cookieID + "." + fmt.Sprintf("%x", tag)
	return cookieID, cookieValue, nil
}

// VerifyCookieValue returns the cookieID portion of cookieValue iff the
// HMAC tag matches. Empty string + nil when the value is malformed or
// the tag doesn't match.
func (s *Service) VerifyCookieValue(cookieValue string) (string, error) {
	dot := strings.IndexByte(cookieValue, '.')
	if dot < 0 || dot == 0 || dot == len(cookieValue)-1 {
		return "", nil
	}
	cookieID := cookieValue[:dot]
	tagHex := cookieValue[dot+1:]
	tag, err := hex.DecodeString(tagHex)
	if err != nil {
		return "", nil
	}
	// codeql[go/weak-sensitive-data-hashing]
	// codeql[go/weak-cryptographic-algorithm]
	// HMAC-SHA-256 verification, paired with MintCookieValue. The
	// comparison below uses hmac.Equal (constant-time); there is no
	// timing side channel and no human-typed secret to brute-force.
	mac := hmac.New(sha256.New, s.cookieKey)
	mac.Write([]byte(cookieID))
	if !hmac.Equal(tag, mac.Sum(nil)) {
		return "", nil
	}
	return cookieID, nil
}

// CreateSession persists a new session row.
func (s *Service) CreateSession(ctx context.Context, userID int64, cookieID, ip, userAgent string, absoluteExpiry, idleExpiry time.Time) (sqlcgen.Session, error) {
	now := s.nowFn()
	var session sqlcgen.Session
	err := s.withTx(ctx, func(q *sqlcgen.Queries) error {
		var txErr error
		session, txErr = q.CreateSession(ctx, sqlcgen.CreateSessionParams{
			CookieID:      cookieID,
			UserID:        userID,
			CreatedAt:     now.Unix(),
			LastSeenAt:    now.Unix(),
			ExpiresAt:     absoluteExpiry.Unix(),
			IdleExpiresAt: idleExpiry.Unix(),
			Ip:            ip,
			UserAgent:     userAgent,
		})
		return txErr
	})
	if err != nil {
		return sqlcgen.Session{}, err
	}
	return session, nil
}

// GetSessionByCookieID wraps the sqlc query.
func (s *Service) GetSessionByCookieID(ctx context.Context, cookieID string) (sqlcgen.Session, error) {
	row, err := s.db.Reader.GetSessionByCookieID(ctx, cookieID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlcgen.Session{}, ErrSessionNotFound
		}
		return sqlcgen.Session{}, err
	}
	return row, nil
}

// TouchSession updates last_seen_at and idle_expires_at.
func (s *Service) TouchSession(ctx context.Context, sessionID int64, lastSeen, idleExpiry time.Time) error {
	return s.withTx(ctx, func(q *sqlcgen.Queries) error {
		return q.TouchSession(ctx, sqlcgen.TouchSessionParams{
			ID:            sessionID,
			LastSeenAt:    lastSeen.Unix(),
			IdleExpiresAt: idleExpiry.Unix(),
		})
	})
}

// RevokeSession marks a single session revoked.
func (s *Service) RevokeSession(ctx context.Context, sessionID int64) error {
	return s.withTx(ctx, func(q *sqlcgen.Queries) error {
		return q.RevokeSession(ctx, sqlcgen.RevokeSessionParams{
			ID:        sessionID,
			RevokedAt: sql.NullInt64{Int64: s.nowFn().Unix(), Valid: true},
		})
	})
}

// RevokeAllUserSessions terminates every active session for a user.
func (s *Service) RevokeAllUserSessions(ctx context.Context, userID int64) error {
	return s.withTx(ctx, func(q *sqlcgen.Queries) error {
		return q.RevokeAllUserSessions(ctx, sqlcgen.RevokeAllUserSessionsParams{
			UserID:    userID,
			RevokedAt: sql.NullInt64{Int64: s.nowFn().Unix(), Valid: true},
		})
	})
}

// WriteLoginAudit appends a login_audit row. Best-effort: a transient DB
// failure is logged and swallowed so it doesn't fail a successful login.
func (s *Service) WriteLoginAudit(ctx context.Context, userID sql.NullInt64, usernamePresented, source, outcome, ip, userAgent, details string) {
	err := s.withTx(ctx, func(q *sqlcgen.Queries) error {
		return q.InsertLoginAudit(ctx, sqlcgen.InsertLoginAuditParams{
			UserID:            userID,
			UsernamePresented: usernamePresented,
			Source:            source,
			Outcome:           outcome,
			Ip:                ip,
			UserAgent:         userAgent,
			Details:           details,
			CreatedAt:         s.nowFn().Unix(),
		})
	})
	if err != nil {
		s.log.Warn("users: write login audit", "err", err.Error())
	}
}

// --- small helpers ---

func randomBytes(n uint32) ([]byte, error) {
	if n == 0 {
		return nil, nil
	}
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	return buf, nil
}

func boolToInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}
