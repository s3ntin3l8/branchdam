// Package mfa owns TOTP-based multi-factor authentication for local users.
//
// The v1 contract is TOTP only (RFC 6238, 30s window, 6 digits, SHA-1).
// Recovery codes serve as a single-use backup factor.
//
// MFA enrollment flow:
//  1. POST /mfa/setup -> generates secret, stores as mfa_pending_secret
//     with mfa_pending_secret_created_at stamped for PendingSecretTTL
//  2. POST /mfa/enable {code} -> validates code (and TTL), promotes to
//     mfa_credentials with a random per-user recovery_code_salt, mints
//     8 recovery codes (each sha256(salt||code)), returns them once
//
// Login flow (when user has mfa_credentials):
//  1. POST /login -> password OK -> creates half-auth session
//     (mfa_verified_at=NULL) -> returns {mfaRequired: true}
//  2. POST /mfa/challenge {code} -> validates TOTP (with replay
//     protection via last_used_step) or recovery code -> sets
//     mfa_verified_at -> returns {ok: true}
//
// Disable flow:
//  3. POST /mfa/disable {password, code} -> verifies password + TOTP
//     -> deletes mfa_credentials + recovery codes
//
// Hardening notes (review feedback on PR #459):
//   - last_used_step is read on every TOTP validate: a candidate step
//     <= stored step is rejected, so a code observed once cannot be
//     replayed within the same 30s window (Issue 6).
//   - PendingSecretTTL is enforced at Enable time via
//     mfa_pending_secret_created_at: a pending secret older than the
//     TTL is rejected and the user has to call /mfa/setup again
//     (Issue 7).
//   - Recovery code hashes use sha256(recovery_code_salt||code) -- a
//     16-byte per-user random salt, stored alongside the TOTP secret
//     envelope on mfa_credentials (Issue 8).
//   - TOTP comparison uses crypto/subtle.ConstantTimeCompare, not the
//     hand-rolled early-exit loop (Issue 9).
package mfa

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/secrets"
)

const (
	DefaultTOTPPeriod     = 30
	DefaultTOTPDigits     = 6
	DefaultTOTPAlgo       = "SHA1"
	DriftTolerance        = 1
	RecoveryCodeCount     = 8
	RecoveryCodeLength    = 10
	PendingSecretTTL      = 15 * time.Minute
	recoveryCodeSaltBytes = 16
)

type Service struct {
	db  *db.DB
	box *secrets.Box
	log *slog.Logger
}

func NewService(database *db.DB, box *secrets.Box, log *slog.Logger) *Service {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Service{db: database, box: box, log: log}
}

type SetupResult struct {
	OtpauthURI string
	ASCIIQR    string
}

func (s *Service) Setup(ctx context.Context, userID int64, username string) (*SetupResult, error) {
	secret, err := generateSecret(20)
	if err != nil {
		return nil, fmt.Errorf("generate secret: %w", err)
	}

	encrypted, err := s.box.Seal(secret)
	if err != nil {
		return nil, fmt.Errorf("encrypt secret: %w", err)
	}

	now := time.Now().Unix()
	if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.SetMFAPendingSecret(ctx, sqlcgen.SetMFAPendingSecretParams{
			ID:                        userID,
			MfaPendingSecret:          sql.NullString{String: encrypted, Valid: true},
			MfaPendingSecretCreatedAt: sql.NullInt64{Int64: now, Valid: true},
		})
	}); err != nil {
		return nil, fmt.Errorf("store pending secret: %w", err)
	}

	otpauthURI := fmt.Sprintf("otpauth://totp/branchDAM:%s?secret=%s&issuer=branchDAM&algorithm=%s&digits=%d&period=%d",
		username, secret, DefaultTOTPAlgo, DefaultTOTPDigits, DefaultTOTPPeriod)

	return &SetupResult{
		OtpauthURI: otpauthURI,
		ASCIIQR:    fmt.Sprintf("Scan this URI in your authenticator app:\n%s", otpauthURI),
	}, nil
}

