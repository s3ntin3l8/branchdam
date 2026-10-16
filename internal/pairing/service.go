// Package pairing owns the per-device API key issuance, rotation, and
// revocation flow for /api/v1/agent/* authentication. It's the only
// place in the codebase that mints the secrets devices send as X-API-Key,
// and it's the only consumer of the device_pairings /
// device_pairing_keys / companion_pairing_audit tables introduced in
// migration 00017.
//
// The Service is layered on top of *db.DB (single-writer + multi-reader)
// and the sqlc-generated queries. Two public surfaces:
//
//   - CreatePairing / RotateKey / RevokePairing / ListKeys / ListPairings:
//     the admin /api/v1/companion/pairings HTTP handlers. All write paths
//     run inside InTx so the pairing row, key row(s), and audit row(s)
//     commit atomically.
//
//   - KeyLookup / LatestActiveKey: read-side, called by internal/auth
//     AgentChain on every authenticated agent request. KeyLookup is the
//     hot path (one indexed SELECT per request); the callback abstraction
//     in auth.AgentConfig.LookupKey delegates here.
package pairing

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
	"strings"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/qr"
	"github.com/s3ntin3l8/branchdam/internal/secrets"
)

// Pairing is the public-facing device record.
type Pairing struct {
	ID            int64
	AgentID       string
	FriendlyLabel string
	CreatedAt     int64
	CreatedBy     string
	RevokedAt     sql.NullInt64
	// UserID is the nullable owner FK from device_pairings.user_id
	// (migration 00020). Set at create time from the creating request's
	// resolved attribution user id; NULL for legacy pairings created
	// before this PR landed. Used by the agent upload path to attribute
	// paired-device uploads back to the human who paired them.
	UserID sql.NullInt64
}

// Key is a device's API key, plaintext included. The plaintext shown to
// the operator is the ONLY copy callers ever see; it is never persisted
// as such. QRSVG is the rendered SVG QR code (computed once at mint
// time) and PayloadURL is the branchdam:// payload it encodes -- both
// are cached on the pairing row so GET /qr.svg and the credentials
// endpoint can re-serve them without re-rendering, but BOTH embed the
// key and are therefore sealed with secrets.Box before they reach the
// database whenever BRANCHDAM_SECRET_KEY is configured (with no key
// configured the pairing_url column stays NULL -- never plaintext; the
// SVG falls back to plaintext storage, same as before sealing existed).
// Both are regenerated on every RotateKey, so an old copy never sits in
// front of a new key.
type Key struct {
	ID         int64
	PairingID  int64
	Plaintext  string
	LookupHash string
	Preview    string
	CreatedAt  int64
	ExpiresAt  sql.NullInt64
	RevokedAt  sql.NullInt64
	QRSVG      []byte
	PayloadURL string
}

// Credentials is the re-displayable material behind the SPA's "Show
// credentials" modal: the decrypted QR SVG for the current active key
// plus the pairing URL when (and only when) one was stored sealed.
// PairingURL is empty when the column is NULL -- a keyless server or a
// legacy row the operator hasn't rotated yet. The API key itself is not
// carried here: HTTP handlers parse it out of PairingURL so the key
// never lingers in a second field. KeyPreview is always populated from
// the newest active key's last-4 (never key material) so the SPA can
// show a stable masked value even when PairingURL is unavailable.
// SealingEnabled is s.box != nil -- whether a secrets box is configured
// on this server *right now*, not row provenance. It only answers the
// empty-PairingURL question: true => "legacy row -- rotate to seal",
// false => "keyless server" (Hermes round 3/4).
type Credentials struct {
	AgentID        string
	FriendlyLabel  string
	QRSVG          []byte
	PairingURL     string
	KeyPreview     string
	SealingEnabled bool
}

// PairingRow is the joined list-row shape returned by ListPairings: it
// includes the active-key count for UI display.
type PairingRow struct {
	ID             int64
	AgentID        string
	FriendlyLabel  string
	CreatedAt      int64
	CreatedBy      string
	RevokedAt      sql.NullInt64
	ActiveKeyCount int64
	NextExpiryUnix any
	LastKeyAt      any
}

// ErrPairingNotFound is returned when a pairing_id / agent_id lookup has
// no row (or the row exists but is revoked). Distinct from sql.ErrNoRows
// so callers can branch without string-matching SQLite errors.
var ErrPairingNotFound = errors.New("pairing: not found")

// ErrNoActiveKey is returned by ActiveQRSVG when the pairing exists but
// has no active key to render a QR for (all keys revoked/expired or the
// pairing was created in a future where qr_svg was a required field).
var ErrNoActiveKey = errors.New("pairing: no active key")

// ErrPairingNotRevoked is returned by DeletePairing when the caller
// attempts to hard-delete a pairing that hasn't been revoked first.
var ErrPairingNotRevoked = errors.New("pairing: not revoked")

// ErrEmptyLabel is returned by RenamePairing when the trimmed label is
// empty. The HTTP layer's Huma validation (minLength 1) catches this
// first; the service check is defense in depth for non-HTTP callers.
var ErrEmptyLabel = errors.New("pairing: empty label")

