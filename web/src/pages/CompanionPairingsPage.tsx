import { useRef, useState } from "react";
import { ApiError } from "../api/client";
import { api } from "../api/client";
import {
  useCreatePairing,
  useDeletePairing,
  usePairings,
  useRenamePairing,
  useRevokePairing,
  useRotatePairing,
} from "../hooks/queries";
import ConfirmDialog from "../components/ConfirmDialog";
import type {
  CompanionPairingListItem,
  CreateCompanionPairingResponse,
  PairingCredentialsResponse,
  RotateCompanionPairingResponse,
} from "../api/types";

// CompanionPairingsPage is the admin-facing surface for the device-pairing
// system documented in docs/mobile.md §4. Operators pair new devices via
// QR (or manual entry of the URL+key), rotate keys with a configurable
// grace window, rename labels, re-show credentials, and revoke individual
// devices. Mutating routes rely on auth.RequireAdmin; the credential
// reveal endpoints additionally require an authenticated admin session
// (requireSettingsAdmin — RequireAdmin's global GET bypass would leave
// GET /credentials and GET qr.svg open).
//
// Layout (top-to-bottom):
//  1. "Pair new device" CTA -> modal with QR + reveal-once plaintext key
//  2. Pairings table: agent_id, friendly label (pencil to rename),
//     active keys, status, actions
//  3. Per-pairing row: show credentials (audited re-fetch), rotate
//     (grace-minutes input), revoke (ConfirmDialog), delete
//     (ConfirmDialog), view audit (modal)

// --- helpers ---

function formatUnixTime(unix: number): string {
  if (!unix) return "—";
  // Intentionally local-time-only: operators are reading a server-side
  // log of their own actions, so server-local is what they want. UTC
  // would just force a mental conversion for the homelab use case.
  return new Date(unix * 1000).toLocaleString();
}

function pairingStatusLabel(p: CompanionPairingListItem): string {
  if (p.revokedAtUnix) return "Revoked";
  if (p.activeKeyCount === 0) return "No active key";
  return "Active";
}

function pairingStatusColor(p: CompanionPairingListItem): string {
  if (p.revokedAtUnix) return "text-red-400";
  if (p.activeKeyCount === 0) return "text-amber-400";
  return "text-emerald-400";
}

// --- modal ---

interface QrModalProps {
  open: boolean;
  onClose: () => void;
  title: string;
  body: React.ReactNode;
}

function QrModal({ open, onClose, title, body }: QrModalProps) {
  if (!open) return null;
  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="qr-modal-title"
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      onClick={onClose}
    >
      <div
        className="w-full max-w-lg rounded-lg border border-neutral-700 bg-neutral-900 p-6 shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <h2 id="qr-modal-title" className="text-lg font-semibold text-neutral-100 mb-4">
          {title}
        </h2>
        {body}
        <div className="mt-6 flex justify-end">
          <button
            type="button"
            onClick={onClose}
            className="rounded border border-neutral-700 px-3 py-1.5 text-xs text-neutral-300 hover:border-neutral-500"
          >
            Close
          </button>
        </div>
      </div>
    </div>
  );
}

function CopyButton({ value, label }: { value: string; label: string }) {
  const [copied, setCopied] = useState(false);
  return (
    <button
      type="button"
      onClick={async () => {
        try {
          await navigator.clipboard.writeText(value);
          setCopied(true);
          setTimeout(() => setCopied(false), 2000);
        } catch {
          // Clipboard API can fail in non-secure contexts; fall back to
          // a manual-select-only reveal without crashing the modal.
        }
      }}
      className="rounded bg-indigo-600 px-2 py-1 text-xs font-medium text-white hover:bg-indigo-500"
    >
      {copied ? "Copied" : label}
    </button>
  );
}

function PairAgentButton({ pairingUrl }: { pairingUrl: string }) {
  return (
    <a
      href={pairingUrl}
      className="inline-flex items-center rounded border border-indigo-500/50 bg-indigo-600/20 px-2.5 py-1 text-xs font-medium text-indigo-300 hover:bg-indigo-600/30"
    >
      Pair with local agent
    </a>
  );
}

