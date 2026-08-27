// Package secrets encrypts settings values that must not be stored in
// plaintext in app_settings (immich.apiKey, agent.apiKey). The key comes
// from BRANCHDAM_SECRET_KEY (base64, 32 bytes), never from config.yaml or
// the database -- a secret encrypted with a key that itself lived next to
// the ciphertext would protect nothing.
//
// A missing or invalid key is deliberately non-fatal: internal/settings
// treats it as "no secret overrides available" and falls back to the
// config/env base value rather than refusing to boot the whole server over
// one settings key. See Box's doc comment for the exact failure contract.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strings"
)

// version1Prefix identifies the encryption scheme a stored value uses, so a
// future key-rotation or algorithm change can recognize old values instead
// of guessing.
const version1Prefix = "v1:"

// ErrUnavailable is returned by Box.Seal when no key was configured -- the
// caller (internal/settings) turns this into a 422 telling the operator to
// set BRANCHDAM_SECRET_KEY, rather than silently storing plaintext.
var ErrUnavailable = errors.New("secrets: BRANCHDAM_SECRET_KEY not set")

// ErrInvalidKey is returned by NewBox when BRANCHDAM_SECRET_KEY is set but
// is not valid base64-encoded 32 bytes (AES-256 key size).
var ErrInvalidKey = errors.New("secrets: BRANCHDAM_SECRET_KEY must be base64-encoded 32 bytes")

// ErrDecryptFailed covers both a wrong/rotated key and corrupted storage --
// GCM's authentication tag makes the two indistinguishable, and callers
// treat them identically (fall back to the base value, log, surface in UI).
var ErrDecryptFailed = errors.New("secrets: decryption failed (wrong or rotated key?)")

// Box seals and opens secret values. A nil *Box (from NewBox with an unset
// key) is safe to call Seal/Open on -- both return ErrUnavailable /
// ErrDecryptFailed rather than panicking, since internal/settings must keep
// serving non-secret fields when no key is configured.
type Box struct {
	gcm cipher.AEAD
}

// NewBox builds a Box from BRANCHDAM_SECRET_KEY's value. An empty string
// (the env var unset) returns (nil, nil) -- not an error -- so callers can
// distinguish "not configured" (degrade gracefully) from "configured but
// invalid" (a real operator mistake worth failing loudly on, since unlike a
// missing key there is no plausible reason to have set it wrong on purpose).
func NewBox(base64Key string) (*Box, error) {
	if base64Key == "" {
		return nil, nil
	}
	key, err := base64.StdEncoding.DecodeString(base64Key)
	if err != nil || len(key) != 32 {
		return nil, ErrInvalidKey
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("secrets: build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("secrets: build GCM: %w", err)
	}
	return &Box{gcm: gcm}, nil
}

// Seal encrypts plaintext into the versioned, storable string form.
func (b *Box) Seal(plaintext string) (string, error) {
	if b == nil {
		return "", ErrUnavailable
	}
	nonce := make([]byte, b.gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("secrets: generate nonce: %w", err)
	}
	sealed := b.gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return version1Prefix + base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value produced by Seal. Any authentication failure
// (wrong key, rotated key, corrupted row) returns ErrDecryptFailed --
// callers must not distinguish these to avoid leaking which is which.
func (b *Box) Open(stored string) (string, error) {
	if b == nil {
		return "", ErrUnavailable
	}
	if !strings.HasPrefix(stored, version1Prefix) {
		return "", ErrDecryptFailed
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(stored, version1Prefix))
	if err != nil {
		return "", ErrDecryptFailed
	}
	nonceSize := b.gcm.NonceSize()
	if len(raw) < nonceSize {
		return "", ErrDecryptFailed
	}
	nonce, ciphertext := raw[:nonceSize], raw[nonceSize:]
	plaintext, err := b.gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", ErrDecryptFailed
	}
	return string(plaintext), nil
}