// Service is the pairing package's only externally-constructed type.
type Service struct {
	db    *db.DB
	log   *slog.Logger
	nowFn func() int64

	// pepper is a 32-byte secret derived from BRANCHDAM_SECRET_KEY used as
	// the HMAC-SHA256 key for API key lookup hashes. This provides defense
	// in depth: a leaked database alone is insufficient to match presented
	// keys without the server-side pepper.
	pepper []byte

	// box seals credential material at rest (qr_svg, pairing_url) under
	// BRANCHDAM_SECRET_KEY. nil means "no key configured": writes skip
	// sealing (pairing_url stays NULL, qr_svg stored plaintext as
	// pre-sealing rows were) and reads of a sealed row fail with
	// secrets.ErrUnavailable instead of serving ciphertext as an SVG.
	box *secrets.Box
}

// defaultPepper is used only when no BRANCHDAM_SECRET_KEY is configured.
// It still satisfies CodeQL (HMAC, not bare hash) but offers no protection
// against a stolen database — pairing should only be used with a real
// secret key in production.
var defaultPepper = sha256.Sum256([]byte("branchdam-pairing-default-pepper"))

// DefaultPepper returns the deterministic fallback pepper used when no
// BRANCHDAM_SECRET_KEY is configured. Exported so other consumers of
// the same pepper (the PAT service, the bootstrap mint) can mirror
// NewService's fallback instead of panicking on an empty key -- the
// two services must always hash under the SAME pepper, so the fallback
// has exactly one definition.
func DefaultPepper() []byte { return defaultPepper[:] }

// NewService constructs a Service backed by db. pepper is the raw 32-byte
// HMAC key derived from BRANCHDAM_SECRET_KEY; pass nil to use a
// deterministic fallback (not recommended for production). box seals
// credential material at rest; pass nil when BRANCHDAM_SECRET_KEY is
// unset (dev/test -- credential re-display degrades to QR-only). log may
// be nil for quieter callers (tests).
func NewService(database *db.DB, log *slog.Logger, pepper []byte, box *secrets.Box) *Service {
	if log == nil {
		log = slog.Default()
	}
	if len(pepper) == 0 {
		pepper = defaultPepper[:]
	}
	return &Service{db: database, log: log, nowFn: nowUnix, pepper: pepper, box: box}
}

func nowUnix() int64 { return time.Now().Unix() }

// CreatePairing mints a new device pairing: a unique agent_id, an
// initial API key, the rendered QR SVG, and a PAIR_CREATED +
// KEY_MINTED audit pair. Returns the pairing and the key (with
// plaintext + SVG); the plaintext is shown to the operator exactly once
// and never persisted.
//
// qrPayloadFor is a closure the HTTP layer builds from the request's
// X-Forwarded-* headers (so the QR carries the externally-reachable
// URL). Passed as a function so the service stays unaware of net/http.

// withTx runs fn inside the writer pool's single transaction. Exposed
// for tests that need to insert rows with constraints the public API
// doesn't cover (e.g. deliberately creating a duplicate agent_id to
// verify the UNIQUE constraint).
func (s *Service) withTx(ctx context.Context, fn func(*sqlcgen.Queries) error) error {
	return s.db.InTx(ctx, fn)
}

// sealCredential returns the values to persist for qr_svg and
// pairing_url. With a box configured both are sealed (pairing_url holds
// the plaintext-embedding branchdam:// payload, so it is only ever
// written sealed -- migration 00034's invariant). With no box, qr_svg
// is stored plaintext (pre-sealing behavior) and pairing_url is NULL:
// keyless servers never put key material at rest beyond the QR itself.
func (s *Service) sealCredential(svg []byte, payloadURL string) (storedSVG []byte, storedURL sql.NullString, err error) {
	if s.box == nil {
		return svg, sql.NullString{}, nil
	}
	sealedSVG, err := s.box.SealBytes(svg)
	if err != nil {
		return nil, sql.NullString{}, fmt.Errorf("seal qr svg: %w", err)
	}
	sealedURL, err := s.box.Seal(payloadURL)
	if err != nil {
		return nil, sql.NullString{}, fmt.Errorf("seal pairing url: %w", err)
	}
	return sealedSVG, sql.NullString{String: sealedURL, Valid: true}, nil
}

// openStored reverses sealCredential on a qr_svg column value: sealed
// rows are decrypted, legacy plaintext rows (written before sealing
// existed) pass through. secrets.ErrDecryptFailed / ErrUnavailable
// propagate so HTTP handlers can distinguish "wrong key" / "key unset"
// from a genuine storage error.
func (s *Service) openStored(stored []byte) ([]byte, error) {
	if !secrets.IsSealed(stored) {
		return stored, nil
	}
	return s.box.OpenBytes(stored) // nil box -> ErrUnavailable
}

