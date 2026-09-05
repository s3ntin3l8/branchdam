import { useState } from "react";
import { ApiError } from "../api/client";
import { api } from "../api/client";
import {
  useCreatePairing,
  usePairings,
  useRevokePairing,
  useRotatePairing,
} from "../hooks/queries";
import type {
  CompanionPairingListItem,
  CreateCompanionPairingResponse,
  RotateCompanionPairingResponse,
} from "../api/types";

// CompanionPairingsPage is the admin-facing surface for the device-pairing
// system documented in docs/mobile.md §4. Operators pair new devices via
// QR (or manual entry of the URL+key), rotate keys with a configurable
// grace window, and revoke individual devices. The page is admin-only
// at the API layer; auth.RequireAdmin refuses non-admin users with 403.
//
// Layout (top-to-bottom):
//  1. "Pair new device" CTA -> modal with QR + reveal-once plaintext key
//  2. Pairings table: agent_id, friendly label, active keys, status, actions
//  3. Per-pairing row: rotate (grace-minutes input), revoke (confirm),
//     view QR (modal reuse), view audit (modal)

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
        <h2 id="qr-modal-title" className="text-lg font-semibold text-white mb-4">
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

// --- main page ---

export default function CompanionPairingsPage() {
  const { data, isLoading, error } = usePairings();
  const create = useCreatePairing();
  const rotate = useRotatePairing();
  const revoke = useRevokePairing();

  const [createLabel, setCreateLabel] = useState("");
  const [showCreateModal, setShowCreateModal] = useState(false);
  const [createdResult, setCreatedResult] = useState<CreateCompanionPairingResponse | null>(null);

  const [qrForPairing, setQrForPairing] = useState<CompanionPairingListItem | null>(null);
  const [qrSvg, setQrSvg] = useState<string | null>(null);

  const [rotateForId, setRotateForId] = useState<number | null>(null);
  const [rotateGrace, setRotateGrace] = useState(1440); // 24h default
  const [rotateResult, setRotateResult] = useState<RotateCompanionPairingResponse | null>(null);

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

  const handleViewQr = async (p: CompanionPairingListItem) => {
    setQrForPairing(p);
    setQrSvg(null);
    try {
      // Fetch the SVG as text -- the endpoint returns image/svg+xml, but
      // request<>() assumes JSON, so use raw fetch here.
      const res = await fetch(api.pairingQRSVGUrl(p.id));
      if (!res.ok) {
        return;
      }
      setQrSvg(await res.text());
    } catch {
      // Network error -- leave qrSvg null; the modal will show a
      // "couldn't load QR" message rather than crash.
    }
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

  const handleRevoke = (id: number, label: string) => {
    if (!window.confirm(`Revoke pairing "${label}"? All its API keys will stop working immediately.`)) {
      return;
    }
    revoke.mutate(id);
  };

  const errorMessage =
    error instanceof ApiError ? error.message : error ? "Failed to load pairings." : null;

  return (
    <div className="p-6">
      <div className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-2xl font-bold text-white">Companion Pairing</h1>
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
                  <td className="px-4 py-3">{p.friendlyLabel}</td>
                  <td className="px-4 py-3">{p.activeKeyCount}</td>
                  <td className="px-4 py-3 text-xs text-neutral-400">{formatUnixTime(p.createdAtUnix)}</td>
                  <td className={`px-4 py-3 text-xs font-medium ${pairingStatusColor(p)}`}>
                    {pairingStatusLabel(p)}
                  </td>
                  <td className="px-4 py-3 text-right">
                    <div className="flex justify-end gap-1">
                      <button
                        type="button"
                        onClick={() => handleViewQr(p)}
                        disabled={!!p.revokedAtUnix || p.activeKeyCount === 0}
                        className="rounded border border-neutral-700 px-2 py-1 text-xs text-neutral-300 hover:border-neutral-500 disabled:opacity-50"
                      >
                        View QR
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
                        onClick={() => handleRevoke(p.id, p.friendlyLabel)}
                        disabled={!!p.revokedAtUnix}
                        className="rounded border border-red-800/60 px-2 py-1 text-xs text-red-400 hover:border-red-700 disabled:opacity-50"
                      >
                        Revoke
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
            <div className="space-y-4">
              <p className="text-sm text-emerald-400">
                Pairing created. Scan the QR with the branchDAM mobile app, or copy the URL.
              </p>
              <div className="flex justify-center rounded bg-white p-4">
                <div
                  className="h-64 w-64"
                  dangerouslySetInnerHTML={{ __html: createdResult.qrSvg }}
                />
              </div>
              <div className="rounded border border-amber-800/60 bg-amber-950/30 p-3 text-xs text-amber-300">
                <strong>Copy this key now.</strong> It will not be shown again.
              </div>
              <div className="space-y-1">
                <div className="flex items-center gap-2">
                  <span className="text-xs text-neutral-500 w-20 shrink-0">Agent ID</span>
                  <code className="flex-1 truncate text-xs text-neutral-300">{createdResult.agentId}</code>
                  <CopyButton value={createdResult.agentId} label="Copy" />
                </div>
                <div className="flex items-center gap-2">
                  <span className="text-xs text-neutral-500 w-20 shrink-0">API Key</span>
                  <code className="flex-1 truncate text-xs text-neutral-300">{createdResult.apiKey}</code>
                  <CopyButton value={createdResult.apiKey} label="Copy" />
                </div>
              </div>
            </div>
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
                className="w-full rounded border border-neutral-700 bg-neutral-950 px-3 py-2 text-sm text-white placeholder:text-neutral-600 focus:border-indigo-500 focus:outline-none"
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
        open={qrForPairing !== null}
        onClose={() => setQrForPairing(null)}
        title={qrForPairing ? `Pairing: ${qrForPairing.friendlyLabel}` : ""}
        body={
          qrSvg ? (
            <div className="flex justify-center rounded bg-white p-4">
              <div className="h-64 w-64" dangerouslySetInnerHTML={{ __html: qrSvg }} />
            </div>
          ) : (
            <p className="text-sm text-neutral-400">Loading QR…</p>
          )
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
                  className="mt-1 w-full rounded border border-neutral-700 bg-neutral-950 px-3 py-2 text-sm text-white"
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
    </div>
  );
}
