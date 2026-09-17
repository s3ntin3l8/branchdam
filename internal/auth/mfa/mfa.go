// Package mfa owns TOTP-based multi-factor authentication for local users.
//
// The v1 contract is TOTP only (RFC 6238, 30s window, 6 digits, SHA-1).
// Recovery codes serve as a single-use backup factor.
//
// MFA enrollment flow:
//  1. POST /mfa/setup -> generates secret, stores as mfa_pending_secret
//  2. POST /mfa/enable {code} -> validates code, promotes to mfa_credentials,
//     mints 8 recovery codes, returns them once
//
// Login flow (when user has mfa_credentials):
//  1. POST /login -> password OK -> creates half-auth session (mfa_verified_at=NULL)
//     -> returns {mfaRequired: true}
//  2. POST /mfa/challenge {code} -> validates TOTP or recovery code
//     -> sets mfa_verified_at -> returns {ok: true}
//
// Disable flow:
//  3. POST /mfa/disable {password, code} -> verifies password + TOTP
//     -> deletes mfa_credentials + recovery codes
package mfa

import (
	"context"
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"database/sql"
	"encoding/base32"
	"encoding/binary"
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
	DefaultTOTPPeriod  = 30
	DefaultTOTPDigits  = 6
	DefaultTOTPAlgo    = "SHA1"
	DriftTolerance     = 1
	RecoveryCodeCount  = 8
	RecoveryCodeLength = 10
	PendingSecretTTL   = 15 * time.Minute
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

	if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.SetMFAPendingSecret(ctx, sqlcgen.SetMFAPendingSecretParams{
			ID:               userID,
			MfaPendingSecret: sql.NullString{String: encrypted, Valid: true},
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

func (s *Service) Enable(ctx context.Context, userID int64, code string) ([]string, error) {
	row, err := s.db.Reader.GetMFAPendingSecret(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("get pending secret: %w", err)
	}
	if !row.MfaPendingSecret.Valid || row.MfaPendingSecret.String == "" {
		return nil, fmt.Errorf("no pending MFA setup; call /mfa/setup first")
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

	if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		if err := q.UpsertMFACredentials(ctx, sqlcgen.UpsertMFACredentialsParams{
			UserID:          userID,
			SecretEncrypted: row.MfaPendingSecret.String,
			Algo:            DefaultTOTPAlgo,
			Digits:          int64(DefaultTOTPDigits),
			Period:          int64(DefaultTOTPPeriod),
			LastUsedStep:    step,
		}); err != nil {
			return fmt.Errorf("upsert mfa credentials: %w", err)
		}
		return q.ClearMFAPendingSecret(ctx, userID)
	}); err != nil {
		return nil, err
	}

	codes, hashes, err := generateRecoveryCodes(RecoveryCodeCount)
	if err != nil {
		return nil, fmt.Errorf("generate recovery codes: %w", err)
	}
	for _, hash := range hashes {
		if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
			return q.InsertRecoveryCodes(ctx, sqlcgen.InsertRecoveryCodesParams{
				UserID:   userID,
				CodeHash: hash,
			})
		}); err != nil {
			return nil, fmt.Errorf("insert recovery code: %w", err)
		}
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

	if !validateTOTP(secret, code, step) {
		return false, nil
	}

	if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.UpdateLastUsedStep(ctx, sqlcgen.UpdateLastUsedStepParams{
			UserID:       userID,
			LastUsedStep: step,
		})
	}); err != nil {
		s.log.Warn("failed to update last_used_step", "userID", userID, "error", err)
	}

	return true, nil
}

func (s *Service) ValidateRecoveryCode(ctx context.Context, userID int64, code string) (bool, error) {
	hash := hashRecoveryCode(code)

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
	if _, err := generateRandomBytes(buf); err != nil {
		return "", err
	}
	return base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf), nil
}

func validateTOTP(secret, code string, step int64) bool {
	for i := -DriftTolerance; i <= DriftTolerance; i++ {
		candidateStep := step + int64(i)
		if candidateStep < 0 {
			continue
		}
		expected := computeTOTP(secret, candidateStep)
		if subtleConstantTimeEqual(code, expected) {
			return true
		}
	}
	return false
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

func subtleConstantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// --- Recovery codes ---

func generateRecoveryCodes(count int) (codes []string, hashes []string, err error) {
	codes = make([]string, count)
	hashes = make([]string, count)
	for i := 0; i < count; i++ {
		code, genErr := generateRecoveryCode(RecoveryCodeLength)
		if genErr != nil {
			return nil, nil, genErr
		}
		codes[i] = code
		hashes[i] = hashRecoveryCode(code)
	}
	return codes, hashes, nil
}

func generateRecoveryCode(length int) (string, error) {
	const charset = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	buf := make([]byte, length)
	if _, err := generateRandomBytes(buf); err != nil {
		return "", err
	}
	for i := range buf {
		buf[i] = charset[int(buf[i])%len(charset)]
	}
	return string(buf), nil
}

func hashRecoveryCode(code string) string {
	h := sha256.Sum256([]byte(code))
	return fmt.Sprintf("%x", h)
}