func (s *Service) CreatePairing(ctx context.Context, friendlyLabel, actor string, userID int64, qrPayloadFor func(agentID, apiKey string) []byte) (*Pairing, *Key, error) {
	now := s.nowFn()
	agentID, err := mintAgentID()
	if err != nil {
		return nil, nil, fmt.Errorf("mint agent id: %w", err)
	}
	plaintext, err := mintAPIKey()
	if err != nil {
		return nil, nil, fmt.Errorf("mint api key: %w", err)
	}
	hash := s.hashKey(plaintext)
	payloadStr := string(qrPayloadFor(agentID, plaintext))
	svg, err := qr.RenderSVG(payloadStr, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("render qr: %w", err)
	}
	storedSVG, storedURL, err := s.sealCredential(svg, payloadStr)
	if err != nil {
		return nil, nil, err
	}

	var userIDArg sql.NullInt64
	if userID != 0 {
		userIDArg = sql.NullInt64{Int64: userID, Valid: true}
	}

	var (
		pRow   sqlcgen.DevicePairing
		keyRow sqlcgen.DevicePairingKey
	)
	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		created, err := q.CreateDevicePairing(ctx, sqlcgen.CreateDevicePairingParams{
			AgentID:       agentID,
			FriendlyLabel: friendlyLabel,
			CreatedAt:     now,
			CreatedBy:     actor,
			QrSvg:         storedSVG,
			UserID:        userIDArg,
			PairingUrl:    storedURL,
		})
		if err != nil {
			return fmt.Errorf("insert pairing: %w", err)
		}
		row, err := q.CreateDevicePairingKey(ctx, sqlcgen.CreateDevicePairingKeyParams{
			PairingID:     created.ID,
			KeyLookupHash: hash,
			KeyPreview:    plaintext[len(plaintext)-4:],
			CreatedAt:     now,
		})
		if err != nil {
			return fmt.Errorf("insert key: %w", err)
		}
		keyRow = row

		if err := q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: created.ID,
			Actor:     actor,
			Event:     audit.EventPairCreated,
			Details:   mustJSON(map[string]any{"agent_id": agentID, "friendly_label": friendlyLabel}),
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("audit PAIR_CREATED: %w", err)
		}
		if err := q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: created.ID,
			Actor:     actor,
			Event:     audit.EventKeyMinted,
			Details:   mustJSON(map[string]any{"key_id": row.ID, "key_preview": plaintext[len(plaintext)-4:]}),
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("audit KEY_MINTED: %w", err)
		}
		pRow = created
		return nil
	})
	if err != nil {
		return nil, nil, err
	}

	pairing := &Pairing{
		ID:            pRow.ID,
		AgentID:       pRow.AgentID,
		FriendlyLabel: pRow.FriendlyLabel,
		CreatedAt:     pRow.CreatedAt,
		CreatedBy:     pRow.CreatedBy,
		RevokedAt:     pRow.RevokedAt,
		UserID:        pRow.UserID,
	}
	key := &Key{
		ID:         keyRow.ID,
		PairingID:  keyRow.PairingID,
		Plaintext:  plaintext,
		LookupHash: keyRow.KeyLookupHash,
		Preview:    keyRow.KeyPreview,
		CreatedAt:  keyRow.CreatedAt,
		ExpiresAt:  keyRow.ExpiresAt,
		RevokedAt:  keyRow.RevokedAt,
		QRSVG:      svg,
		PayloadURL: payloadStr,
	}
	return pairing, key, nil
}

// KeyLookup resolves an X-API-Key header value to the agent_id of the
// device it authenticates AND the per-device HMAC signing key (issue
// #453 PR C; auth.SigningKeyForAPIKey), or (auth.KeyLookupResult{}, nil) when no active key
// matches. A non-nil error is a genuine DB failure and propagates as
// 500.
//
// Returns auth.KeyLookupResult (defined in the consumer package) to
// keep the import graph acyclic: internal/pairing imports
// internal/audit and internal/audit imports internal/auth, so
// internal/auth cannot import internal/pairing.
func (s *Service) KeyLookup(ctx context.Context, presented string) (auth.KeyLookupResult, error) {
	hash := s.hashKey(presented)
	row, err := s.db.Reader.GetDevicePairingKeyByHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return auth.KeyLookupResult{}, nil
	}
	if err != nil {
		return auth.KeyLookupResult{}, fmt.Errorf("lookup key: %w", err)
	}
	pairing, err := s.db.Reader.GetDevicePairingByID(ctx, row.PairingID)
	if err != nil {
		return auth.KeyLookupResult{}, fmt.Errorf("lookup pairing: %w", err)
	}
	if row.AwaitingAdoption != 0 {
		// First use of a rotation-minted key: the device has proved it
		// holds the new key, so its older keys stop working now rather
		// than at the end of the grace window. A failure here never fails
		// authentication -- the key is valid either way, and the flag is
		// still set, so the next request retries.
		if err := s.adoptKey(ctx, row, pairing.AgentID); err != nil {
			s.log.Warn("pairing: adopting rotated key failed; will retry on next request",
				"agentId", pairing.AgentID, "keyId", row.ID, "err", err)
		}
	}
	// The signing key is auth.SigningKeyForAPIKey -- the raw presented
	// key, the same key branchdam-agent and branchdam-mobile sign with
	// -- and deliberately NOT a pepper-derived value: a key only the
	// server can compute is one no client can sign with, which is how
	// agent.signedRequests=true once rejected every real device. The
	// pepper still protects the stored lookup hash (hashKey); the
	// signing key is never persisted.
	return auth.KeyLookupResult{
		AgentID:    pairing.AgentID,
		SigningKey: auth.SigningKeyForAPIKey(presented),
	}, nil
}

