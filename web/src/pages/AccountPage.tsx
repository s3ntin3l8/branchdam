import { useId, useState, type FormEvent } from "react";
import { Link } from "react-router";
import type { CreatePatResponse, Me, Pat, PatScope } from "../api/types";
import { useCreatePat, useMe, useMfaDisable, usePats, useRevokePat } from "../hooks/queries";
import ConfirmDialog from "../components/ConfirmDialog";
import Dialog from "../components/Dialog";

// AccountPage (/account) is the signed-in user's own security settings:
// their TOTP second factor (any local user) and their personal access
// tokens (admins only -- the server mints a PAT only for a live local
// admin, and a PAT stops authenticating once its owner is demoted).
// Reached from the account menu at the bottom of the sidebar.

function formatUnixTime(unix: number | undefined): string {
  if (!unix) return "—";
  return new Date(unix * 1000).toLocaleString();
}

const sectionClass = "rounded-lg border border-neutral-800 bg-neutral-900/40 p-5";
const inputClass =
  "mt-1 w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100";
const labelClass = "block text-xs text-neutral-400";
const secondaryButton =
  "rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-xs text-neutral-300 hover:bg-neutral-700 disabled:opacity-50";
const primaryButton =
  "rounded bg-brand-dark px-3 py-1.5 text-xs font-medium text-white hover:bg-indigo-700 disabled:opacity-50";

export default function AccountPage() {
  const { data: me, isLoading } = useMe();

  if (isLoading || !me) {
    return <div className="p-6 text-neutral-400">Loading account…</div>;
  }

  return (
    <div className="mx-auto max-w-3xl space-y-6 p-4 sm:p-6">
      <header>
        <h1 className="text-xl font-semibold text-neutral-100">Account security</h1>
        <p className="mt-1 text-sm text-neutral-400">
          Signed in as <span className="text-neutral-200">{me.name ?? "unknown"}</span>
        </p>
      </header>
      <MfaSection me={me} />
      <PatSection isAdmin={me.isAdmin} />
    </div>
  );
}

// --- MFA ---

function MfaSection({ me }: { me: Me }) {
  const [disableOpen, setDisableOpen] = useState(false);

  return (
    <section aria-labelledby="mfa-heading" className={sectionClass}>
      <div className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h2 id="mfa-heading" className="text-base font-semibold text-neutral-100">
            Two-factor authentication
          </h2>
          <p className="mt-1 text-sm text-neutral-400">
            Ask for a code from an authenticator app after your password.
          </p>
        </div>
        {me.isLocal ? (
          me.mfaEnabled ? (
            <span className="rounded border border-emerald-800/60 bg-emerald-950/80 px-2 py-0.5 text-xs text-emerald-300">
              On
            </span>
          ) : (
            <span className="rounded bg-neutral-800/80 px-2 py-0.5 text-xs text-neutral-400">Off</span>
          )
        ) : null}
      </div>
      <div className="mt-4">
        {!me.isLocal ? (
          <p className="text-sm text-neutral-400">
            Your sign-in is managed by your identity provider. Set up two-factor authentication there.
          </p>
        ) : me.mfaEnabled ? (
          <button type="button" onClick={() => setDisableOpen(true)} className={secondaryButton}>
            Turn off two-factor authentication
          </button>
        ) : (
          <Link to="/mfa/setup" className={`inline-block ${primaryButton}`}>
            Set up two-factor authentication
          </Link>
        )}
      </div>
      {disableOpen && <MfaDisableDialog onClose={() => setDisableOpen(false)} />}
    </section>
  );
}

