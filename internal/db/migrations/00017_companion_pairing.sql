-- +goose Up
-- Companion pairing: per-device API keys for /api/v1/agent/*.
--
-- Three tables, all FK RESTRICT and no triggers (same invariants as every
-- other table here, see docs/schema.md fixes #4/#5/#6 and AGENTS.md
-- invariant #1). All timestamps are Unix seconds to match the rest of
-- the schema (created_at/updated_at columns elsewhere).
--
-- device_pairings holds one row per logical device. The agent_id is the
-- server-minted identifier embedded in the QR payload; uniqueness is
-- enforced at the DB level (UNIQUE constraint). friendly_label is the
-- operator-supplied human-readable name shown in the UI -- two pairings
-- are allowed to share the same label (uniqueness only on agent_id).
-- revoked_at is NULL while the pairing is active; setting it terminates
-- every key for the pairing in one shot (the HTTP layer enforces this).
--
-- device_pairing_keys holds N rows per pairing (multi-key per device, so
-- rotation can have an overlap window). The plaintext key is never
-- stored -- only sha256(key), hex-encoded, so a DB leak doesn't
-- compromise active credentials. key_preview is the last 4 chars of the
-- plaintext, shown in the UI for human identification only. expires_at
-- and revoked_at are both NULL while a key is active; the partial index
-- covers the active-set hot path.
--
-- companion_pairing_audit is an append-only event log of PAIR_CREATED /
-- KEY_MINTED / KEY_ROTATED / PAIR_REVOKED events, keyed by pairing_id
-- with a created_at-desc index for tail reads. Separate from
-- audit_queue (which is edge review, a different domain) per the
-- pairing design.
CREATE TABLE device_pairings (
    id              INTEGER PRIMARY KEY,
    agent_id        TEXT    NOT NULL UNIQUE,
    friendly_label  TEXT    NOT NULL,
    created_at      INTEGER NOT NULL,
    created_by      TEXT    NOT NULL,                -- "user:<auth principal name>"
    revoked_at      INTEGER,                         -- NULL = active
    qr_svg          BLOB,                            -- server-rendered SVG for the current active key
    CHECK (revoked_at IS NULL OR revoked_at >= created_at)
);

CREATE TABLE device_pairing_keys (
    id              INTEGER PRIMARY KEY,
    pairing_id      INTEGER NOT NULL REFERENCES device_pairings(id) ON DELETE RESTRICT,
    key_lookup_hash TEXT    NOT NULL UNIQUE,
    key_preview     TEXT    NOT NULL,
    created_at      INTEGER NOT NULL,
    expires_at      INTEGER,
    revoked_at      INTEGER,
    CHECK (length(key_lookup_hash) = 64),
    CHECK ((revoked_at IS NULL) OR (expires_at IS NULL) OR (revoked_at <= expires_at))
);

CREATE INDEX device_pairing_keys_pairing_active_idx
    ON device_pairing_keys(pairing_id)
    WHERE revoked_at IS NULL AND expires_at IS NULL;

CREATE TABLE companion_pairing_audit (
    id          INTEGER PRIMARY KEY,
    pairing_id  INTEGER NOT NULL REFERENCES device_pairings(id) ON DELETE RESTRICT,
    actor       TEXT    NOT NULL,
    event       TEXT    NOT NULL,
    details     TEXT    NOT NULL DEFAULT '{}',
    created_at  INTEGER NOT NULL
);

CREATE INDEX companion_pairing_audit_pairing_idx
    ON companion_pairing_audit(pairing_id, created_at DESC);

-- +goose Down
DROP INDEX IF EXISTS companion_pairing_audit_pairing_idx;
DROP TABLE IF EXISTS companion_pairing_audit;
DROP INDEX IF EXISTS device_pairing_keys_pairing_active_idx;
DROP TABLE IF EXISTS device_pairing_keys;
DROP TABLE IF EXISTS device_pairings;
