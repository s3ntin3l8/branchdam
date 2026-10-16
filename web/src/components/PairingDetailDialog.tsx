import { useState } from "react";
import { usePairing, usePairingAudit } from "../hooks/queries";
import type { CompanionPairingAuditItem, CompanionPairingKey } from "../api/types";
import Dialog from "./Dialog";

// PairingDetailDialog shows one companion pairing in full: its keys
// (GET /api/v1/companion/pairings/{id}) and its paged audit trail
// (GET /api/v1/companion/pairings/{id}/audit). Opened from the "Details"
// button on CompanionPairingsPage's rows. Read-only: the row's own
// buttons still do rotate, revoke and delete.

const PAIRING_AUDIT_PAGE_SIZE = 20;

function formatUnixTime(unix?: number): string {
  if (!unix) return "—";
  return new Date(unix * 1000).toLocaleString();
}

function keyStatus(key: CompanionPairingKey, nowSec: number): { label: string; className: string } {
  if (key.revokedAtUnix) return { label: "Revoked", className: "text-red-400" };
  if (key.expiresAtUnix && key.expiresAtUnix <= nowSec) return { label: "Expired", className: "text-neutral-500" };
  if (key.expiresAtUnix) return { label: "In grace period", className: "text-amber-400" };
  return { label: "Active", className: "text-emerald-400" };
}

function AuditDetails({ detailsJson }: { detailsJson: string }) {
  if (!detailsJson || detailsJson === "{}") return <span className="text-neutral-600">—</span>;
  let pretty = detailsJson;
  try {
    pretty = JSON.stringify(JSON.parse(detailsJson), null, 2);
  } catch {
    // not JSON: show as-is
  }
  return (
    <details>
      <summary className="cursor-pointer text-indigo-400 hover:text-indigo-300">Details</summary>
      <pre className="mt-1 max-h-40 overflow-auto rounded border border-neutral-800 bg-neutral-950 p-2 font-mono text-[11px] text-neutral-300">
        {pretty}
      </pre>
    </details>
  );
}