// RotateKey mints a new API key, sets expires_at on the pairing's
// currently-active keys, refreshes the cached QR SVG on the pairing
// row, and returns the new plaintext key.
func (s *Service) RotateKey(ctx context.Context, pairingID int64, actor string, graceMinutes int, qrPayloadFor func(agentID, apiKey string) []byte) (*Key, int64, error) {
	if graceMinutes <= 0 {
		graceMinutes = 24 * 60
	}
	now := s.nowFn()
	expiresAt := now + int64(graceMinutes)*60

	plaintext, err := mintAPIKey()
	if err != nil {
		return nil, 0, fmt.Errorf("mint api key: %w", err)
	}
	hash := s.hashKey(plaintext)
	// The device learns the new key from its next handshake (00043), so
	// the plaintext is kept sealed at rest until it is adopted -- only with
	// a box configured, like pairing_url; a keyless server stores nothing
	// and its devices re-pair by QR.
	var sealedForDelivery sql.NullString
	if s.box != nil {
		sealed, err := s.box.Seal(plaintext)
		if err != nil {
			return nil, 0, fmt.Errorf("seal rotated key: %w", err)
		}
		sealedForDelivery = sql.NullString{String: sealed, Valid: true}
	}

	var (
		keyRow        sqlcgen.DevicePairingKey
		agentID       string
		friendlyLabel string
	)
	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		existing, err := q.GetDevicePairingByID(ctx, pairingID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPairingNotFound
			}
			return fmt.Errorf("load pairing: %w", err)
		}
		agentID = existing.AgentID
		friendlyLabel = existing.FriendlyLabel

		if err := q.SetActiveKeyExpirations(ctx, sqlcgen.SetActiveKeyExpirationsParams{
			PairingID: pairingID,
			ExpiresAt: sql.NullInt64{Int64: expiresAt, Valid: true},
		}); err != nil {
			return fmt.Errorf("set grace on active keys: %w", err)
		}

		row, err := q.CreateDevicePairingKey(ctx, sqlcgen.CreateDevicePairingKeyParams{
			PairingID:     pairingID,
			KeyLookupHash: hash,
			KeyPreview:    plaintext[len(plaintext)-4:],
			CreatedAt:     now,
		})
		if err != nil {
			return fmt.Errorf("insert new key: %w", err)
		}
		keyRow = row
		if err := q.MarkKeyAwaitingAdoption(ctx, sqlcgen.MarkKeyAwaitingAdoptionParams{
			ID:                row.ID,
			RotationSealedKey: sealedForDelivery,
		}); err != nil {
			return fmt.Errorf("mark key awaiting adoption: %w", err)
		}
		if err := q.ClearSupersededRotationSecrets(ctx, sqlcgen.ClearSupersededRotationSecretsParams{
			PairingID: pairingID,
			ID:        row.ID,
		}); err != nil {
			return fmt.Errorf("clear superseded rotation secrets: %w", err)
		}

		if err := q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: pairingID,
			Actor:     actor,
			Event:     audit.EventKeyRotated,
			Details: mustJSON(map[string]any{
				"new_key_id":          row.ID,
				"new_key_preview":     plaintext[len(plaintext)-4:],
				"grace_minutes":       graceMinutes,
				"previous_expires_at": expiresAt,
			}),
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("audit KEY_ROTATED: %w", err)
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}

	// Render and persist the new QR AFTER the transaction commits. We
	// don't want to render before the key exists (a concurrent read of
	// qr_svg could pick up a stale SVG); and we don't want to render
	// inside the transaction (an external dep, slow on slow devices).
	// The SVG is derived purely from (agent_id, plaintext, server URL)
	// so a re-render race produces identical bytes -- no harm if the
	// pairing row already has the previous key's SVG.
	payloadStr := string(qrPayloadFor(agentID, plaintext))
	svg, err := qr.RenderSVG(payloadStr, 0)
	if err != nil {
		return nil, 0, fmt.Errorf("render qr: %w", err)
	}
	storedSVG, storedURL, err := s.sealCredential(svg, payloadStr)
	if err != nil {
		return nil, 0, err
	}
	if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.UpdateDevicePairingCredentials(ctx, sqlcgen.UpdateDevicePairingCredentialsParams{
			ID:         pairingID,
			QrSvg:      storedSVG,
			PairingUrl: storedURL,
		})
	}); err != nil {
		return nil, 0, fmt.Errorf("persist qr svg: %w", err)
	}

	// FriendlyLabel is currently unused here; the key's Plaintext is
	// the only output the handler surfaces, plus the rendered SVG.
	_ = friendlyLabel

	return &Key{
		ID:         keyRow.ID,
		PairingID:  keyRow.PairingID,
		Plaintext:  plaintext,
		LookupHash: keyRow.KeyLookupHash,
		Preview:    keyRow.KeyPreview,
		CreatedAt:  keyRow.CreatedAt,
		ExpiresAt:  keyRow.ExpiresAt,
		RevokedAt:  keyRow.RevokedAt,
		QRSVG:      svg,
		PayloadURL: payloadStr,
	}, expiresAt, nil

}

