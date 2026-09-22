-- +goose Up
-- Sealed pairing URL: lets an operator re-open the full credentials
-- dialogue (QR + agent ID + API key + pairing URL + local-agent deep
-- link) for an existing pairing without minting a new key.
--
-- pairing_url stores the branchdam://?server=...&key=...&agent=...
-- payload for the pairing's current active key -- which means it embeds
-- the plaintext API key. Invariant: this column NEVER holds plaintext
-- key material. When BRANCHDAM_SECRET_KEY is configured, writes go
-- through secrets.Box (AES-256-GCM, "v1:" + base64 prefix) before they
-- reach this column. When the key is unset (dev/test), the column is
-- simply left NULL -- "not stored", never "empty". The same seal-or-NULL
-- rule applies to device_pairings.qr_svg (which already encodes the key
-- in QR modules); both columns are encrypted at rest by pairing.Service
-- rather than by this migration (SQL cannot run the crypto).
--
-- Existing rows keep pairing_url NULL until the operator rotates the
-- key for that pairing -- rotation recomputes the payload anyway and
-- heals the row. The plaintext key for a legacy row is not recoverable
-- without decoding the QR SVG, which is deliberately not done here.
ALTER TABLE device_pairings ADD COLUMN pairing_url TEXT;

-- +goose Down
ALTER TABLE device_pairings DROP COLUMN pairing_url;