export default function PairingDetailDialog({ pairingId, onClose }: { pairingId: number; onClose: () => void }) {
  const [page, setPage] = useState(0);
  const [nowSec] = useState(() => Math.floor(Date.now() / 1000));
  const detail = usePairing(pairingId);
  const audit = usePairingAudit(pairingId, { limit: PAIRING_AUDIT_PAGE_SIZE, offset: page * PAIRING_AUDIT_PAGE_SIZE });

  const p = detail.data;
  const total = audit.data?.total ?? 0;
  const pages = Math.max(1, Math.ceil(total / PAIRING_AUDIT_PAGE_SIZE));
  const events: CompanionPairingAuditItem[] = audit.data?.events ?? [];

  return (
    <Dialog
      title={p ? `Pairing details: ${p.friendlyLabel}` : "Pairing details"}
      onClose={onClose}
      overlayClassName="z-50 bg-black/60"
      panelClassName="w-full max-w-3xl max-h-[90vh] overflow-y-auto rounded-lg border border-neutral-700 bg-neutral-900 p-6 shadow-xl"
      titleClassName="text-lg font-semibold text-neutral-100 mb-4"
    >
      {detail.isLoading ? (
        <p className="text-sm text-neutral-400">Loading pairing…</p>
      ) : detail.isError || !p ? (
        <p role="alert" className="text-sm text-red-400">
          Failed to load pairing: {detail.error?.message ?? "not found"}
        </p>
      ) : (
        <div className="space-y-6 text-sm">
          <dl className="grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-xs">
            <dt className="text-neutral-500">Agent ID</dt>
            <dd className="font-mono text-neutral-200 break-all">{p.agentId}</dd>
            <dt className="text-neutral-500">Created</dt>
            <dd className="text-neutral-300">
              {formatUnixTime(p.createdAtUnix)}
              {p.createdBy ? ` by ${p.createdBy}` : ""}
            </dd>
            <dt className="text-neutral-500">Status</dt>
            <dd className={p.revokedAtUnix ? "text-red-400" : "text-emerald-400"}>
              {p.revokedAtUnix ? `Revoked ${formatUnixTime(p.revokedAtUnix)}` : "Active"}
            </dd>
          </dl>

          <section aria-labelledby="pairing-keys-heading">
            <h3 id="pairing-keys-heading" className="mb-2 text-sm font-semibold text-neutral-200">
              API keys
            </h3>
            {p.keys.length === 0 ? (
              <p className="text-xs text-neutral-500">No keys.</p>
            ) : (
              <div className="overflow-x-auto rounded border border-neutral-800">
                <table className="w-full text-left text-xs">
                  <thead className="border-b border-neutral-800 text-neutral-500">
                    <tr>
                      <th scope="col" className="px-3 py-2">Key</th>
                      <th scope="col" className="px-3 py-2">Created</th>
                      <th scope="col" className="px-3 py-2">Expires</th>
                      <th scope="col" className="px-3 py-2">Status</th>
                    </tr>
                  </thead>
                  <tbody className="divide-y divide-neutral-800/60 text-neutral-300">
                    {p.keys.map((k) => {
                      const status = keyStatus(k, nowSec);
                      return (
                        <tr key={k.id}>
                          <td className="px-3 py-2 font-mono">••••{k.keyPreview}</td>
                          <td className="whitespace-nowrap px-3 py-2">{formatUnixTime(k.createdAtUnix)}</td>
                          <td className="whitespace-nowrap px-3 py-2">{formatUnixTime(k.expiresAtUnix)}</td>
                          <td className={`px-3 py-2 font-medium ${status.className}`}>{status.label}</td>
                        </tr>
                      );
                    })}
                  </tbody>
                </table>
              </div>
            )}
          </section>

          <section aria-labelledby="pairing-audit-heading">
            <h3 id="pairing-audit-heading" className="mb-2 text-sm font-semibold text-neutral-200">
              Audit trail
            </h3>
            {audit.isLoading ? (
              <p className="text-xs text-neutral-400">Loading audit trail…</p>
            ) : audit.isError ? (
              <p role="alert" className="text-xs text-red-400">
                Failed to load audit trail: {audit.error.message}
              </p>
            ) : events.length === 0 ? (
              <p className="text-xs text-neutral-500">No audit events.</p>
            ) : (
              <>
                <div className="overflow-x-auto rounded border border-neutral-800">
                  <table className="w-full text-left text-xs">
                    <thead className="border-b border-neutral-800 text-neutral-500">
                      <tr>
                        <th scope="col" className="px-3 py-2">When</th>
                        <th scope="col" className="px-3 py-2">Event</th>
                        <th scope="col" className="px-3 py-2">Actor</th>
                        <th scope="col" className="px-3 py-2">Details</th>
                      </tr>
                    </thead>
                    <tbody className="divide-y divide-neutral-800/60 text-neutral-300">
                      {events.map((e) => (
                        <tr key={e.id}>
                          <td className="whitespace-nowrap px-3 py-2">{formatUnixTime(e.createdAtUnix)}</td>
                          <td className="px-3 py-2 font-mono text-indigo-300">{e.event}</td>
                          <td className="px-3 py-2">{e.actor || "—"}</td>
                          <td className="px-3 py-2">
                            <AuditDetails detailsJson={e.detailsJson} />
                          </td>
                        </tr>
                      ))}
                    </tbody>
                  </table>
                </div>
                {pages > 1 && (
                  <div className="mt-2 flex items-center justify-between text-xs text-neutral-400">
                    <span>
                      Page {page + 1} of {pages}
                    </span>
                    <div className="flex gap-2">
                      <button
                        type="button"
                        disabled={page === 0}
                        onClick={() => setPage((n) => Math.max(0, n - 1))}
                        className="rounded border border-neutral-700 px-2 py-1 hover:border-neutral-500 disabled:opacity-40"
                      >
                        Newer
                      </button>
                      <button
                        type="button"
                        disabled={page + 1 >= pages}
                        onClick={() => setPage((n) => n + 1)}
                        className="rounded border border-neutral-700 px-2 py-1 hover:border-neutral-500 disabled:opacity-40"
                      >
                        Older
                      </button>
                    </div>
                  </div>
                )}
              </>
            )}
          </section>
        </div>
      )}
      <div className="mt-6 flex justify-end">
        <button
          type="button"
          onClick={onClose}
          className="rounded border border-neutral-700 px-3 py-1.5 text-xs text-neutral-300 hover:border-neutral-500"
        >
          Close
        </button>
      </div>
    </Dialog>
  );
}