// adoptKey records that the device authenticated with rotation-minted key
// row for the first time: the key is adopted (its sealed delivery copy
// dropped) and every older still-active key of the pairing is revoked,
// with a KEY_ADOPTED audit row. Idempotent: a concurrent adoption makes
// AdoptRotatedKey match zero rows and this a no-op.
func (s *Service) adoptKey(ctx context.Context, row sqlcgen.DevicePairingKey, agentID string) error {
	now := s.nowFn()
	return s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.AdoptRotatedKey(ctx, sqlcgen.AdoptRotatedKeyParams{
			ID:        row.ID,
			AdoptedAt: sql.NullInt64{Int64: now, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("adopt key: %w", err)
		}
		if n == 0 {
			return nil
		}
		retired, err := q.RetireKeysOlderThan(ctx, sqlcgen.RetireKeysOlderThanParams{
			PairingID: row.PairingID,
			ID:        row.ID,
			RevokedAt: sql.NullInt64{Int64: now, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("retire older keys: %w", err)
		}
		return q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: row.PairingID,
			Actor:     agentID,
			Event:     audit.EventKeyAdopted,
			Details:   mustJSON(map[string]any{"key_id": row.ID, "key_preview": row.KeyPreview, "retired_keys": retired}),
			CreatedAt: now,
		})
	})
}

// RotationSealInfo is the HMAC-SHA256 message that derives a device's
// rotation transport key from its current API key (docs/agent-api.md).
const RotationSealInfo = "branchdam-key-rotation-v1"

// rotationBox returns the secrets.Box that seals a rotated key for the
// device holding currentKey: AES-256-GCM under
// HMAC-SHA256(key = currentKey, message = RotationSealInfo). Only a
// holder of currentKey can derive it, so the sealed value in a handshake
// response is useless to anything that merely sees the response.
func rotationBox(currentKey string) (*secrets.Box, error) {
	mac := hmac.New(sha256.New, []byte(currentKey))
	mac.Write([]byte(RotationSealInfo))
	return secrets.NewBox(base64.StdEncoding.EncodeToString(mac.Sum(nil)))
}

// SealForDevice seals newKey for the device that authenticates with
// currentKey. The result uses secrets.Box's format: "v1:" followed by the
// standard-base64 encoding of a 12-byte nonce, the ciphertext and the
// 16-byte GCM tag.
func SealForDevice(currentKey, newKey string) (string, error) {
	box, err := rotationBox(currentKey)
	if err != nil {
		return "", err
	}
	return box.Seal(newKey)
}

// OpenRotatedKey is the client half of SealForDevice, kept here as the
// reference implementation for clients and for tests.
func OpenRotatedKey(currentKey, sealed string) (string, error) {
	box, err := rotationBox(currentKey)
	if err != nil {
		return "", err
	}
	return box.Open(sealed)
}

// PendingRotation is what /agent/handshake reports about the requesting
// key. CurrentKeyID is always set; KeyID is zero when the device is
// already on its pairing's newest active key.
type PendingRotation struct {
	CurrentKeyID int64
	KeyID        int64
	// SealedAPIKey is the new key sealed for the device (SealForDevice),
	// or empty when it cannot be delivered: the server has no
	// BRANCHDAM_SECRET_KEY, or the rotation predates 00043. The device
	// then has to be re-paired by QR.
	SealedAPIKey string
	// PreviousKeyExpiresAt is when the requesting key stops working
	// unless the device adopts the new key first (0 = no expiry set).
	PreviousKeyExpiresAt int64
}

// PendingRotationFor resolves presented -- the X-API-Key the request
// authenticated with -- to its key row and reports whether a newer key
// is waiting for the device, sealed for it. Returns (nil, nil) when
// presented is not an active key of agentID's pairing. That is
// defensive only: in production the agent auth chain admits nothing
// but pairing keys (AgentChain.LookupKey is pairing.KeyLookup).
func (s *Service) PendingRotationFor(ctx context.Context, agentID, presented string) (*PendingRotation, error) {
	row, err := s.db.Reader.GetDevicePairingKeyByHash(ctx, s.hashKey(presented))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup key: %w", err)
	}
	pairing, err := s.db.Reader.GetDevicePairingByID(ctx, row.PairingID)
	if err != nil {
		return nil, fmt.Errorf("lookup pairing: %w", err)
	}
	if pairing.AgentID != agentID || pairing.RevokedAt.Valid {
		return nil, nil
	}
	out := &PendingRotation{CurrentKeyID: row.ID}
	newer, err := s.db.Reader.NewestActiveKeyForPairing(ctx, sqlcgen.NewestActiveKeyForPairingParams{
		PairingID: row.PairingID,
		ID:        row.ID,
	})
	if errors.Is(err, sql.ErrNoRows) {
		return out, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup newest key: %w", err)
	}
	out.KeyID = newer.ID
	if row.ExpiresAt.Valid {
		out.PreviousKeyExpiresAt = row.ExpiresAt.Int64
	}
	if !newer.RotationSealedKey.Valid || s.box == nil {
		return out, nil
	}
	plaintext, err := s.box.Open(newer.RotationSealedKey.String)
	if err != nil {
		// Wrong or changed BRANCHDAM_SECRET_KEY: report the rotation
		// without the key, so the device falls back to re-pairing. The
		// copy is kept: restoring the right secret key makes it
		// deliverable again, and adoption, a newer rotation or a
		// revoke clears it.
		s.log.Warn("pairing: cannot open sealed rotated key; device must re-pair",
			"agentId", agentID, "keyId", newer.ID, "err", err)
		return out, nil
	}
	if out.SealedAPIKey, err = SealForDevice(presented, plaintext); err != nil {
		return nil, fmt.Errorf("seal rotated key for device: %w", err)
	}
	return out, nil
}

