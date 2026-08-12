import { useAuditQueue, useConfirmEdge, useRejectEdge } from "../hooks/queries";
import type { AuditEntry } from "../api/types";

function AuditRow({ entry }: { entry: AuditEntry }) {
  const confirm = useConfirmEdge();
  const reject = useRejectEdge();
  const busy = confirm.isPending || reject.isPending;

  return (
    <div className="flex items-center justify-between gap-4 rounded border border-neutral-800 p-3">
      <div className="min-w-0 flex-1">
        <div className="flex items-center gap-2 text-sm">
          <span className="font-mono text-neutral-300">
            {entry.sourceNodeId} → {entry.targetNodeId}
          </span>
          <span className="rounded bg-neutral-800 px-1.5 py-0.5 text-xs text-neutral-400">{entry.relationshipType}</span>
          {entry.parentMissing && (
            <span className="rounded bg-red-900/50 px-1.5 py-0.5 text-xs text-red-300">parent missing</span>
          )}
        </div>
        <div className="mt-1 text-xs text-neutral-500">
          confidence {entry.confidence.toFixed(2)} · resolver {entry.resolver}
        </div>
      </div>
      <div className="flex shrink-0 gap-2">
        <button
          type="button"
          disabled={busy}
          onClick={() => confirm.mutate(entry.id)}
          className="rounded bg-emerald-700 px-3 py-1.5 text-sm font-medium text-white hover:bg-emerald-600 disabled:opacity-50"
        >
          Confirm
        </button>
        <button
          type="button"
          disabled={busy}
          onClick={() => reject.mutate(entry.id)}
          className="rounded bg-neutral-800 px-3 py-1.5 text-sm font-medium text-neutral-200 hover:bg-neutral-700 disabled:opacity-50"
        >
          Reject
        </button>
      </div>
    </div>
  );
}

export default function AuditQueuePage() {
  const { data, isLoading, isError } = useAuditQueue({ limit: 100 });

  if (isLoading) return <div className="p-6 text-neutral-400">Loading…</div>;
  if (isError) return <div className="p-6 text-red-400">Failed to load the audit queue.</div>;

  const entries = data?.entries ?? [];

  return (
    <div className="p-6">
      <h1 className="mb-4 text-xl font-semibold">Audit Queue</h1>
      {entries.length === 0 ? (
        <p className="text-neutral-500">Nothing needs review right now.</p>
      ) : (
        <div className="space-y-2">
          {entries.map((e) => (
            <AuditRow key={e.id} entry={e} />
          ))}
        </div>
      )}
    </div>
  );
}
