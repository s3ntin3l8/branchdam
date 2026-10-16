-- +goose Up
-- Delivers a rotated device key to the device itself (S13). Before this,
-- POST /api/v1/agent/handshake could only tell a device that a newer key
-- existed (pendingRotation.keyId): the plaintext is never stored, so the
-- device had to be re-paired by QR before its old key's grace window ran
-- out.
--
-- rotation_sealed_key holds a rotation-minted key's plaintext, sealed with
-- secrets.Box under BRANCHDAM_SECRET_KEY -- the same at-rest sealing as
-- device_pairings.pairing_url (00034). It is written only when a box is
-- configured (NULL otherwise: a keyless server never stores key material,
-- and its devices still re-pair by QR), and cleared as soon as the key is
-- adopted, superseded by a later rotation, or revoked with its pairing.
-- The handshake re-seals it for transport under a key derived from the
-- requesting device's current API key (docs/agent-api.md).
--
-- awaiting_adoption marks a rotation-minted key the device has not used
-- yet. The first request authenticated with it adopts it: adopted_at is
-- stamped, the flag and the sealed copy are cleared, and every older
-- still-active key of the pairing is revoked -- the old key stops working
-- as soon as the device has proved it holds the new one, instead of at
-- the end of the grace window. Keys are updated, never deleted.
ALTER TABLE device_pairing_keys ADD COLUMN rotation_sealed_key TEXT;
ALTER TABLE device_pairing_keys ADD COLUMN awaiting_adoption INTEGER NOT NULL DEFAULT 0
    CHECK (awaiting_adoption IN (0, 1));
ALTER TABLE device_pairing_keys ADD COLUMN adopted_at INTEGER;

-- +goose Down
ALTER TABLE device_pairing_keys DROP COLUMN adopted_at;
ALTER TABLE device_pairing_keys DROP COLUMN awaiting_adoption;
ALTER TABLE device_pairing_keys DROP COLUMN rotation_sealed_key;