// RevokePairing terminates pairingID: sets revoked_at on the pairing row
// AND on every (still-active) key for the pairing. Returns the timestamp
// used for revoked_at. Idempotent for an existing pairing; an unknown
// pairingID returns ErrPairingNotFound and writes nothing.
func (s *Service) RevokePairing(ctx context.Context, pairingID int64, actor string) (int64, error) {
	now := s.nowFn()
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		// Existence check first: the PAIR_REVOKED audit insert below
		// carries a RESTRICT FK to device_pairings, so an unknown id
		// used to surface as a FOREIGN KEY constraint failure (500)
		// instead of a 404.
		if _, err := q.GetDevicePairingByID(ctx, pairingID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPairingNotFound
			}
			return fmt.Errorf("load pairing: %w", err)
		}
		if err := q.RevokeAllKeysForPairing(ctx, sqlcgen.RevokeAllKeysForPairingParams{
			PairingID: pairingID,
			RevokedAt: sql.NullInt64{Int64: now, Valid: true},
		}); err != nil {
			return fmt.Errorf("revoke keys: %w", err)
		}
		if err := q.RevokeDevicePairing(ctx, sqlcgen.RevokeDevicePairingParams{
			ID:        pairingID,
			RevokedAt: sql.NullInt64{Int64: now, Valid: true},
		}); err != nil {
			return fmt.Errorf("revoke pairing: %w", err)
		}
		return q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: pairingID,
			Actor:     actor,
			Event:     audit.EventPairRevoked,
			Details:   "{}",
			CreatedAt: now,
		})
	})
	return now, err
}

// DeletePairing hard-deletes a revoked pairing and all its child rows
// (audit, keys). The pairing must already be revoked -- active pairings
// cannot be deleted. A trace event is written to actor_audit (the
// global cross-cutting audit table) before the pairing-scoped rows are
// removed, so the action remains auditable after deletion.
func (s *Service) DeletePairing(ctx context.Context, pairingID int64, actor string) error {
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		p, err := q.GetDevicePairingByID(ctx, pairingID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPairingNotFound
			}
			return fmt.Errorf("load pairing: %w", err)
		}
		if !p.RevokedAt.Valid {
			return ErrPairingNotRevoked
		}
		actorKind, actorName := parseActorString(actor)
		if err := q.InsertActorAudit(ctx, sqlcgen.InsertActorAuditParams{
			ActorUserID:  sql.NullInt64{},
			ActorKind:    actorKind,
			ActorName:    actorName,
			Event:        audit.EventPairingDeleted,
			ResourceType: "companion_pairing",
			ResourceID:   sql.NullString{String: fmt.Sprintf("%d", pairingID), Valid: true},
			DetailsJson:  "{}",
		}); err != nil {
			return fmt.Errorf("actor audit: %w", err)
		}
		if err := q.DeletePairingAuditForPairing(ctx, pairingID); err != nil {
			return fmt.Errorf("delete audit: %w", err)
		}
		if err := q.DeletePairingKeysForPairing(ctx, pairingID); err != nil {
			return fmt.Errorf("delete keys: %w", err)
		}
		return q.DeleteDevicePairing(ctx, pairingID)
	})
	return err
}

// parseActorString splits the actor string (e.g. "user:alice" or "system")
// into (actor_kind, actor_name) for the actor_audit table.
func parseActorString(actor string) (kind, name string) {
	if strings.HasPrefix(actor, "user:") {
		return "user", strings.TrimPrefix(actor, "user:")
	}
	if actor == "system" || actor == "" {
		return "system", actor
	}
	return "user", actor
}

// LatestActiveKey returns the device's newest key that isn't the one
// the caller used. Used by /agent/handshake to send a pendingRotation
// hint. Returns sql.ErrNoRows when the caller is already on the
// newest key OR the pairing is revoked.
func (s *Service) LatestActiveKey(ctx context.Context, agentID string, currentKeyID int64) (sqlcgen.DevicePairingKey, error) {
	pairing, err := s.db.Reader.GetDevicePairingByAgentID(ctx, agentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return sqlcgen.DevicePairingKey{}, sql.ErrNoRows
		}
		return sqlcgen.DevicePairingKey{}, fmt.Errorf("lookup pairing: %w", err)
	}
	if pairing.RevokedAt.Valid {
		return sqlcgen.DevicePairingKey{}, sql.ErrNoRows
	}
	return s.db.Reader.NewestActiveKeyForPairing(ctx, sqlcgen.NewestActiveKeyForPairingParams{
		PairingID: pairing.ID,
		ID:        currentKeyID,
	})
}

// ListKeys returns the device's full key history (audit trail). Plaintext
// is never populated -- only the preview. Used by the SPA's
// pairing-detail panel to render the rotate/revoke actions.
func (s *Service) ListKeys(ctx context.Context, pairingID int64) ([]sqlcgen.DevicePairingKey, error) {
	return s.db.Reader.ListKeysByPairing(ctx, pairingID)
}

// GetPairing loads a pairing by its primary key.
func (s *Service) GetPairing(ctx context.Context, id int64) (*Pairing, error) {
	row, err := s.db.Reader.GetDevicePairingByID(ctx, id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPairingNotFound
		}
		return nil, err
	}
	return &Pairing{
		ID:            row.ID,
		AgentID:       row.AgentID,
		FriendlyLabel: row.FriendlyLabel,
		CreatedAt:     row.CreatedAt,
		CreatedBy:     row.CreatedBy,
		RevokedAt:     row.RevokedAt,
	}, nil
}