// Enable validates the presented TOTP code against the pending secret,
// promotes it to mfa_credentials, and mints RecoveryCodeCount recovery
// codes (each salted + sha256-hashed). Returns the plaintext codes
// exactly once.
//
// Refuses a pending secret older than PendingSecretTTL (Issue 7): the
// user must call /mfa/setup again. Refuses if MFA is already enabled --
// the handler layer enforces this earlier; the redundant check here is
// belt-and-braces against the credentials row appearing between
// handler check and store.
//
// All RecoveryCodeCount inserts happen inside ONE write transaction
// (Issue 11): a failure mid-loop used to commit a partial set while
// Enable returned an error, leaving the user with no plaintext codes
// but a partial DB set. Now the whole set either commits or rolls back.
func (s *Service) Enable(ctx context.Context, userID int64, code string) ([]string, error) {
	row, err := s.db.Reader.GetMFAPendingSecret(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get pending secret: %w", err)
	}
	if !row.MfaPendingSecret.Valid || row.MfaPendingSecret.String == "" {
		return nil, fmt.Errorf("no pending MFA setup; call /mfa/setup first")
	}
	if !row.MfaPendingSecretCreatedAt.Valid {
		return nil, fmt.Errorf("no pending MFA setup; call /mfa/setup first")
	}
	if time.Since(time.Unix(row.MfaPendingSecretCreatedAt.Int64, 0)) > PendingSecretTTL {
		// Clear the stale pending secret so the next /mfa/setup
		// call isn't blocked by an old envelope.
		_ = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
			return q.ClearMFAPendingSecret(ctx, userID)
		})
		return nil, fmt.Errorf("pending MFA setup expired; call /mfa/setup again")
	}

	secret, err := s.box.Open(row.MfaPendingSecret.String)
	if err != nil {
		return nil, fmt.Errorf("decrypt pending secret: %w", err)
	}

	now := time.Now().Unix()
	step := now / int64(DefaultTOTPPeriod)
	if !validateTOTP(secret, code, step) {
		return nil, fmt.Errorf("invalid TOTP code")
	}

	salt, err := generateRecoveryCodeSalt()
	if err != nil {
		return nil, fmt.Errorf("generate recovery code salt: %w", err)
	}

	codes, hashes, err := generateRecoveryCodes(RecoveryCodeCount, salt)
	if err != nil {
		return nil, fmt.Errorf("generate recovery codes: %w", err)
	}

	if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.UpsertMFACredentials(ctx, sqlcgen.UpsertMFACredentialsParams{
			UserID:           userID,
			SecretEncrypted:  row.MfaPendingSecret.String,
			Algo:             DefaultTOTPAlgo,
			Digits:           int64(DefaultTOTPDigits),
			Period:           int64(DefaultTOTPPeriod),
			LastUsedStep:     step,
			RecoveryCodeSalt: salt,
		}); err != nil {
			return fmt.Errorf("upsert mfa credentials: %w", err)
		}
		if err := q.ClearMFAPendingSecret(ctx, userID); err != nil {
			return fmt.Errorf("clear pending secret: %w", err)
		}
		// Insert all recovery codes in this single tx; a failure
		// on any one rolls back the whole set, leaving Enable's
		// "no codes returned to user" contract consistent with
		// the DB state (Issue 11).
		for _, hash := range hashes {
			if err := q.InsertRecoveryCodes(ctx, sqlcgen.InsertRecoveryCodesParams{
				UserID:   userID,
				CodeHash: hash,
			}); err != nil {
				return fmt.Errorf("insert recovery code: %w", err)
			}
		}
		return nil
	}); err != nil {
		return nil, err
	}

	return codes, nil
}

func (s *Service) Disable(ctx context.Context, userID int64, verifyPassword func(hash string) error, code string) error {
	creds, err := s.db.Reader.GetMFACredentials(ctx, userID)
	if err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("MFA is not enabled")
		}
		return fmt.Errorf("get mfa credentials: %w", err)
	}

	user, err := s.db.Reader.GetUserByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("get user: %w", err)
	}
	if !user.PasswordHash.Valid {
		return fmt.Errorf("user has no password")
	}
	if err := verifyPassword(user.PasswordHash.String); err != nil {
		return fmt.Errorf("invalid password")
	}

	secret, err := s.box.Open(creds.SecretEncrypted)
	if err != nil {
		return fmt.Errorf("decrypt secret: %w", err)
	}
	now := time.Now().Unix()
	step := now / int64(creds.Period)
	if !validateTOTP(secret, code, step) {
		return fmt.Errorf("invalid TOTP code")
	}

	return s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.DeleteMFACredentials(ctx, userID); err != nil {
			return fmt.Errorf("delete mfa credentials: %w", err)
		}
		return q.DeleteRecoveryCodes(ctx, userID)
	})
}

// ValidateTOTPCode checks the presented code against the user's TOTP
// credentials with both drift tolerance and replay protection
// (Issue 6): a candidate step <= the stored last_used_step is
// rejected, so an attacker who observes a valid code cannot replay
// it within the same 30s window. The accepted step is then written
// back as the new last_used_step, so the same comparison rejects any
// future replay attempt in that window.
func (s *Service) ValidateTOTPCode(ctx context.Context, userID int64, code string) (bool, error) {
	creds, err := s.db.Reader.GetMFACredentials(ctx, userID)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("get mfa credentials: %w", err)
	}

	secret, err := s.box.Open(creds.SecretEncrypted)
	if err != nil {
		return false, fmt.Errorf("decrypt secret: %w", err)
	}

	now := time.Now().Unix()
	step := now / int64(creds.Period)

	acceptedStep, ok := acceptTOTP(secret, code, step, creds.LastUsedStep)
	if !ok {
		return false, nil
	}

	if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.UpdateLastUsedStep(ctx, sqlcgen.UpdateLastUsedStepParams{
			UserID:       userID,
			LastUsedStep: acceptedStep,
		})
	}); err != nil {
		s.log.Warn("failed to update last_used_step", "userID", userID, "error", err)
	}

	return true, nil
}