function MfaDisableDialog({ onClose }: { onClose: () => void }) {
  const disable = useMfaDisable();
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const ids = useId();
  const passwordId = `${ids}-password`;
  const codeId = `${ids}-code`;
  const introId = `${ids}-intro`;

  const submit = (e: FormEvent) => {
    e.preventDefault();
    if (!password || !code.trim()) return;
    disable.mutate({ password, code: code.trim() }, { onSuccess: onClose });
  };

  return (
    <Dialog title="Turn off two-factor authentication" onClose={onClose} dismissible={!disable.isPending}>
      <form onSubmit={submit} className="space-y-3">
        <p id={introId} className="text-xs leading-relaxed text-neutral-300">
          Confirm with your password and a current code from your authenticator app. Your recovery
          codes stop working too.
        </p>
        <div>
          <label htmlFor={passwordId} className={labelClass}>
            Password
          </label>
          <input
            id={passwordId}
            type="password"
            required
            autoComplete="current-password"
            aria-describedby={introId}
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            className={inputClass}
          />
        </div>
        <div>
          <label htmlFor={codeId} className={labelClass}>
            Authenticator code
          </label>
          <input
            id={codeId}
            type="text"
            required
            inputMode="numeric"
            autoComplete="one-time-code"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            placeholder="123456"
            className={inputClass}
          />
        </div>
        {disable.isError ? (
          <p role="alert" className="text-xs text-red-400">
            {disable.error.message}
          </p>
        ) : null}
        <div className="flex justify-end gap-2 pt-2">
          <button type="button" onClick={onClose} disabled={disable.isPending} className={secondaryButton}>
            Cancel
          </button>
          <button
            type="submit"
            disabled={disable.isPending || !password || !code.trim()}
            className="rounded bg-red-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-red-500 disabled:opacity-50"
          >
            {disable.isPending ? "Turning off…" : "Turn off"}
          </button>
        </div>
      </form>
    </Dialog>
  );
}

// --- Personal access tokens ---

// Mirrors auth.PATGrantableScopes and httpapi's patScopeFor route groups.
const SCOPE_OPTIONS: { value: PatScope; label: string; hint: string }[] = [
  { value: "admin", label: "admin", hint: "Admin API routes, except tokens and companion pairings" },
  { value: "pairings:write", label: "pairings:write", hint: "Companion pairings" },
  { value: "pats:write", label: "pats:write", hint: "Personal access tokens" },
  { value: "*", label: "* (full access)", hint: "Every admin route, including minting tokens" },
];

function patStatus(pat: Pat, nowSec: number): "active" | "revoked" | "expired" {
  if (pat.revokedAt) return "revoked";
  if (pat.expiresAt && pat.expiresAt <= nowSec) return "expired";
  return "active";
}

// endOfDayUnix turns a yyyy-mm-dd date input into unix seconds at the end
// of that day in the viewer's time zone (so "expires on the 3rd" means it
// still works on the 3rd). Returns undefined for an empty or invalid date.
function endOfDayUnix(date: string): number | undefined {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(date);
  if (!m) return undefined;
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]), 23, 59, 59);
  if (Number.isNaN(d.getTime())) return undefined;
  return Math.floor(d.getTime() / 1000);
}