// ListPairings returns paginated rows of all pairings (active + revoked).
// ActiveKeyCount surfaces in each row so the SPA can render "this
// device still has working credentials" without a per-pairing follow-up.
func (s *Service) ListPairings(ctx context.Context, limit, offset int64) ([]PairingRow, error) {
	rows, err := s.db.Reader.ListDevicePairings(ctx, sqlcgen.ListDevicePairingsParams{
		Limit:  limit,
		Offset: offset,
	})
	if err != nil {
		return nil, err
	}
	out := make([]PairingRow, len(rows))
	for i, r := range rows {
		out[i] = PairingRow{
			ID:             r.ID,
			AgentID:        r.AgentID,
			FriendlyLabel:  r.FriendlyLabel,
			CreatedAt:      r.CreatedAt,
			CreatedBy:      r.CreatedBy,
			RevokedAt:      r.RevokedAt,
			ActiveKeyCount: toInt64(r.ActiveKeyCount),
			NextExpiryUnix: r.NextExpiryUnix,
			LastKeyAt:      r.LastKeyAt,
		}
	}
	return out, nil
}

// CountPairings returns the total row count (used by the SPA's pagination).
func (s *Service) CountPairings(ctx context.Context) (int64, error) {
	return s.db.Reader.CountDevicePairings(ctx)
}

// ListAudit returns the pairing's audit log (paginated).
func (s *Service) ListAudit(ctx context.Context, pairingID, limit, offset int64) ([]sqlcgen.CompanionPairingAudit, error) {
	return s.db.Reader.ListPairingAudit(ctx, sqlcgen.ListPairingAuditParams{
		PairingID: pairingID,
		Limit:     limit,
		Offset:    offset,
	})
}

// CountPairingAudit returns the total audit count for a pairing.
func (s *Service) CountPairingAudit(ctx context.Context, pairingID int64) (int64, error) {
	return s.db.Reader.CountPairingAudit(ctx, pairingID)
}

// ActiveQRSVG returns the cached QR SVG for a pairing's current key,
// decrypted when the stored copy is sealed. Returns ErrNoActiveKey when
// the pairing exists but has no qr_svg cached (e.g. all keys revoked),
// and propagates secrets.ErrDecryptFailed / ErrUnavailable for a sealed
// row whose key is wrong or unset.
func (s *Service) ActiveQRSVG(ctx context.Context, pairingID int64) ([]byte, error) {
	creds, err := s.ActiveCredentials(ctx, pairingID)
	if err != nil {
		return nil, err
	}
	return creds.QRSVG, nil
}

// ActiveCredentials returns the re-displayable material for a pairing's
// current key: decrypted QR SVG plus the pairing URL when one was stored
// sealed (empty string when the column is NULL -- keyless server or
// legacy row awaiting rotation). Same error contract as the old
// ActiveQRSVG body: ErrPairingNotFound, ErrNoActiveKey for revoked /
// uncached rows, secrets errors for undecryptable material.
func (s *Service) ActiveCredentials(ctx context.Context, pairingID int64) (*Credentials, error) {
	row, err := s.db.Reader.GetDevicePairingByID(ctx, pairingID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrPairingNotFound
		}
		return nil, err
	}
	if row.RevokedAt.Valid {
		return nil, ErrNoActiveKey
	}
	if len(row.QrSvg) == 0 {
		return nil, ErrNoActiveKey
	}
	svg, err := s.openStored(row.QrSvg)
	if err != nil {
		return nil, err
	}
	creds := &Credentials{
		AgentID:        row.AgentID,
		FriendlyLabel:  row.FriendlyLabel,
		QRSVG:          svg,
		SealingEnabled: s.box != nil,
	}
	// Always surface the newest active key's last-4 from the DB so an
	// empty PairingURL (NULL column: keyless or pre-00034 legacy) still
	// yields a stable masked preview instead of Unavailable (Hermes r3).
	if preview, err := s.db.Reader.ActiveKeyPreviewForPairing(ctx, pairingID); err == nil {
		creds.KeyPreview = preview
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if row.PairingUrl.Valid && row.PairingUrl.String != "" {
		// pairing_url is only ever written sealed (migration 00034), so
		// a non-NULL value always carries the "v1:" prefix here.
		url, err := s.box.Open(row.PairingUrl.String) // nil box -> ErrUnavailable
		if err != nil {
			return nil, err
		}
		creds.PairingURL = url
	}
	return creds, nil
}

