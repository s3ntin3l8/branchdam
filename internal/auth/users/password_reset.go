package users

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// Password-reset primitives. Tokens are 32 random bytes, base64url-encoded
// for transport, sha256-hashed for at-rest storage. The plaintext token
// is only available at mint time -- it surfaces to the operator via
// slog.WARN (self-service) or the response body (admin reset) and is
// never persisted.
//
// Single-use is enforced by ConsumePasswordResetToken's atomic CAS
// (UPDATE...WHERE used_at IS NULL AND expires_at > now). A second
// confirm attempt with the same token matches zero rows and the caller
// treats it as 404 -- no enumeration of "already used" vs "expired".

// ErrTokenNotFound is the unified error for an invalid, used, or
// expired reset token. The HTTP layer maps it to 404; the message is
// deliberately generic so an attacker who has guessed-or-stolen a
// token cannot distinguish "wrong" from "right but already used".
var ErrTokenNotFound = errors.New("users: password-reset token not found, used, or expired")

// ResetTokenBytes is the size of the random token material. 32 bytes
// (256 bits) is well above the threshold for unguessability; the
// base64url-encoded form is 43 characters.
const ResetTokenBytes = 32

// PasswordResetServiceOptions is the optional-arg bundle for
// NewPasswordResetService. TokenTTL is the lifetime of an issued
// token; default is 24h.
type PasswordResetServiceOptions struct {
	TokenTTL time.Duration
}

// PasswordResetService is the public surface for the password-reset
// flow. It composes the existing *Service for the user / password-hash
// plumbing, so callers don't have to wire two services.
type PasswordResetService struct {
	svc      *Service
	tokenTTL time.Duration
}

// NewPasswordResetService constructs a PasswordResetService sharing
// the underlying users.Service.
func NewPasswordResetService(svc *Service, opts PasswordResetServiceOptions) *PasswordResetService {
	ttl := opts.TokenTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &PasswordResetService{svc: svc, tokenTTL: ttl}
}

// TokenTTL exposes the configured TTL so the HTTP layer can surface
// the "expires in" duration in the admin-UI pending-resets panel.
func (p *PasswordResetService) TokenTTL() time.Duration { return p.tokenTTL }

// IssueRequest is the result of RequestPasswordReset. PlaintextToken
// is the operator-facing token (for slog.WARN + the admin-UI panel);
// Empty when the user does not exist (enumeration defense -- the
// caller returns 200 either way).
type IssueRequest struct {
	Token          sqlcgen.PasswordResetToken
	PlaintextToken string
	ExpiresAt      time.Time
}

// RequestPasswordReset mints a new single-use token for the user
// identified by email. Returns (zero, false, nil) when no user has
// that email -- the caller (HTTP handler) treats both branches as
// 200 OK so a timing-side-channel attacker cannot enumerate users.
//
// On success, the plaintext token is also returned so the HTTP layer
// can slog.WARN it; the database stores only the sha256 hash.
func (p *PasswordResetService) RequestPasswordReset(ctx context.Context, email, ip, userAgent string) (IssueRequest, bool, error) {
	email = normalizeEmail(email)
	if email == "" {
		return IssueRequest{}, false, nil
	}
	user, err := p.svc.GetUserByEmailSource(ctx, email, "local")
	if err != nil {
		if errors.Is(err, ErrUserNotFound) {
			return IssueRequest{}, false, nil
		}
		return IssueRequest{}, false, fmt.Errorf("password-reset: lookup user: %w", err)
	}
	if user.DisabledAt.Valid {
		return IssueRequest{}, false, nil
	}
	plaintext, tokenHash, err := mintResetToken()
	if err != nil {
		return IssueRequest{}, false, fmt.Errorf("password-reset: mint token: %w", err)
	}
	now := p.svc.nowFn()
	expiresAt := now.Add(p.tokenTTL)
	createdBy := "self-service:" + ip
	var token sqlcgen.PasswordResetToken
	err = p.svc.withTx(ctx, func(q *sqlcgen.Queries) error {
		var txErr error
		token, txErr = q.CreatePasswordResetToken(ctx, sqlcgen.CreatePasswordResetTokenParams{
			UserID:    user.ID,
			TokenHash: tokenHash,
			CreatedAt: now.Unix(),
			ExpiresAt: expiresAt.Unix(),
			CreatedBy: createdBy,
		})
		if txErr != nil {
			return txErr
		}
		details := fmt.Sprintf(`{"token_id":%d,"kind":"self-service-request"}`, token.ID)
		return q.InsertLoginAudit(ctx, sqlcgen.InsertLoginAuditParams{
			UserID:            sql.NullInt64{Int64: user.ID, Valid: true},
			UsernamePresented: user.Username,
			Source:            "password-reset",
			Outcome:           "ok",
			Ip:                ip,
			UserAgent:         userAgent,
			Details:           details,
			CreatedAt:         now.Unix(),
		})
	})
	if err != nil {
		return IssueRequest{}, false, fmt.Errorf("password-reset: persist token: %w", err)
	}
	return IssueRequest{
		Token:          token,
		PlaintextToken: plaintext,
		ExpiresAt:      expiresAt,
	}, true, nil
}