// CredentialsBody renders the shared credential display used by both the
// create-success body and the show-credentials modal for an existing
// pairing. pairingUrl/apiKey may be empty on keyless servers (QR-only).
// variant="once" keeps the show-once warning (create/rotate mint a key
// the operator must copy before closing); variant="redisplays" is the
// reveal path, where "will not be shown again" would be false by design.
function CredentialsBody({
  agentId,
  apiKey,
  keyPreview,
  pairingUrl,
  qrSvg,
  variant = "once",
}: {
  agentId: string;
  apiKey: string;
  keyPreview: string;
  pairingUrl: string;
  qrSvg: string;
  variant?: "once" | "redisplays";
}) {
  return (
    <div className="space-y-4">
      <p className="text-sm text-emerald-400">
        Credentials for this pairing. Scan the QR with the branchDAM mobile app, or copy the URL.
      </p>
      <div className="flex justify-center rounded bg-white p-4">
        <div className="h-64 w-64" dangerouslySetInnerHTML={{ __html: qrSvg }} />
      </div>
      {variant === "once" ? (
        <div className="rounded border border-amber-800/60 bg-amber-950/30 p-3 text-xs text-amber-300">
          <strong>Copy this key now.</strong> It will not be shown again.
        </div>
      ) : (
        <div className="rounded border border-neutral-700/60 bg-neutral-900/40 p-3 text-xs text-neutral-400">
          Credentials can be re-opened later from this page. Each reveal is recorded in the audit log.
        </div>
      )}
      <div className="space-y-1">
        <div className="flex items-center gap-2">
          <span className="text-xs text-neutral-500 w-20 shrink-0">Agent ID</span>
          <code className="flex-1 truncate text-xs text-neutral-300">{agentId}</code>
          <CopyButton value={agentId} label="Copy" />
        </div>
        <div className="flex items-center gap-2">
          <span className="text-xs text-neutral-500 w-20 shrink-0">API Key</span>
          <code className="flex-1 truncate text-xs text-neutral-300">
            {apiKey || (keyPreview ? `••••${keyPreview}` : "Unavailable (keyless server)")}
          </code>
          {apiKey && <CopyButton value={apiKey} label="Copy" />}
        </div>
        {pairingUrl && (
          <div className="flex items-center gap-2 pt-1">
            <span className="text-xs text-neutral-500 w-20 shrink-0">Pairing URL</span>
            <code className="flex-1 truncate text-xs text-neutral-400">{pairingUrl}</code>
            <CopyButton value={pairingUrl} label="Copy URL" />
            <PairAgentButton pairingUrl={pairingUrl} />
          </div>
        )}
      </div>
    </div>
  );
}

// --- main page ---

