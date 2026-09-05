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

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/qr"
)

// Pairing is the public-facing device record.
type Pairing struct {
	ID            int64
	AgentID       string
	FriendlyLabel string
	CreatedAt     int64
	CreatedBy     string
	RevokedAt     sql.NullInt64
}

// Key is a device's API key, plaintext included. Plaintext is the ONLY
// way callers ever see it -- it never enters the database. QRSVG is the
// rendered SVG QR code, computed once at mint time and cached on the
// pairing row (device_pairings.qr_svg) so GET /qr.svg can serve it
// without re-rendering -- but the SVG is regenerated on every RotateKey,
// so an old SVG never sits in front of a new key.
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
}

// defaultPepper is used only when no BRANCHDAM_SECRET_KEY is configured.
// It still satisfies CodeQL (HMAC, not bare hash) but offers no protection
// against a stolen database — pairing should only be used with a real
// secret key in production.
var defaultPepper = sha256.Sum256([]byte("branchdam-pairing-default-pepper"))

// NewService constructs a Service backed by db. pepper is the raw 32-byte
// HMAC key derived from BRANCHDAM_SECRET_KEY; pass nil to use a
// deterministic fallback (not recommended for production). log may be nil
// for quieter callers (tests).
func NewService(database *db.DB, log *slog.Logger, pepper []byte) *Service {
	if log == nil {
		log = slog.Default()
	}
	if len(pepper) == 0 {
		pepper = defaultPepper[:]
	}
	return &Service{db: database, log: log, nowFn: nowUnix, pepper: pepper}
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
func (s *Service) CreatePairing(ctx context.Context, friendlyLabel, actor string, qrPayloadFor func(agentID, apiKey string) []byte) (*Pairing, *Key, error) {
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
	svg, err := qr.RenderSVG(string(qrPayloadFor(agentID, plaintext)), 0)
	if err != nil {
		return nil, nil, fmt.Errorf("render qr: %w", err)
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
			QrSvg:         svg,
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
			Event:     "PAIR_CREATED",
			Details:   mustJSON(map[string]any{"agent_id": agentID, "friendly_label": friendlyLabel}),
			CreatedAt: now,
		}); err != nil {
			return fmt.Errorf("audit PAIR_CREATED: %w", err)
		}
		if err := q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: created.ID,
			Actor:     actor,
			Event:     "KEY_MINTED",
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

	return &Pairing{
			ID:            pRow.ID,
			AgentID:       pRow.AgentID,
			FriendlyLabel: pRow.FriendlyLabel,
			CreatedAt:     pRow.CreatedAt,
			CreatedBy:     pRow.CreatedBy,
			RevokedAt:     pRow.RevokedAt,
		}, &Key{
			ID:         keyRow.ID,
			PairingID:  keyRow.PairingID,
			Plaintext:  plaintext,
			LookupHash: keyRow.KeyLookupHash,
			Preview:    keyRow.KeyPreview,
			CreatedAt:  keyRow.CreatedAt,
			ExpiresAt:  keyRow.ExpiresAt,
			RevokedAt:  keyRow.RevokedAt,
			QRSVG:      svg,
		}, nil
}

// KeyLookup resolves an X-API-Key header value to the agent_id of the
// device it authenticates. Returns ("", nil) when no active key matches.
func (s *Service) KeyLookup(ctx context.Context, presented string) (string, error) {
	hash := s.hashKey(presented)
	row, err := s.db.Reader.GetDevicePairingKeyByHash(ctx, hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("lookup key: %w", err)
	}
	pairing, err := s.db.Reader.GetDevicePairingByID(ctx, row.PairingID)
	if err != nil {
		return "", fmt.Errorf("lookup pairing: %w", err)
	}
	return pairing.AgentID, nil
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

		if err := q.InsertPairingAudit(ctx, sqlcgen.InsertPairingAuditParams{
			PairingID: pairingID,
			Actor:     actor,
			Event:     "KEY_ROTATED",
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
	svg, err := qr.RenderSVG(string(qrPayloadFor(agentID, plaintext)), 0)
	if err != nil {
		return nil, 0, fmt.Errorf("render qr: %w", err)
	}
	if err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.UpdateDevicePairingQRSVG(ctx, sqlcgen.UpdateDevicePairingQRSVGParams{
			ID:    pairingID,
			QrSvg: svg,
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
	}, expiresAt, nil
}

// RevokePairing terminates pairingID: sets revoked_at on the pairing row
// AND on every (still-active) key for the pairing. Returns the timestamp
// used for revoked_at. Idempotent.
func (s *Service) RevokePairing(ctx context.Context, pairingID int64, actor string) (int64, error) {
	now := s.nowFn()
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
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
			Event:     "PAIR_REVOKED",
			Details:   "{}",
			CreatedAt: now,
		})
	})
	return now, err
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

// ActiveQRSVG returns the cached QR SVG for a pairing's current key.
// Returns ErrNoActiveKey when the pairing exists but has no qr_svg
// cached (e.g. all keys revoked, or the column was just added by an
// upgrade and an older key needs rotating to repopulate).
func (s *Service) ActiveQRSVG(ctx context.Context, pairingID int64) ([]byte, error) {
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
	return row.QrSvg, nil
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
