# Local Auth — Implementation Plan

This is the in-worktree reference for the local-auth implementation.

## Decisions

| # | Decision | Choice |
|---|---|---|
| 1 | Interaction model when both configured | Either-path-succeeds |
| 2 | Group union | Union both sources |
| 3 | Account linking | Email-keyed |
| 4 | Bootstrap (local-only) | One-time setup prompt on `/login` when `users` empty (no env vars) |
| 5 | Bootstrap (local+forward) | JIT-provision local `is_admin=true` when X-Authentik-Email user is in `auth.forward.adminGroups` |
| 6 | Admin policy | `(local.is_admin == true) ∨ (any-group ∈ authz.groups)` |
| 7 | MFA | None in v1 |
| 8 | Password hash | argon2id (OWASP-recommended baseline) |
| 9 | Session shape | Server-side `sessions` table + signed cookie |
| 10 | Password reset | Deferred to follow-up PR |
| 11 | Rate limit | Per-IP exponential backoff (in-memory sliding window) |
| 12 | `auth.mode` default | `"forward"` (zero behavior change on upgrade; opt-in) |

## Deferred (out of scope for v1)

- Password reset (admin + self-service)
- Admin user-management endpoints/UI (operators can use SQL for now)
- MFA / TOTP
- Background reconciler for forward-jit admins removed from admin group
- `docs/local-auth.md` (code comments carry the contract)

## Tables (migration 00018)

`users`, `sessions`, `login_audit`. UNIQUE on `(email, source)` (not just
`email`) so local and forward-jit users with the same email don't
collide.

## Cookie contract

- Name: `__Host-branchdam_session` (HTTPS) / `branchdam_session` (HTTP)
- Value: `<cookie_id_hex>.<hmac_hex>`
- HMAC key: derived from `BRANCHDAM_SECRET_KEY`
- Attributes: `HttpOnly`, `Secure`, `SameSite=Lax`, `Path=/`

## sqlc workaround

sqlc v1.20-v1.31.1 all corrupt the regenerated sqlcgen files in this
environment (the existing companion_pairing.sql subqueries break the
parser). The new tables / queries / methods are hand-added in the
sqlcgen package style with a doc comment explaining the workaround.

## Order of work

1. Migration + sqlc (foundation)
2. `internal/auth/users` (hashing, CRUD, sessions table)
3. `internal/auth/ratelimit` (independent)
4. `internal/auth/session` (middleware)
5. `internal/auth/route.go` v2 (merge logic)
6. `internal/auth/authz.go` v2 (LocalUserView)
7. `internal/config` (auth config parsing)
8. HTTP endpoints (`/setup/*`, `/login`, `DELETE /session`, `/me` update)
9. Wire into `httpapi/Handler()`
10. SPA: `LoginPage`, `SessionContext`, `RequireAuth`
11. Tests
12. Lint, typecheck, commit, PR