export default function CompanionPairingsPage() {
  const { data, isLoading, error } = usePairings();
  const create = useCreatePairing();
  const rotate = useRotatePairing();
  const rename = useRenamePairing();
  const revoke = useRevokePairing();
  const deletePairing = useDeletePairing();

  const [createLabel, setCreateLabel] = useState("");
  const [showCreateModal, setShowCreateModal] = useState(false);
  const [createdResult, setCreatedResult] = useState<CreateCompanionPairingResponse | null>(null);

  // Show-credentials for an existing pairing: imperative api fetch (each
  // open is a fresh CREDENTIALS_REVEALED audit row), not React Query, so
  // state is always cleared on close rather than served from a cache.
  // credsSeqRef invalidates in-flight responses when the modal switches
  // pairings or closes, so a slow response for A never lands under B.
  const [credsForPairing, setCredsForPairing] = useState<CompanionPairingListItem | null>(null);
  const [credentials, setCredentials] = useState<PairingCredentialsResponse | null>(null);
  const [credsError, setCredsError] = useState<string | null>(null);
  const [credsLoading, setCredsLoading] = useState(false);
  const credsSeqRef = useRef(0);

  // Rename modal (pencil by label cell). Allowed on revoked pairings.
  const [renameForPairing, setRenameForPairing] = useState<CompanionPairingListItem | null>(null);
  const [renameLabel, setRenameLabel] = useState("");

  const [rotateForId, setRotateForId] = useState<number | null>(null);
  const [rotateGrace, setRotateGrace] = useState(1440); // 24h default
  const [rotateResult, setRotateResult] = useState<RotateCompanionPairingResponse | null>(null);

  // ConfirmDialog targets: revoke and delete replace the old
  // window.confirm prompts (pairing page only; UsersPage's three
  // confirms are tracked separately).
  const [revokeTarget, setRevokeTarget] = useState<CompanionPairingListItem | null>(null);
  const [deleteTarget, setDeleteTarget] = useState<CompanionPairingListItem | null>(null);

  const handleCreate = () => {
    create.mutate(
      { friendlyLabel: createLabel.trim() },
      {
        onSuccess: (res) => {
          setCreatedResult(res);
          setCreateLabel("");
        },
      }
    );
  };

  const closeCredentials = () => {
    credsSeqRef.current += 1; // invalidate any in-flight fetch
    setCredsForPairing(null);
    setCredentials(null);
    setCredsError(null);
    setCredsLoading(false);
  };

  const handleShowCredentials = async (p: CompanionPairingListItem) => {
    const seq = ++credsSeqRef.current;
    setCredsForPairing(p);
    setCredentials(null);
    setCredsError(null);
    setCredsLoading(true);
    try {
      const res = await api.pairingCredentials(p.id);
      if (seq !== credsSeqRef.current) return; // stale: modal switched or closed
      setCredentials(res);
    } catch (e) {
      if (seq !== credsSeqRef.current) return;
      setCredsError(
        e instanceof ApiError ? e.message : "Failed to load credentials for this pairing."
      );
    } finally {
      if (seq === credsSeqRef.current) setCredsLoading(false);
    }
  };

  const closeRename = () => {
    setRenameForPairing(null);
    setRenameLabel("");
  };

  const handleRename = () => {
    if (!renameForPairing) return;
    rename.mutate(
      { id: renameForPairing.id, input: { friendlyLabel: renameLabel.trim() } },
      {
        onSuccess: () => closeRename(),
      }
    );
  };

  const handleRotate = (id: number) => {
    rotate.mutate(
      { id, input: { graceMinutes: rotateGrace } },
      {
        onSuccess: (res) => {
          setRotateResult(res);
        },
      }
    );
  };

  const errorMessage =
    error instanceof ApiError ? error.message : error ? "Failed to load pairings." : null;

  return (
    <div className="p-6">
      <div className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-xl font-semibold text-neutral-100">Companion Pairing</h1>
          <p className="mt-1 text-sm text-neutral-400">
            Pair mobile devices (Android, iOS) with this server. Each device gets its own API key.
          </p>
        </div>
        <button
          type="button"
          onClick={() => {
            setShowCreateModal(true);
            setCreatedResult(null);
          }}
          className="rounded bg-indigo-600 px-3 py-1.5 text-sm font-medium text-white hover:bg-indigo-500"
        >
          Pair new device
        </button>
      </div>

      {errorMessage && (
        <div className="mb-6 rounded-lg border border-red-800/60 bg-red-950/30 p-4 text-sm text-red-300">
          {errorMessage}
        </div>
      )}

      {isLoading ? (
        <p className="text-sm text-neutral-400">Loading pairings…</p>
      ) : data && data.pairings.length === 0 ? (
        <div className="rounded-lg border border-neutral-800 bg-neutral-900/50 p-8 text-center text-neutral-400">
          No paired devices yet. Click <strong>Pair new device</strong> to create the first one.
        </div>
      ) : (
        <div className="overflow-x-auto rounded-lg border border-neutral-800 bg-neutral-900/50">
          <table className="w-full text-left text-sm">
            <thead className="border-b border-neutral-800 text-xs uppercase tracking-wider text-neutral-500">
              <tr>
                <th className="px-4 py-3">Agent ID</th>
                <th className="px-4 py-3">Label</th>
                <th className="px-4 py-3">Active Keys</th>
                <th className="px-4 py-3">Created</th>
                <th className="px-4 py-3">Status</th>
                <th className="px-4 py-3 text-right">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800/60 text-neutral-200">
              {data?.pairings.map((p) => (
                <tr key={p.id}>
                  <td className="px-4 py-3 font-mono text-xs">{p.agentId}</td>
                  <td className="px-4 py-3">
                    <span className="inline-flex items-center gap-1">
                      {p.friendlyLabel}
                      <button
                        type="button"
                        aria-label={`Rename ${p.friendlyLabel}`}
                        onClick={() => {
                          setRenameForPairing(p);
                          setRenameLabel(p.friendlyLabel);
                          rename.reset();
                        }}
                        className="rounded border border-neutral-700 px-1 text-xs text-neutral-400 hover:border-neutral-500 hover:text-neutral-200"
                      >
                        ✎
                      </button>
                    </span>
                  </td>
                  <td className="px-4 py-3">{p.activeKeyCount}</td>
                  <td className="px-4 py-3 text-xs text-neutral-400">{formatUnixTime(p.createdAtUnix)}</td>
                  <td className={`px-4 py-3 text-xs font-medium ${pairingStatusColor(p)}`}>
                    {pairingStatusLabel(p)}
                  </td>
                  <td className="px-4 py-3 text-right">
                    <div className="flex justify-end gap-1">
                      <button
                        type="button"
                        onClick={() => void handleShowCredentials(p)}
                        disabled={!!p.revokedAtUnix || p.activeKeyCount === 0}
                        className="rounded border border-neutral-700 px-2 py-1 text-xs text-neutral-300 hover:border-neutral-500 disabled:opacity-50"
                      >
                        Show credentials
                      </button>
                      <button
                        type="button"
                        onClick={() => {
                          setRotateForId(p.id);
                          setRotateResult(null);
                        }}
                        disabled={!!p.revokedAtUnix}
                        className="rounded border border-neutral-700 px-2 py-1 text-xs text-neutral-300 hover:border-neutral-500 disabled:opacity-50"
                      >
                        Rotate
                      </button>
                      <button
                        type="button"
                        onClick={() => {
                          setRevokeTarget(p);
                          revoke.reset();
                        }}
                        disabled={!!p.revokedAtUnix}
                        className="rounded border border-red-800/60 px-2 py-1 text-xs text-red-400 hover:border-red-700 disabled:opacity-50"
                      >
                        Revoke
                      </button>
                      <button
                        type="button"
                        onClick={() => {
                          setDeleteTarget(p);
                          deletePairing.reset();
                        }}
                        disabled={!p.revokedAtUnix}
                        className="rounded border border-red-900 px-2 py-1 text-xs text-red-500 hover:border-red-800 disabled:opacity-50"
                      >
                        Delete
                      </button>
                    </div>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <QrModal
        open={showCreateModal}
        onClose={() => setShowCreateModal(false)}
        title="Pair new device"
        body={
          createdResult ? (
            <CredentialsBody
              agentId={createdResult.agentId}
              apiKey={createdResult.apiKey}
              keyPreview={createdResult.keyPreview}
              pairingUrl={createdResult.pairingUrl}
              qrSvg={createdResult.qrSvg}
            />
          ) : (
            <div className="space-y-4">
              <p className="text-sm text-neutral-300">
                Enter a friendly label for this device. The server will mint a unique agent ID and an
                initial API key.
              </p>
              <input
                type="text"
                value={createLabel}
                onChange={(e) => setCreateLabel(e.target.value)}
                placeholder="e.g. Björn's iPhone 16 Pro"
                maxLength={120}
                className="w-full rounded border border-neutral-700 bg-neutral-950 px-3 py-2 text-sm text-neutral-100 placeholder:text-neutral-600 focus:border-indigo-500 focus:outline-none"
              />
              {create.error && (
                <p className="text-xs text-red-400">Failed: {create.error.message}</p>
              )}
              <div className="flex justify-end">
                <button
                  type="button"
                  onClick={handleCreate}
                  disabled={!createLabel.trim() || create.isPending}
                  className="rounded bg-indigo-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-indigo-500 disabled:opacity-50"
                >
                  {create.isPending ? "Creating…" : "Create pairing"}
                </button>
              </div>
            </div>
          )
        }
      />

      <QrModal
        open={credsForPairing !== null}
        onClose={closeCredentials}
        title={credsForPairing ? `Pairing: ${credsForPairing.friendlyLabel}` : ""}
        body={
          credsLoading ? (
            <p className="text-sm text-neutral-400">Loading credentials…</p>
          ) : credsError ? (
            <p className="text-sm text-red-400">{credsError}</p>
          ) : credentials ? (
            <CredentialsBody
              agentId={credentials.agentId}
              apiKey={credentials.apiKey}
              keyPreview={credentials.keyPreview}
              pairingUrl={credentials.pairingUrl}
              qrSvg={credentials.qrSvg}
              variant="redisplays"
            />
          ) : (
            <p className="text-sm text-neutral-400">No credentials loaded.</p>
          )
        }
      />

      <QrModal
        open={renameForPairing !== null}
        onClose={closeRename}
        title="Rename pairing"
        body={
          <div className="space-y-4">
            <p className="text-sm text-neutral-300">
              Choose a friendly label for this device. The label is display-only and may be changed
              at any time, including after revoke.
            </p>
            <input
              type="text"
              value={renameLabel}
              onChange={(e) => setRenameLabel(e.target.value)}
              placeholder="e.g. Björn's iPhone 16 Pro"
              maxLength={120}
              className="w-full rounded border border-neutral-700 bg-neutral-950 px-3 py-2 text-sm text-neutral-100 placeholder:text-neutral-600 focus:border-indigo-500 focus:outline-none"
            />
            {rename.error && (
              <p className="text-xs text-red-400">Failed: {rename.error.message}</p>
            )}
            <div className="flex justify-end">
              <button
                type="button"
                onClick={handleRename}
                disabled={!renameLabel.trim() || rename.isPending}
                className="rounded bg-indigo-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-indigo-500 disabled:opacity-50"
              >
                {rename.isPending ? "Saving…" : "Save label"}
              </button>
            </div>
          </div>
        }
      />

      <QrModal
        open={rotateForId !== null}
        onClose={() => setRotateForId(null)}
        title="Rotate API key"
        body={
          rotateResult ? (
            <div className="space-y-4">
              <p className="text-sm text-emerald-400">
                New key minted. The previous key still works until{" "}
                <strong>{formatUnixTime(rotateResult.previousKeyExpiresAtUnix)}</strong>.
              </p>
              <div className="rounded border border-amber-800/60 bg-amber-950/30 p-3 text-xs text-amber-300">
                <strong>Copy this key now.</strong> It will not be shown again.
              </div>
              <div className="flex items-center gap-2">
                <code className="flex-1 truncate text-xs text-neutral-300">{rotateResult.apiKey}</code>
                <CopyButton value={rotateResult.apiKey} label="Copy" />
              </div>
              {rotateResult.pairingUrl && (
                <div className="flex items-center gap-2 pt-1">
                  <span className="text-xs text-neutral-500 w-20 shrink-0">Pairing URL</span>
                  <code className="flex-1 truncate text-xs text-neutral-400">{rotateResult.pairingUrl}</code>
                  <CopyButton value={rotateResult.pairingUrl} label="Copy URL" />
                  <PairAgentButton pairingUrl={rotateResult.pairingUrl} />
                </div>
              )}
            </div>

          ) : (
            <div className="space-y-4">
              <p className="text-sm text-neutral-300">
                The new key activates immediately. The previous key continues to authenticate until the
                grace period ends, so the device has time to pick up the new key on its next handshake.
              </p>
              <label className="block text-xs text-neutral-400">
                Grace period (minutes)
                <input
                  type="number"
                  min="1"
                  max="10080"
                  value={rotateGrace}
                  onChange={(e) => setRotateGrace(Number(e.target.value))}
                  className="mt-1 w-full rounded border border-neutral-700 bg-neutral-950 px-3 py-2 text-sm text-neutral-100"
                />
              </label>
              {rotate.error && (
                <p className="text-xs text-red-400">Failed: {rotate.error.message}</p>
              )}
              <div className="flex justify-end">
                <button
                  type="button"
                  onClick={() => rotateForId !== null && handleRotate(rotateForId)}
                  disabled={rotate.isPending}
                  className="rounded bg-indigo-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-indigo-500 disabled:opacity-50"
                >
                  {rotate.isPending ? "Rotating…" : "Rotate key"}
                </button>
              </div>
            </div>
          )
        }
      />

      {revokeTarget && (
        <ConfirmDialog
          titleId="revoke-pairing-title"
          title="Revoke pairing"
          body={
            <>
              Revoke pairing{" "}
              <strong className="text-white">"{revokeTarget.friendlyLabel}"</strong>? All its API
              keys will stop working immediately.
            </>
          }
          confirmLabel="Confirm Revoke"
          pendingLabel="Revoking…"
          isPending={revoke.isPending}
          error={revoke.isError ? revoke.error : undefined}
          errorLabel="Failed to revoke"
          onConfirm={() => {
            revoke.mutate(revokeTarget.id, {
              onSuccess: () => setRevokeTarget(null),
            });
          }}
          onCancel={() => setRevokeTarget(null)}
        />
      )}

      {deleteTarget && (
        <ConfirmDialog
          titleId="delete-pairing-title"
          title="Delete pairing"
          body={
            <>
              Permanently delete pairing{" "}
              <strong className="text-white">"{deleteTarget.friendlyLabel}"</strong>? This cannot be
              undone.
            </>
          }
          confirmLabel="Confirm Delete"
          pendingLabel="Deleting…"
          isPending={deletePairing.isPending}
          error={deletePairing.isError ? deletePairing.error : undefined}
          errorLabel="Failed to delete"
          onConfirm={() => {
            deletePairing.mutate(deleteTarget.id, {
              onSuccess: () => setDeleteTarget(null),
            });
          }}
          onCancel={() => setDeleteTarget(null)}
        />
      )}
    </div>
  );
}