// ConfirmResult is the outcome of ConfirmPasswordReset. NewPassword
// is set on the new-password flow; User is the rotated user.
type ConfirmResult struct {
	User sqlcgen.User
}

// ConfirmPasswordReset validates a plaintext token, rotates the
// user's password hash, marks the token used, and writes an audit
// row. Returns ErrTokenNotFound for any of: token not in DB, used,
// expired. The user-visible message is identical in all three
// branches.
func (p *PasswordResetService) ConfirmPasswordReset(ctx context.Context, plaintextToken, newPassword, ip, userAgent string) (ConfirmResult, error) {
	if len(plaintextToken) == 0 || len(newPassword) < 8 {
		return ConfirmResult{}, ErrTokenNotFound
	}
	tokenHash := hashResetToken(plaintextToken)
	now := p.svc.nowFn()
	usedAt := now.Unix()

	var (
		rotated           sqlcgen.User
		usernamePresented string
	)
	err := p.svc.withTx(ctx, func(q *sqlcgen.Queries) error {
		match, err := q.GetPasswordResetTokenByHash(ctx, sqlcgen.GetPasswordResetTokenByHashParams{
			TokenHash: tokenHash,
			Now:       now.Unix(),
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrTokenNotFound
			}
			return err
		}

		// CAS consume.
		consumed, err := q.ConsumePasswordResetToken(ctx, sqlcgen.ConsumePasswordResetTokenParams{
			ID:     match.ID,
			Now:    now.Unix(),
			UsedAt: usedAt,
		})
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrTokenNotFound
			}
			return err
		}

		user, err := q.GetUserByID(ctx, consumed.UserID)
		if err != nil {
			return fmt.Errorf("password-reset: lookup user after consume: %w", err)
		}
		usernamePresented = user.Username

		hash, err := p.svc.HashPassword(newPassword)
		if err != nil {
			return fmt.Errorf("password-reset: hash new password: %w", err)
		}
		rotated, err = updateUserPasswordHash(ctx, q, user.ID, hash, now.Unix())
		if err != nil {
			return err
		}

		// Revoke every active session for this user in the same
		// transaction as the password rotation: a session stolen
		// alongside the compromised password must not survive the
		// reset, and the writer pool's SetMaxOpenConns(1) makes the
		// two queries naturally serial, but doing both inside one tx
		// makes the atomicity obvious in the code and survives any
		// future move to a multi-writer pool. Idempotent: re-issuing
		// the same reset (already-revoked sessions match zero rows
		// in the WHERE revoked_at IS NULL clause).
		if err := revokeAllUserSessionsTx(ctx, q, user.ID, now.Unix()); err != nil {
			return fmt.Errorf("password-reset: revoke sessions: %w", err)
		}

		details := fmt.Sprintf(`{"token_id":%d,"kind":"self-service-confirm"}`, consumed.ID)
		return q.InsertLoginAudit(ctx, sqlcgen.InsertLoginAuditParams{
			UserID:            sql.NullInt64{Int64: user.ID, Valid: true},
			UsernamePresented: user.Username,
			Source:            "password-reset",
			Outcome:           "ok",
			Ip:                ip,
			UserAgent:         userAgent,
			Details:           details,
			CreatedAt:         now.Unix(),
		})
	})
	if err != nil {
		if errors.Is(err, ErrTokenNotFound) {
			p.svc.WriteLoginAudit(ctx, sql.NullInt64{}, usernamePresented, "password-reset", "user-locked", ip, userAgent, `{"kind":"self-service-confirm-failed"}`)
			return ConfirmResult{}, ErrTokenNotFound
		}
		return ConfirmResult{}, err
	}
	return ConfirmResult{User: rotated}, nil
}

