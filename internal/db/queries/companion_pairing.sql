-- Companion pairing queries. The handlers in internal/httpapi/companion_pairings.go
-- and the KeyLookup callback in internal/pairing/service.go both go through
-- these -- no raw SQL outside this file (per the project's sqlc convention,
-- documented in CONTRIBUTING.md).
--
-- All positional params use bare ?1/?2/?3 (not sqlc.arg(name)) per AGENTS.md's
-- "SQL Syntax Traps" note.

-- name: CreateDevicePairing :one
-- Inserts the pairing row and returns it. The HTTP layer wraps this with
-- the matching KEY_MINTED audit insert in the same tx (see pairing.Service).
INSERT INTO device_pairings (
    agent_id, friendly_label, created_at, created_by, qr_svg
) VALUES (
    ?1, ?2, ?3, ?4, ?5
)
RETURNING id, agent_id, friendly_label, created_at, created_by, revoked_at, qr_svg;

-- name: GetDevicePairingByID :one
SELECT id, agent_id, friendly_label, created_at, created_by, revoked_at, qr_svg
FROM device_pairings
WHERE id = ?1;

-- name: GetDevicePairingByAgentID :one
-- Used by the handshake's pendingRotation hint to load the pairing by
-- the agent_id attached to the request's Principal.
SELECT id, agent_id, friendly_label, created_at, created_by, revoked_at, qr_svg
FROM device_pairings
WHERE agent_id = ?1;

-- name: ListDevicePairings :many
-- One row per pairing with a count of currently-active keys (NULL -> 0)
-- and the earliest unexpired-but-soon-to-expire key (NULL if all keys
-- are permanent or none). Used by the SPA's list view to render the
-- grace-window countdown for in-progress rotations.
SELECT
    p.id, p.agent_id, p.friendly_label, p.created_at, p.created_by, p.revoked_at,
    COALESCE((
        SELECT COUNT(*) FROM device_pairing_keys k
        WHERE k.pairing_id = p.id
          AND k.revoked_at IS NULL
          AND (k.expires_at IS NULL OR k.expires_at > unixepoch())
    ), 0) AS active_key_count,
    (
        SELECT MIN(k.expires_at) FROM device_pairing_keys k
        WHERE k.pairing_id = p.id
          AND k.revoked_at IS NULL
          AND k.expires_at IS NOT NULL
          AND k.expires_at > unixepoch()
    ) AS next_expiry_unix,
    (
        SELECT MAX(k.created_at) FROM device_pairing_keys k
        WHERE k.pairing_id = p.id
    ) AS last_key_at
FROM device_pairings p
ORDER BY p.created_at DESC, p.id DESC
LIMIT ?1 OFFSET ?2;

-- name: CountDevicePairings :one
SELECT COUNT(*) FROM device_pairings;

-- name: RevokeDevicePairing :exec
-- Sets revoked_at on the pairing (does NOT touch keys -- the HTTP layer
-- also revokes every key for the pairing in the same tx).
UPDATE device_pairings
SET revoked_at = ?2
WHERE id = ?1
  AND revoked_at IS NULL;

-- name: CreateDevicePairingKey :one
INSERT INTO device_pairing_keys (
    pairing_id, key_lookup_hash, key_preview, created_at
) VALUES (
    ?1, ?2, ?3, ?4
)
RETURNING id, pairing_id, key_lookup_hash, key_preview, created_at,
          expires_at, revoked_at;

-- name: GetDevicePairingKeyByHash :one
-- The hot path: KeyLookup runs this on every authenticated agent request.
-- UNIQUE index on key_lookup_hash keeps it O(log n). Active means
-- revoked_at IS NULL AND (expires_at IS NULL OR expires_at > now).
SELECT k.id, k.pairing_id, k.key_lookup_hash, k.key_preview, k.created_at,
       k.expires_at, k.revoked_at
FROM device_pairing_keys k
JOIN device_pairings p ON p.id = k.pairing_id
WHERE k.key_lookup_hash = ?1
  AND p.revoked_at IS NULL
  AND k.revoked_at IS NULL
  AND (k.expires_at IS NULL OR k.expires_at > unixepoch())
LIMIT 1;

-- name: GetDevicePairingKeyByID :one
SELECT id, pairing_id, key_lookup_hash, key_preview, created_at,
       expires_at, revoked_at
FROM device_pairing_keys
WHERE id = ?1;

-- name: ListKeysByPairing :many
SELECT id, pairing_id, key_lookup_hash, key_preview, created_at,
       expires_at, revoked_at
FROM device_pairing_keys
WHERE pairing_id = ?1
ORDER BY created_at DESC, id DESC;

-- name: NewestActiveKeyForPairing :one
-- Used by /agent/handshake to find the device's newest unexpired key that
-- the caller hasn't been told about yet. ?2 is the caller's current
-- key_id; we filter strictly newer (created_at, id) so the caller is
-- never told about an older key they already missed or were using before.
-- Returns no rows (sql.ErrNoRows) when the caller is already on the
-- newest active key.
SELECT k.id, k.pairing_id, k.key_lookup_hash, k.key_preview, k.created_at,
       k.expires_at, k.revoked_at
FROM device_pairing_keys k
WHERE k.pairing_id = ?1
  AND k.revoked_at IS NULL
  AND (k.expires_at IS NULL OR k.expires_at > unixepoch())
  AND k.id <> ?2
  AND (
    k.created_at > (SELECT created_at FROM device_pairing_keys WHERE id = ?2)
    OR (
      k.created_at = (SELECT created_at FROM device_pairing_keys WHERE id = ?2)
      AND k.id > ?2
    )
  )
ORDER BY k.created_at DESC, k.id DESC
LIMIT 1;

-- name: SetActiveKeyExpirations :exec
-- Rotation: set expires_at on every currently-active key for this pairing
-- that doesn't already have one. Idempotent — re-running after the same
-- clock has no effect.
UPDATE device_pairing_keys
SET expires_at = ?2
WHERE pairing_id = ?1
  AND revoked_at IS NULL
  AND expires_at IS NULL;

-- name: RevokeAllKeysForPairing :exec
UPDATE device_pairing_keys
SET revoked_at = ?2
WHERE pairing_id = ?1
  AND revoked_at IS NULL;

-- name: InsertPairingAudit :exec
INSERT INTO companion_pairing_audit (
    pairing_id, actor, event, details, created_at
) VALUES (
    ?1, ?2, ?3, ?4, ?5
);

-- name: ListPairingAudit :many
-- Offset pagination. Audit log for a single pairing is bounded (a few
-- hundred events over the lifetime of a device, never tens of thousands),
-- so offset drift isn't a concern the way it is for the unbounded
-- audit_queue edge-review list.
SELECT id, pairing_id, actor, event, details, created_at
FROM companion_pairing_audit
WHERE pairing_id = ?1
ORDER BY created_at DESC, id DESC
LIMIT ?2 OFFSET ?3;

-- name: UpdateDevicePairingQRSVG :exec
-- Refresh the cached QR SVG after a key rotation. The SVG is computed
-- outside the transaction (in pairing.Service) so this UPDATE is a
-- pure byte-write with no rendering dependency.
UPDATE device_pairings
SET qr_svg = ?2
WHERE id = ?1;

-- name: GetDevicePairingQRSVG :one
-- Returns just the qr_svg column for the ActiveQRSVG hot path. Skips
-- the row-wide scan if all the caller wants is the bytes.
SELECT qr_svg
FROM device_pairings
WHERE id = ?1;

-- name: CountPairingAudit :one
SELECT COUNT(*) FROM companion_pairing_audit WHERE pairing_id = ?1;
