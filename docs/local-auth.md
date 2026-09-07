# Local Auth (password login)

branchDAM has two authentication surfaces: a **forward-auth** path
(Authentik + Traefik ForwardAuth, covered in [`docs/forward-auth.md`](forward-auth.md))
and a **local-auth** path (password-based, session cookie, optionally
with forward-auth running side by side). This page is the operator-facing
guide to the local-auth surface. The code-level contracts live in
[`internal/auth/users`](../internal/auth/users), [`internal/auth/session`](../internal/auth/session),
and [`internal/auth/ratelimit`](../internal/auth/ratelimit); the SPA
surface is in [`web/src/pages/LoginPage.tsx`](../web/src/pages/LoginPage.tsx).

This page mirrors the structure of `forward-auth.md` (auth.mode semantics,
env contract, bring-up, runtime contract, security knobs, troubleshooting
at the end) so an operator reading one can read the other.

## 1. `auth.mode`: which chain runs

The `auth.mode` config field (in `config.yaml`, under the top-level
`auth:` key) selects the chain. The default is **`forward`**, byte-identical
to branchDAM's pre-local-auth behavior — an operator who never touches
`auth.mode` keeps the existing ForwardAuth path with no other change.

| Value     | Behavior                                                                 |
|-----------|--------------------------------------------------------------------------|
| `forward` | (default) Authentik ForwardAuth via the Traefik layer only               |
| `local`   | Password-based session-cookie login only. The `X-Authentik-*` chain is off |
| `both`    | Either path authenticates. Groups from both union; local wins on Name/Email collision |

The `both` mode is the migration path: an operator can run forward-auth
for their existing users AND let a second human create a local password
account, without flipping the entire deployment. The collision rule
(local wins on Name/Email) is deliberate — the interactive login is
the source of truth for who someone is once they have a local account.

Empty / unset `auth.mode` is treated as `forward` (the default), so
existing configs upgrade without modification.

## 2. Required env: `BRANCHDAM_SECRET_KEY`

When `auth.mode` is `local` or `both`, the server **refuses to boot**
unless `BRANCHDAM_SECRET_KEY` is set and is valid base64-decoded 32 bytes.
This is a hard fail at startup, with a log line that names the env var
verbatim:

```
auth: auth.mode is local or both, but BRANCHDAM_SECRET_KEY is missing or invalid -- refusing to boot.
Set a 32-byte base64 key in BRANCHDAM_SECRET_KEY before enabling local auth.
```

The 32-byte key is the HMAC-SHA-256 secret that signs the session cookie.
A missing or guessable secret would mean any attacker who knows the
cookie format (which is in the repo) can forge a session. The hard-fail
is the same posture as the agent API key (`BRANCHDAM_AGENT_API_KEY`):
fail closed rather than silently accept an insecure default.

Generate a key with:

```sh
openssl rand -base64 32
```

Store it in a gitignored `.env` (alongside `BRANCHDAM_AGENT_API_KEY`).
Treat it with the same care as a database password — see section 9 for
backup implications.

## 3. First-user setup

On a fresh database, the users table is empty. The SPA's `/login` page
calls `GET /api/v1/setup/status` on mount; when the response is
`{readyForSetup: true, mode: "local"}` (or `"both"`), the page renders
the **first-user setup form** instead of the regular login. Submitting
that form POSTs to `/api/v1/setup/admin`, which:

1. Hashes the password with argon2id (parameters in section 5).
2. Inserts a `source='local', is_admin=1` user inside a single write
   transaction (count-check + create are atomic; the count being zero
   IS the implicit setup token).
3. Mints a session cookie, persists the session row, and writes a
   `login_audit` row with `source='local', outcome='ok'`.
4. Returns 200 with the session cookie set.

The setup endpoint is **not** exposed when `auth.mode='forward'` —
calling it returns 409 Conflict. It's also not re-runnable: the moment
any user exists, `readyForSetup` becomes `false` and the form goes away.
The only way to bootstrap a new database's first admin is via the form.

Why not a CLI subcommand? Because the SPA's setup form is the one path
that doesn't require the operator to have shell access on the server.
A homelab operator who can reach the web UI but lost SSH can still
recover; a CLI bootstrap would lock that recovery behind shell access.

## 4. Cookie contract

| Attribute     | Value                                                  |
|---------------|--------------------------------------------------------|
| Name          | `branchdam_session` (configurable via `auth.local.cookieName`) |
| Value         | `<cookie_id_hex>.<hmac_hex>` — 32-byte random ID + HMAC-SHA-256 over the ID, using the 32-byte `BRANCHDAM_SECRET_KEY`-derived key |
| `HttpOnly`    | `true` — JavaScript cannot read the cookie              |
| `Secure`      | `true` when the request reached us over TLS directly, or via `X-Forwarded-Proto: https` from a configured trusted proxy (see `http.trustedProxies` in `docs/configuration.md`); `false` only in plain-HTTP local dev |
| `SameSite`    | `Lax`                                                   |
| `Path`        | `/`                                                     |
| Idle timeout  | `auth.local.idleTimeout`, default `24h`                 |
| Absolute timeout | `auth.local.absoluteTimeout`, default `720h` (30d)    |