// AdminResetResult is the result of AdminResetPassword. NewPassword
// is the freshly-minted plaintext password -- the caller returns it
// in the response body exactly once.
type AdminResetResult struct {
	User        sqlcgen.User
	NewPassword string
}

// AdminResetPassword rotates a user's password, returning a fresh
// 16-byte base64url-encoded plaintext. The plaintext surfaces in the
// HTTP response (and is logged to the admin-audit row in PR #408).
// It is NOT persisted -- only the argon2id hash is stored.
func (p *PasswordResetService) AdminResetPassword(ctx context.Context, targetUserID int64, actor string, ip, userAgent string) (AdminResetResult, error) {
	user, err := p.svc.GetUserByID(ctx, targetUserID)
	if err != nil {
		return AdminResetResult{}, err
	}
	plaintext, err := randomStrongPassword(16)
	if err != nil {
		return AdminResetResult{}, fmt.Errorf("password-reset: mint admin password: %w", err)
	}
	hash, err := p.svc.HashPassword(plaintext)
	if err != nil {
		return AdminResetResult{}, fmt.Errorf("password-reset: hash admin password: %w", err)
	}
	now := p.svc.nowFn()
	err = p.svc.withTx(ctx, func(q *sqlcgen.Queries) error {
		rotated, err := updateUserPasswordHash(ctx, q, user.ID, hash, now.Unix())
		if err != nil {
			return err
		}
		user = rotated
		// Revoke every active session for the target user. The reset
		// is the operator's signal that the existing credential is
		// compromised; a session the legitimate user had on a phone,
		// or that an attacker had stolen, must not survive. Idempotent.
		if err := revokeAllUserSessionsTx(ctx, q, user.ID, now.Unix()); err != nil {
			return fmt.Errorf("password-reset: revoke sessions: %w", err)
		}
		details := fmt.Sprintf(`{"actor":"%s","target_user_id":%d,"kind":"admin-reset"}`, sanitizeForJSON(actor), user.ID)
		return q.InsertLoginAudit(ctx, sqlcgen.InsertLoginAuditParams{
			UserID:            sql.NullInt64{Int64: user.ID, Valid: true},
			UsernamePresented: user.Username,
			Source:            "password-reset",
			Outcome:           "ok",
			Ip:                ip,
			UserAgent:         userAgent,
			Details:           details,
			CreatedAt:         now.Unix(),
		})
	})
	if err != nil {
		return AdminResetResult{}, err
	}
	return AdminResetResult{User: user, NewPassword: plaintext}, nil
}

// ListPending returns the active (un-consumed, un-expired) tokens
// across all users, paginated for the admin-UI panel.
func (p *PasswordResetService) ListPending(ctx context.Context, limit, offset int64) ([]sqlcgen.PasswordResetToken, error) {
	return p.svc.db.Reader.ListAllActivePasswordResetTokens(ctx, sqlcgen.ListAllActivePasswordResetTokensParams{
		Now:    p.svc.nowFn().Unix(),
		Limit:  limit,
		Offset: offset,
	})
}

// ListPendingForUser returns the active tokens for a single user.
// Used by the admin users page in PR #408.
func (p *PasswordResetService) ListPendingForUser(ctx context.Context, userID int64) ([]sqlcgen.PasswordResetToken, error) {
	return p.svc.db.Reader.ListActivePasswordResetTokens(ctx, sqlcgen.ListActivePasswordResetTokensParams{
		UserID: userID,
		Now:    p.svc.nowFn().Unix(),
	})
}

// RevokeToken marks a token as used. Idempotent.
func (p *PasswordResetService) RevokeToken(ctx context.Context, tokenID int64) error {
	return p.svc.withTx(ctx, func(q *sqlcgen.Queries) error {
		return q.RevokePasswordResetToken(ctx, sqlcgen.RevokePasswordResetTokenParams{
			ID:     tokenID,
			UsedAt: p.svc.nowFn().Unix(),
		})
	})
}