func (s *Service) ValidateRecoveryCode(ctx context.Context, userID int64, code string) (bool, error) {
	creds, err := s.db.Reader.GetMFACredentials(ctx, userID)
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("get mfa credentials: %w", err)
	}
	hash := hashRecoveryCode(code, creds.RecoveryCodeSalt)

	row, err := s.db.Reader.FindUnusedRecoveryCode(ctx, sqlcgen.FindUnusedRecoveryCodeParams{
		UserID:   userID,
		CodeHash: hash,
	})
	if err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, fmt.Errorf("find recovery code: %w", err)
	}

	now := time.Now().Unix()
	if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.MarkRecoveryCodeUsed(ctx, sqlcgen.MarkRecoveryCodeUsedParams{
			ID:     row.ID,
			UsedAt: sql.NullInt64{Int64: now, Valid: true},
		})
	}); err != nil {
		return false, fmt.Errorf("mark recovery code used: %w", err)
	}

	return true, nil
}

func (s *Service) HasMFA(ctx context.Context, userID int64) bool {
	_, err := s.db.Reader.GetMFACredentials(ctx, userID)
	return err == nil
}

// --- TOTP (RFC 6238) ---

func generateSecret(length int) (string, error) {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

// validateTOTP is the pure check used at setup/disable time, where
// there's no prior last_used_step to defend against. ValidateTOTPCode
// is the per-session entry point that adds replay protection.
func validateTOTP(secret, code string, step int64) bool {
	for i := -DriftTolerance; i <= DriftTolerance; i++ {
		candidateStep := step + int64(i)
		if candidateStep < 0 {
			continue
		}
		expected := computeTOTP(secret, candidateStep)
		if subtle.ConstantTimeCompare([]byte(code), []byte(expected)) == 1 {
			return true
		}
	}
	return false
}

// acceptTOTP mirrors validateTOTP but additionally enforces
// lastUsedStep < candidateStep (Issue 6: replay protection within the
// same 30s window). Returns the step that was accepted, so the caller
// can persist it as the new last_used_step and a future identical
// challenge fails on the same gate.
func acceptTOTP(secret, code string, step, lastUsedStep int64) (int64, bool) {
	for i := -DriftTolerance; i <= DriftTolerance; i++ {
		candidateStep := step + int64(i)
		if candidateStep <= lastUsedStep {
			continue
		}
		expected := computeTOTP(secret, candidateStep)
		if subtle.ConstantTimeCompare([]byte(code), []byte(expected)) == 1 {
			return candidateStep, true
		}
	}
	return 0, false
}

func computeTOTP(secret string, step int64) string {
	key, err := base32.StdEncoding.WithPadding(base32.NoPadding).DecodeString(strings.ToUpper(secret))
	if err != nil {
		return ""
	}

	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(step))

	mac := hmac.New(sha1.New, key)
	mac.Write(buf[:])
	sum := mac.Sum(nil)

	offset := sum[len(sum)-1] & 0x0f
	code := binary.BigEndian.Uint32(sum[offset:offset+4]) & 0x7fffffff
	code = code % uint32(math.Pow10(DefaultTOTPDigits))

	return fmt.Sprintf("%0*d", DefaultTOTPDigits, code)
}

// --- Recovery codes ---

func generateRecoveryCodeSalt() (string, error) {
	buf := make([]byte, recoveryCodeSaltBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("read salt: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

func generateRecoveryCodes(count int, salt string) (codes []string, hashes []string, err error) {
	codes = make([]string, count)
	hashes = make([]string, count)
	for i := 0; i < count; i++ {
		code, genErr := generateRecoveryCode(RecoveryCodeLength)
		if genErr != nil {
			return nil, nil, genErr
		}
		codes[i] = code
		hashes[i] = hashRecoveryCode(code, salt)
	}
	return codes, hashes, nil
}

func generateRecoveryCode(length int) (string, error) {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = charset[int(buf[i])%len(charset)]
	}
	return string(buf), nil
}

// hashRecoveryCode returns hex(sha256(salt || code)). salt is the
// per-user 16-byte random salt stored on mfa_credentials (Issue 8);
// without it the 50-bit recovery codes are brute-forceable from an
// offline DB dump in seconds.
func hashRecoveryCode(code, salt string) string {
	h := sha256.New()
	h.Write([]byte(salt))
	h.Write([]byte(code))
	return hex.EncodeToString(h.Sum(nil))
}