// RenamePairing updates a pairing's friendly_label and writes a
// LABEL_RENAMED audit row carrying old and new. Labels are deliberately
// not unique (migration 00017), so no collision check applies. Allowed
// on revoked pairings too -- renaming history is harmless and useful
// for cleanup.
func (s *Service) RenamePairing(ctx context.Context, pairingID int64, label, actor string) (*Pairing, error) {
	label = strings.TrimSpace(label)
	if label == "" {
		return nil, ErrEmptyLabel
	}
	now := s.nowFn()
	var out *Pairing
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		existing, err := q.GetDevicePairingByID(ctx, pairingID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return ErrPairingNotFound
			}
			return fmt.Errorf("load pairing: %w", err)
		}
		if err := q.UpdateDevicePairingFriendlyLabel(ctx, sqlcgen.UpdateDevicePairingFriendlyLabelParams{
			ID:            pairingID,
			FriendlyLabel: label,
		}); err != nil {
			return fmt.Errorf("update label: %w", err)
		}
		if err := q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: pairingID,
			Actor:     actor,
			Event:     audit.EventLabelRenamed,
			Details: mustJSON(map[string]any{
				"old": existing.FriendlyLabel,
				"new": label,
			}),
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("audit LABEL_RENAMED: %w", err)
		}
		out = &Pairing{
			ID:            existing.ID,
			AgentID:       existing.AgentID,
			FriendlyLabel: label,
			CreatedAt:     existing.CreatedAt,
			CreatedBy:     existing.CreatedBy,
			RevokedAt:     existing.RevokedAt,
			UserID:        existing.UserID,
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// RecordCredentialReveal writes a CREDENTIALS_REVEALED audit row for a
// successful read of re-displayable credential material (the credentials
// endpoint or GET qr.svg). Both endpoints reveal the same secret, so
// both are audited with a channel discriminator in details. Callers
// invoke this AFTER the read succeeds and BEFORE serving -- an audit
// failure fails the request rather than leaking unaudited.
func (s *Service) RecordCredentialReveal(ctx context.Context, pairingID int64, actor, channel string) error {
	now := s.nowFn()
	return s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: pairingID,
			Actor:     actor,
			Event:     audit.EventCredentialsRevealed,
			Details:   mustJSON(map[string]any{"channel": channel}),
			CreatedAt: now,
		})
	})
}

// BackfillSealedCredentials seals any plaintext qr_svg rows left over
// from before sealing existed (SQL migrations cannot run the crypto).
// pairing_url is not backfillable -- the plaintext key for a legacy row
// isn't recoverable without decoding the QR -- so those stay NULL until
// the operator rotates, which heals them. No-op when no box is
// configured (keyless server never seals). Idempotent: sealed rows are
// skipped. Returns the number of rows sealed.
func (s *Service) BackfillSealedCredentials(ctx context.Context) (int, error) {
	if s.box == nil {
		return 0, nil
	}
	rows, err := s.db.Reader.ListPairingSecrets(ctx)
	if err != nil {
		return 0, fmt.Errorf("list pairing secrets: %w", err)
	}
	sealed := 0
	for _, r := range rows {
		if secrets.IsSealed(r.QrSvg) {
			continue
		}
		open, err := s.box.SealBytes(r.QrSvg)
		if err != nil {
			return sealed, fmt.Errorf("seal pairing %d qr svg: %w", r.ID, err)
		}
		if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
			return q.UpdateDevicePairingCredentials(ctx, sqlcgen.UpdateDevicePairingCredentialsParams{
				ID:         r.ID,
				QrSvg:      open,
				PairingUrl: r.PairingUrl,
			})
		}); err != nil {
			return sealed, fmt.Errorf("persist sealed pairing %d: %w", r.ID, err)
		}
		sealed++
	}
	if sealed > 0 {
		s.log.Info("sealed legacy plaintext pairing QR credentials", "count", sealed)
	}
	return sealed, nil
}

// IsAgentRevoked reports whether the pairing for agentID is revoked.
// Used by the handshake's pendingRotation hint to short-circuit.
func (s *Service) IsAgentRevoked(ctx context.Context, agentID string) (bool, error) {
	row, err := s.db.Reader.GetDevicePairingByAgentID(ctx, agentID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return true, nil
		}
		return false, err
	}
	return row.RevokedAt.Valid, nil
}

// hashKey returns the hex-encoded HMAC-SHA256 of the API key using the
// service's pepper. The pepper is a 32-byte secret derived from
// BRANCHDAM_SECRET_KEY. HMAC-SHA256 satisfies CodeQL's
// go/weak-cryptographic-algorithm while keeping the lookup as a single
// indexed SELECT — the key is already 256-bit random, so no password
// stretching (bcrypt/argon2) is needed.
func (s *Service) hashKey(plaintext string) string {
	mac := hmac.New(sha256.New, s.pepper)
	mac.Write([]byte(plaintext))
	return hex.EncodeToString(mac.Sum(nil))
}

// toInt64 coerces the sqlite integer (which sqlc surfaces as
// interface{} for COUNT(*)) to int64. Returns 0 on type mismatch --
// only COUNT(*) results hit this path today, so a non-int64 is a bug
// rather than a runtime condition.
func toInt64(v any) int64 {
	switch x := v.(type) {
	case int64:
		return x
	case int:
		return int64(x)
	default:
		return 0
	}
}

// mintAPIKey returns a 256-bit cryptographically random URL-safe string.
func mintAPIKey() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return strings.TrimRight(base64.URLEncoding.EncodeToString(buf), "="), nil
}

// mintAgentID returns "dev-<8 hex>" -- server-minted unique identifier.
func mintAgentID() (string, error) {
	buf := make([]byte, 4)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return fmt.Sprintf("dev-%s", hex.EncodeToString(buf)), nil
}

// mustJSON marshals v or panics -- the audit detail payload is always a
// literal JSON object built in this file, never user input, so a marshal
// failure would be a coding bug, not a runtime condition.
func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(fmt.Sprintf("pairing: mustJSON: %v", err))
	}
	return string(b)
}