function PatSection({ isAdmin }: { isAdmin: boolean }) {
  const pats = usePats({ enabled: isAdmin });
  const [revoking, setRevoking] = useState<Pat | null>(null);
  const [created, setCreated] = useState<CreatePatResponse | null>(null);
  const revoke = useRevokePat();
  // "Now" for the expired badge, fixed when the page mounts.
  const [nowSec] = useState(() => Math.floor(Date.now() / 1000));

  return (
    <section aria-labelledby="pats-heading" className={sectionClass}>
      <h2 id="pats-heading" className="text-base font-semibold text-neutral-100">
        Personal access tokens
      </h2>
      <p className="mt-1 text-sm text-neutral-400">
        Tokens let scripts and tools call the admin API as you, with a Bearer header.
      </p>
      {!isAdmin ? (
        <p className="mt-4 text-sm text-neutral-400">Personal access tokens are available to administrators.</p>
      ) : (
        <>
          <CreatePatForm onCreated={setCreated} />
          <div className="mt-6">
            {pats.isLoading ? (
              <p className="text-sm text-neutral-500">Loading tokens…</p>
            ) : pats.isError ? (
              <p role="alert" className="text-sm text-red-400">
                Failed to load tokens: {pats.error.message}
              </p>
            ) : !pats.data || pats.data.pats.length === 0 ? (
              <p className="text-sm text-neutral-500">No tokens yet.</p>
            ) : (
              <div className="overflow-x-auto rounded border border-neutral-800">
                <table className="w-full text-left text-sm text-neutral-200">
                  <thead className="border-b border-neutral-800 bg-neutral-900/60 text-xs uppercase tracking-wider text-neutral-400">
                    <tr>
                      <th scope="col" className="px-3 py-2">Name</th>
                      <th scope="col" className="px-3 py-2">Scopes</th>
                      <th scope="col" className="px-3 py-2">Created</th>
                      <th scope="col" className="px-3 py-2">Last used</th>
                      <th scope="col" className="px-3 py-2">Expires</th>
                      <th scope="col" className="px-3 py-2">Status</th>
                      <th scope="col" className="px-3 py-2 text-right">
                        <span className="sr-only">Actions</span>
                      </th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-neutral-800/60">
                    {pats.data.pats.map((pat) => {
                      const status = patStatus(pat, nowSec);
                      return (
                        <tr key={pat.id}>
                          <td className="px-3 py-2">
                            <div className="font-medium text-neutral-100">{pat.name}</div>
                            <div className="font-mono text-[11px] text-neutral-500">{pat.hashedKeyPrefix}…</div>
                          </td>
                          <td className="px-3 py-2 font-mono text-xs text-neutral-300">{pat.scopes.join(", ")}</td>
                          <td className="whitespace-nowrap px-3 py-2 text-xs text-neutral-400">{formatUnixTime(pat.createdAt)}</td>
                          <td className="whitespace-nowrap px-3 py-2 text-xs text-neutral-400">
                            {pat.lastUsedAt ? formatUnixTime(pat.lastUsedAt) : "Never"}
                          </td>
                          <td className="whitespace-nowrap px-3 py-2 text-xs text-neutral-400">
                            {pat.expiresAt ? formatUnixTime(pat.expiresAt) : "Never"}
                          </td>
                          <td className="px-3 py-2">
                            {status === "active" ? (
                              <span className="rounded border border-emerald-800/60 bg-emerald-950/80 px-2 py-0.5 text-xs text-emerald-300">
                                Active
                              </span>
                            ) : (
                              <span className="rounded bg-neutral-800/80 px-2 py-0.5 text-xs text-neutral-400">
                                {status === "revoked" ? "Revoked" : "Expired"}
                              </span>
                            )}
                          </td>
                          <td className="px-3 py-2 text-right">
                            {!pat.revokedAt && (
                              <button
                                type="button"
                                onClick={() => {
                                  revoke.reset();
                                  setRevoking(pat);
                                }}
                                aria-label={`Revoke ${pat.name}`}
                                className="rounded border border-red-900/60 px-2.5 py-1 text-xs text-red-300 hover:border-red-700"
                              >
                                Revoke
                              </button>
                            )}
                          </td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </div>
        </>
      )}
      {revoking && (
        <ConfirmDialog
          title={`Revoke "${revoking.name}"?`}
          body="Anything using this token stops working immediately. This can't be undone."
          confirmLabel="Revoke"
          pendingLabel="Revoking…"
          isPending={revoke.isPending}
          error={revoke.error?.message}
          errorLabel="Revoke failed"
          onConfirm={() => revoke.mutate(revoking.id, { onSuccess: () => setRevoking(null) })}
          onCancel={() => setRevoking(null)}
        />
      )}
      {created && <PatRevealDialog created={created} onDone={() => setCreated(null)} />}
    </section>
  );
}

function CreatePatForm({ onCreated }: { onCreated: (pat: CreatePatResponse) => void }) {
  const create = useCreatePat();
  const [name, setName] = useState("");
  const [scopes, setScopes] = useState<PatScope[]>(["admin"]);
  const [expires, setExpires] = useState("");
  const [formError, setFormError] = useState<string | null>(null);
  const ids = useId();
  const nameId = `${ids}-name`;
  const expiresId = `${ids}-expires`;
  const scopeId = (scope: PatScope) => `${ids}-scope-${scope.replace(/[^a-z]/gi, "_")}`;

  const toggleScope = (scope: PatScope) =>
    setScopes((prev) => (prev.includes(scope) ? prev.filter((s) => s !== scope) : [...prev, scope]));

  const submit = (e: FormEvent) => {
    e.preventDefault();
    setFormError(null);
    const trimmed = name.trim();
    if (!trimmed) {
      setFormError("Give the token a name.");
      return;
    }
    if (scopes.length === 0) {
      setFormError("Pick at least one scope.");
      return;
    }
    let expiresAt: number | undefined;
    if (expires) {
      expiresAt = endOfDayUnix(expires);
      if (expiresAt === undefined || expiresAt <= Date.now() / 1000) {
        setFormError("The expiry date must be in the future.");
        return;
      }
    }
    create.mutate(
      { name: trimmed, scopes, ...(expiresAt ? { expiresAt } : {}) },
      {
        onSuccess: (data) => {
          setName("");
          setScopes(["admin"]);
          setExpires("");
          onCreated(data);
        },
      },
    );
  };

  return (
    <form onSubmit={submit} aria-label="Create token" className="mt-4 space-y-3">
      <div className="grid gap-3 sm:grid-cols-2">
        <div>
          <label htmlFor={nameId} className={labelClass}>
            Name
          </label>
          <input
            id={nameId}
            type="text"
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="backup script"
            className={inputClass}
          />
        </div>
        <div>
          <label htmlFor={expiresId} className={labelClass}>
            Expires (optional)
          </label>
          <input
            id={expiresId}
            type="date"
            value={expires}
            onChange={(e) => setExpires(e.target.value)}
            className={inputClass}
          />
        </div>
      </div>
      <fieldset>
        <legend className="text-xs text-neutral-400">Scopes</legend>
        <div className="mt-1 grid gap-1 sm:grid-cols-2">
          {SCOPE_OPTIONS.map((opt) => {
            const id = scopeId(opt.value);
            return (
              <div key={opt.value} className="flex items-start gap-2 text-sm text-neutral-200">
                <input
                  id={id}
                  type="checkbox"
                  checked={scopes.includes(opt.value)}
                  onChange={() => toggleScope(opt.value)}
                  aria-describedby={`${id}-hint`}
                  className="mt-1"
                />
                <div>
                  <label htmlFor={id} className="font-mono text-xs">
                    {opt.label}
                  </label>
                  <p id={`${id}-hint`} className="text-xs text-neutral-500">
                    {opt.hint}
                  </p>
                </div>
              </div>
            );
          })}
        </div>
      </fieldset>
      {formError || create.isError ? (
        <p role="alert" className="text-xs text-red-400">
          {formError ?? create.error?.message}
        </p>
      ) : null}
      <button type="submit" disabled={create.isPending} className={primaryButton}>
        {create.isPending ? "Creating…" : "Create token"}
      </button>
    </form>
  );
}

// PatRevealDialog shows a new token's plaintext, the only time the server
// ever returns it. Escape and the backdrop don't close it: only Done does,
// so a stray key press can't lose the token before it's copied.
function PatRevealDialog({ created, onDone }: { created: CreatePatResponse; onDone: () => void }) {
  const [copied, setCopied] = useState(false);
  const noticeId = useId();

  const copy = async () => {
    try {
      await navigator.clipboard.writeText(created.plaintext);
      setCopied(true);
    } catch {
      setCopied(false);
    }
  };

  return (
    <Dialog title={`Token "${created.name}" created`} dismissible={false} onClose={onDone}>
      <div className="space-y-3">
        <p id={noticeId} className="text-xs leading-relaxed text-amber-300">
          Copy this token now. It won't be shown again.
        </p>
        <input
          readOnly
          aria-label="New token"
          aria-describedby={noticeId}
          value={created.plaintext}
          onFocus={(e) => e.currentTarget.select()}
          className="w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 font-mono text-xs text-neutral-100"
        />
        <p className="text-xs text-neutral-400">
          Send it as <code className="text-neutral-300">Authorization: Bearer &lt;token&gt;</code>.
        </p>
        <div className="flex justify-end gap-2 pt-2">
          <button type="button" onClick={() => void copy()} className={secondaryButton}>
            {copied ? "Copied" : "Copy"}
          </button>
          <button type="button" onClick={onDone} className={primaryButton}>
            Done
          </button>
        </div>
      </div>
    </Dialog>
  );
}