`SameSite=Lax` is the same posture as the rest of the SPA: top-level
navigations from external links carry the cookie, but cross-site `POST`s
do not. CSRF on the mutating endpoints is gated by the `auth.AuthMode`
middleware chain; the cookie attribute is defense-in-depth, not the
primary defense.

The `Secure` attribute is conditional on the request's transport —
a plain-HTTP request from `make dev-api` (loopback) is intentionally
allowed to set a non-Secure cookie so the browser accepts it on the
local dev path. The conditional logic is in
`internal/auth/session/middleware.go`'s `SecureCookie(r, trustedProxies)`.

## 5. argon2id parameters and what they cost

The password-hash primitive is argon2id (the only modern choice that
survives the parallel-attack era; the rest of the KDF family — bcrypt,
scrypt, PBKDF2 — falls behind at current hardware). Defaults:

| Parameter   | Default | Notes                                           |
|-------------|---------|-------------------------------------------------|
| `memoryKB`  | 19456   | 19 MiB per password hash                        |
| `iterations`| 2       | Two passes over the memory cost                  |
| `parallelism` | 1     | Single-threaded per hash (simpler, portable)     |
| `saltLength` | 16     | 128-bit random salt                              |
| `keyLength`  | 32     | 256-bit derived key                              |

These match the [OWASP password-storage cheat sheet](https://cheatsheetseries.owasp.org/cheatsheets/Password_Storage_Cheat_Sheet.html)
baseline. On a typical x86_64 server, a single hash takes ~50ms — high
enough to make offline brute force on a stolen database expensive, low
enough that the login flow doesn't feel slow.

Memory cost: under sustained load the server holds roughly
`memoryKB × parallelism × concurrent_logins` of argon2 working memory
(≈19 MiB per in-flight login). For a homelab serving one user, this is
negligible. For a multi-tenant deployment behind a load balancer, scale
the hash worker budget in `auth.local.argon2.*` down before scaling
the login rate up — the resource ceiling is the box's RAM, not its
CPU.

Tuning guidance:

- **Homelab (1-5 users)**: defaults are fine. Don't tune.
- **Mid-size (10-50 users)**: leave defaults; monitor `process_resident_memory_bytes`.
- **Multi-tenant / shared**: drop `memoryKB` to 12288 (12 MiB) and `iterations` to 2.
  Going below 12 MiB / 2 iter materially weakens the hash; that's the
  OWASP floor.

Existing hashes retain their original parameters when verified — the
parameter set is encoded in the stored hash itself (`$argon2id$v=19$m=...,t=...,p=...$salt$key`).
An operator can raise `memoryKB` and `iterations` and the next hash
write will use the new values; old hashes verify against their stored
parameters until the user next changes their password.

## 6. Rate limit: per-IP sliding window

The login and password-reset endpoints are gated by a per-IP sliding
window (in `internal/auth/ratelimit`). Two thresholds:

| Window   | Default failures | Default cool-off | Rationale                                |
|----------|------------------|------------------|------------------------------------------|
| Fast     | 5 within 5 min   | 60 s             | Stop a focused brute force               |
| Slow     | 5 within 5 min (defaults to fast window) | 5 min | Stop a slow, persistent attacker |

Both windows are per-**source IP** (using `http.trustedProxies` to
identify the real client behind a reverse proxy), not per-account. This
is deliberate: a per-account limiter can be defeated by trying every
account, while a per-IP limiter is shared across the whole /24 (or /48)
that the attacker controls. The trade-off is shared-fate during
NAT-collisions (corporate offices, CGNAT, IPv6 /64s) — a runaway tab on
one workstation affects the whole NAT pool — but the alternative is
unbounded enumeration.

The fast cool-off (60s) is short enough that a fat-fingered user
recovers within a minute. The slow cool-off (5min) is a hard floor for
continued abuse; an operator who needs to allow a known-bad IP
momentarily can `make restart-api` to flush the limiter (it's an
in-process map, not persisted to SQLite).

When the limiter trips, the response is `429 Too Many Requests` with a
`Retry-After` header (in seconds) and a JSON body of the form
`{"error": "rate limited; retry after 1m0s"}`.

## 7. Forward-JIT (only when `auth.mode = both`)

When the chain is `both`, a forward-auth request with a group in
`auth.forward.adminGroups` provisions a local user on the fly. The
provisioning is **JIT (just-in-time)**: the forward-auth headers drive
the insert, the local users table gets a `source='forward-jit'` row,
and from then on the same browser session authenticates via the local
cookie path on subsequent requests (the BrowserChain and the
LocalUserView merge in `internal/auth/route.go`).

The config knobs:

- `auth.forward.adminGroups`: list of group names (forward-auth asserted
  via `X-Authentik-Groups`, pipe-`|`-delimited) that trigger JIT
  provisioning with `is_admin=1`. Empty list = no JIT, even in `both` mode.
- `auth.forward.requireEmailForJIT` (default `true`): when true, refuse
  JIT if the forward-auth asserted email is empty. A homelab Authentik
  deployment that doesn't surface email can set this to `false`; the
  JIT user is then keyed by username and the second forward-auth
  request for the same username returns the same user row.

Username-clash behavior: if a forward-JIT request arrives with an email
that matches an existing `source='local'` user, the local user wins on
the Name/Email merge and the JIT path becomes a no-op for that user.
The rationale lives in the `route.go` `mergeIdentity` function: the
interactive login is the source of truth once someone has a local
account, and a forward-auth assertion shouldn't be able to silently
downgrade a local admin.

## 8. Login audit log

Every authentication event — successful login, failed password, rate
limit trip, disabled-account attempt — writes a row to `login_audit`
(defined in migration `00018_local_auth.sql`). The schema:

| Column              | Notes                                                    |
|---------------------|----------------------------------------------------------|
| `id`                | Auto-incrementing                                          |
| `user_id`           | FK to `users.id`, NULL for unknown-user attempts          |
| `username_presented`| What the caller typed, before lookup                      |
| `source`            | `local`, `forward-jit-create`, or `forward-noop` (one of the three CHECK-enum values) |
| `outcome`           | `ok`, `bad-password`, `rate-limited`, `user-disabled`, `no-such-user`, `user-locked` |
| `ip`                | Source IP, resolved via `http.trustedProxies`              |
| `user_agent`        | Request `User-Agent`, truncated to 512 chars at write     |
| `details`           | Free-form JSON, default `{}`                              |
| `created_at`        | Unix seconds                                              |

Two indexes: `login_audit_user_time_idx` for "what happened to this
user" queries, `login_audit_ip_time_idx` for "who is coming from this
IP" queries. The table is append-only — there is no UPDATE or DELETE
path; the only operator-side cleanup is a manual SQLite `DELETE FROM
login_audit WHERE created_at < ?` for retention.

The follow-up PR #408 (admin user-management UI) will surface a 50-row
tail of this table on the `/admin/users` page, filterable by source and
outcome. Until that lands, the audit is queryable only via `sqlite3` on
the database file directly.

## 9. Operational notes

**Rotating `BRANCHDAM_SECRET_KEY`**: every existing session becomes
invalid the moment the key changes. There is no graceful migration —
the cookie HMAC tag is verified against the active key, and a tag
signed with the old key fails verification. The expected rotation
workflow is:

1. Notify users to log out (or just let the rotation happen; the SPA
   redirects to `/login` on a 401 from the session middleware).
2. Stop the server, set the new `BRANCHDAM_SECRET_KEY` in `.env`.
3. Restart. All users log in again with their existing credentials
   (password hashes are unaffected; only the cookie HMAC key changed).

Sessions do not survive a rotation. There is no key-versioning on the
cookie itself — a deliberate "fail loud" posture so an operator who
accidentally rotates can diagnose it from the symptom (everyone
unexpectedly logged out) rather than the silent alternative (some
sessions working, some not, depending on which key was active when
they were minted).

**Backups**: a `branchdam.db` backup pairs with the `BRANCHDAM_SECRET_KEY`
that was active when the backup was taken. Restoring the database
without restoring the key makes the app_settings table's encrypted
fields unreadable (and the session cookies unverifiable, but those are
transient). Treat `.env` with the same care as the database file — back
it up alongside.

**Forward-auth identity stays intact**: the local-auth surface does not
change how the `X-Authentik-*` headers are read or trusted. The header
isolation invariant in `AGENTS.md` is unchanged: `internal/auth.BrowserChain`
is still the only code in the repo that reads those headers, and the
`X-Authentik-*` chain still runs in front of the local-auth chain when
`auth.mode='both'`.

**No triggers, no CASCADE**: the `users` table has no `ON DELETE`
actions on its foreign keys — every FK is `RESTRICT`. Disabling a
local user (via `PATCH /api/v1/admin/users/{id}` from #408) sets
`disabled_at`; it does not delete the row. Sessions for a disabled
user are revoked by clearing `revoked_at`; their `login_audit` rows
are preserved. Recovery from a "deleted everything by accident" is via
backup, not via FK behavior.

**Code paths for the load-bearing pieces**:

- Cookie mint/verify: `internal/auth/users/users.go` (`MintCookieValue`,
  `VerifyCookieValue`).
- Session CRUD: `internal/auth/users/users.go` (`CreateSession`,
  `GetSessionByCookieID`, `TouchSession`, `RevokeSession`).
- Password hash: `internal/auth/users/users.go` (`HashPassword`,
  `VerifyPassword`).
- Rate limit: `internal/auth/ratelimit/ratelimit.go`.
- HTTP handlers: `internal/httpapi/local_auth.go`.
- SPA entry point: `web/src/pages/LoginPage.tsx`.

For changes to the auth surface, run `make check` and `make check-web`
before pushing; both gates must be green. For new env vars, update
`config.example.yaml` and `docs/configuration.md` in the same PR.
