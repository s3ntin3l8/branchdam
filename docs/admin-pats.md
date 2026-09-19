# Admin Personal Access Tokens (PATs)

Admin PATs are long-lived Bearer tokens an operator (or operator tooling
such as Ansible) can use to call the branchDAM admin API without a
browser session. They are the mechanism behind the issue #453 plan's
"unattended provisioning" story: pair workstations, rotate keys, and
drive the API from configuration management.

## Token format

```
bdam_pat_<43 base64-url characters>
```

The prefix makes tokens grep-able in logs and rotation tooling. The 43
characters encode 32 random bytes (256 bits).

Only the HMAC-SHA256 of the token is stored
(`user_pats.hashed_key`, hex, UNIQUE), keyed with the same pepper
pairing uses for device keys. A database-only compromise cannot
recover any token without the pepper. The plaintext is shown to the
operator exactly once — at mint time — and never re-served.

## Bootstrap mechanism

The kubeadm-init pattern: an env var mints a one-shot wildcard admin
PAT on boot, writes it to a file, and refuses to ever mint again while
the file exists.

```yaml
# config.yaml
admin:
  bootstrapPAT: ${ADMIN_BOOTSTRAP_PAT}   # any non-empty value enables it
```

or directly:

```sh
ADMIN_BOOTSTRAP_PAT=1 branchdam
```

Semantics:

- On boot, if the env var is non-empty **and**
  `<dataDir>/bootstrap-pat.txt` does not exist, the server creates a
  dedicated bootstrap user (promoted to admin), mints a PAT with
  scopes `["*"]`, and writes the plaintext to
  `<dataDir>/bootstrap-pat.txt` (mode 0600).
- If the file already exists, the boot logs a skip and continues —
  the env var is consumed once. Delete the file (and the
  `user_pats` row, if you also want the token dead) to re-bootstrap.
- The bootstrap PAT satisfies every scope-gated route.

For Ansible, the playbook reads the file once, distributes the token
to the target's agent config, and the file stays on the server as the
consume-once sentinel.

## API endpoints

All endpoints require an admin identity (session cookie, forward-auth,
or a PAT). A PAT-authenticated request carries its owner's `users.id`,
so the endpoints work identically regardless of which identity path
authenticated the request.

| Method | Path | Description |
|---|---|---|
| `POST` | `/api/v1/users/me/pats` | Mint. Body: `{"name": "...", "scopes": ["..."], "expiresAt": 0}`. Returns the **plaintext once** plus metadata. |
| `GET` | `/api/v1/users/me/pats` | List (limit/offset query params). Never includes plaintext — only the first 8 hex chars of the stored hash (`hashedKeyPrefix`). |
| `POST` | `/api/v1/users/me/pats/{id}/revoke` | Soft-delete (sets `revoked_at`). Revoked tokens fail closed with 401. |

Mint and revoke are written to `actor_audit` (`pat.minted` /
`pat.revoked`) with the hash prefix, never the plaintext or full hash.

## Scope model

A token's `scopes` is a JSON array of strings. The wildcard `"*"`
satisfies every scope; otherwise the requested scope must appear as an
exact match. An empty scope list satisfies nothing (fail-closed
default). Route-level granularity comes from the middleware `Scope`;
the `is_admin` flag on a PAT principal is always true in this PR
(PATs are admin-only), so `RequireAdmin` passes for any authenticated
PAT even when `authz.groups` is non-empty.

`last_used_at` is bumped asynchronously, throttled to one write per
60 seconds per token, so a request flood doesn't become a write flood.

## Request routing

`Authorization: Bearer bdam_pat_...` on any non-agent path is
authenticated by the PAT middleware and bypasses the forward-auth /
session-cookie identity extraction — a request can't carry both a PAT
and a session, and the PAT path never consults `X-Authentik-*`
headers. Requests without a PAT header, and **all** requests under
`/api/v1/agent/*`, continue through the existing chains unchanged: a
PAT header on an agent route never short-circuits the agent key
validation.