// --- helpers ---

// updateUserPasswordHash is a thin wrapper around the sqlcgen query
// that exists to keep the password-reset file self-contained and
// future-proof: when #410 (MFA) needs to record password_changed_at
// for its re-auth grace window, the rotation logic here is the only
// place to add it.
func updateUserPasswordHash(ctx context.Context, q *sqlcgen.Queries, userID int64, hash string, _ int64) (sqlcgen.User, error) {
	return q.UpdateUserPasswordHash(ctx, sqlcgen.UpdateUserPasswordHashParams{
		ID:           userID,
		PasswordHash: sql.NullString{String: hash, Valid: true},
	})
}

// revokeAllUserSessionsTx is the in-transaction form of
// Service.RevokeAllUserSessions: it calls the sqlc query directly
// rather than going through Service.withTx, which would attempt a
// nested transaction (and fail on SQLite's single-writer pool).
// Both reset paths use it inside their own withTx scope so the
// password rotation and the session revocation commit together.
func revokeAllUserSessionsTx(ctx context.Context, q *sqlcgen.Queries, userID, revokedAt int64) error {
	return q.RevokeAllUserSessions(ctx, sqlcgen.RevokeAllUserSessionsParams{
		UserID:    userID,
		RevokedAt: sql.NullInt64{Int64: revokedAt, Valid: true},
	})
}

// mintResetToken returns (plaintext-base64url, sha256-hex). The
// plaintext is 32 random bytes; the base64url form is 43 characters
// (no padding), transport-safe.
func mintResetToken() (plaintext, hashHex string, err error) {
	buf := make([]byte, ResetTokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plaintext = base64URLStrip(buf)
	sum := sha256.Sum256([]byte(plaintext))
	hashHex = hex.EncodeToString(sum[:])
	return plaintext, hashHex, nil
}

// hashResetToken computes the same sha256-hex that mintResetToken
// stored. Kept as a separate function so the HTTP layer can validate
// a confirm-time token without re-minting.
func hashResetToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// base64URLStrip is stdlib base64.RawURLEncoding without an import in
// the file (the package's existing imports don't need encoding/base64
// outside this one place). Returns the URL-safe encoding with no
// padding, suitable for transport in JSON bodies and slog lines.
func base64URLStrip(b []byte) string {
	const alphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789-_"
	out := make([]byte, 0, ((len(b)+2)/3)*4)
	n := len(b) / 3 * 3
	for i := 0; i < n; i += 3 {
		v := uint32(b[i])<<16 | uint32(b[i+1])<<8 | uint32(b[i+2])
		out = append(out,
			alphabet[(v>>18)&0x3f],
			alphabet[(v>>12)&0x3f],
			alphabet[(v>>6)&0x3f],
			alphabet[v&0x3f],
		)
	}
	rem := len(b) - n
	switch rem {
	case 1:
		v := uint32(b[n]) << 16
		out = append(out,
			alphabet[(v>>18)&0x3f],
			alphabet[(v>>12)&0x3f],
		)
	case 2:
		v := uint32(b[n])<<16 | uint32(b[n+1])<<8
		out = append(out,
			alphabet[(v>>18)&0x3f],
			alphabet[(v>>12)&0x3f],
			alphabet[(v>>6)&0x3f],
		)
	}
	return string(out)
}

// randomStrongPassword returns a 16-byte random password, base64url-
// encoded without padding (22 characters). The "strength" here is the
// 128 bits of entropy in 16 random bytes; operators are expected to
// communicate the result out-of-band (the response body of the admin
// reset endpoint), so the alphabet doesn't need to be human-pronounceable.
func randomStrongPassword(n int) (string, error) {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64URLStrip(buf), nil
}

func normalizeEmail(e string) string {
	for i := 0; i < len(e); i++ {
		if e[i] == ' ' || e[i] == '\t' || e[i] == '\n' || e[i] == '\r' {
			return ""
		}
	}
	return e
}

// sanitizeForJSON escapes characters that would break the JSON
// details column. Only used for the admin actor field, which is
// expected to be a username or "admin:<username>" -- short,
// alphanumeric + colon.
func sanitizeForJSON(s string) string {
	out := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c == '"' || c == '\\' || c < 0x20 {
			continue
		}
		out = append(out, c)
	}
	return string(out)
}
