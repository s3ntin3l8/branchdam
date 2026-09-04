import { useCallback, useEffect, useRef, useState } from "react";
import { useSearchParams } from "react-router";
import { useAuditQueue, useConfirmEdge, useCreateEdge, useRejectEdge } from "../hooks/queries";
import Thumbnail from "../components/Thumbnail";
import type { AuditEntry } from "../api/types";

const PAGE_SIZE = 50;

function formatTimestamp(unix?: number): string {
  if (!unix) return "—";
  return new Date(unix * 1000).toLocaleString();
}

function formatEvidence(evidenceJson?: string): React.ReactNode {
  if (!evidenceJson || evidenceJson === "{}" || evidenceJson === "") return null;
  try {
    const parsed = JSON.parse(evidenceJson);
    return (
      <div className="mt-2 rounded bg-neutral-950 p-2 font-mono text-xs text-neutral-400">
        <div className="mb-1 font-semibold text-neutral-500">Evidence:</div>
        {Object.entries(parsed).map(([k, v]) => (
          <div key={k} className="flex justify-between gap-2">
            <span className="text-neutral-500">{k}:</span>
            <span className="text-neutral-300">{String(v)}</span>
          </div>
        ))}
      </div>
    );
  } catch {
    return <div className="mt-2 font-mono text-xs text-neutral-400">Evidence: {evidenceJson}</div>;
  }
}

function AuditRow({ entry }: { entry: AuditEntry }) {
  const confirm = useConfirmEdge();
  const reject = useRejectEdge();
  const busy = confirm.isPending || reject.isPending;

  return (
    <div className="rounded border border-neutral-800 bg-neutral-900/50 p-4">
      {/* Top Bar: Edge Meta & Action Buttons */}
      <div className="mb-3 flex items-center justify-between border-b border-neutral-800/80 pb-3">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <span className="font-semibold text-neutral-200">Edge #{entry.id}</span>
          <span className="rounded bg-indigo-900/60 px-2 py-0.5 text-xs text-indigo-200 font-medium">
            {entry.relationshipType}
          </span>
          <span className="rounded bg-neutral-800 px-2 py-0.5 text-xs text-neutral-400">
            Tier {entry.tier}
          </span>
          <span className="text-xs text-neutral-400">
            Confidence: <strong className="text-neutral-200">{(entry.confidence * 100).toFixed(0)}%</strong>
          </span>
          <span className="text-xs text-neutral-400">
            Resolver: <code className="text-neutral-300">{entry.resolver}</code>
          </span>
          {entry.parentMissing && (
            <span className="rounded bg-red-900/60 px-2 py-0.5 text-xs text-red-200">parent missing</span>
          )}
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

      {(confirm.isError || reject.isError) && (
        <div className="mb-3 text-xs text-red-400">
          Failed to {confirm.isError ? "confirm" : "reject"} edge: {String(confirm.error || reject.error)}
        </div>
      )}

      {/* Side-by-side comparison */}
      <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
        {/* Source Node (Parent) */}
        <div className="rounded border border-neutral-800/60 bg-neutral-950/40 p-3">
          <div className="mb-2 text-xs font-bold uppercase tracking-wider text-blue-400">
            Parent (Node #{entry.sourceNodeId})
          </div>
          <div className="mb-2 flex items-start gap-3">
            <Thumbnail
              assetId={entry.sourceNodeId}
              thumbState={entry.sourceNode?.thumbState ?? "PENDING"}
              alt={entry.sourceNode?.fileName ?? `Node ${entry.sourceNodeId}`}
              className="h-20 w-20 shrink-0 rounded object-cover"
            />
            <div className="min-w-0">
              <div className="truncate text-sm font-semibold text-neutral-200" title={entry.sourceNode?.fileName}>
                {entry.sourceNode?.fileName || `Node ${entry.sourceNodeId}`}
              </div>
              <div className="truncate text-xs text-neutral-500" title={entry.sourceNode?.filePath}>
                {entry.sourceNode?.filePath || "—"}
              </div>
            </div>
          </div>
          <div className="mt-2 space-y-1 text-xs text-neutral-400">
            <div>
              <span className="text-neutral-500">Captured:</span> {formatTimestamp(entry.sourceNode?.capturedAtUnix)}
            </div>
            <div>
              <span className="text-neutral-500">Camera:</span> {entry.sourceNode?.cameraModel || "—"}
            </div>
            <div>
              <span className="text-neutral-500">pHash:</span>{" "}
              {entry.sourceNode?.phash ? <code className="font-mono text-neutral-300">{entry.sourceNode.phash}</code> : "—"}
            </div>
          </div>
        </div>

        {/* Signals / Deltas / Evidence */}
        <div className="flex flex-col justify-between rounded border border-neutral-800/60 bg-neutral-950/20 p-3">
          <div>
            <div className="mb-2 text-xs font-bold uppercase tracking-wider text-neutral-400">
              Matching Metrics
            </div>
            <div className="space-y-1 text-xs">
              <div className="flex justify-between">
                <span className="text-neutral-500">Timestamp Delta:</span>
                <span className="font-mono font-medium text-neutral-200">
                  {entry.captureDeltaSeconds !== undefined ? `${entry.captureDeltaSeconds}s` : "N/A"}
                </span>
              </div>
              <div className="flex justify-between">
                <span className="text-neutral-500">pHash Distance:</span>
                <span className="font-mono font-medium text-neutral-200">
                  {entry.phashDistance !== undefined ? `${entry.phashDistance} bits` : "N/A"}
                </span>
              </div>
            </div>
          </div>
          {formatEvidence(entry.evidenceJson)}
        </div>

        {/* Target Node (Child) */}
        <div className="rounded border border-neutral-800/60 bg-neutral-950/40 p-3">
          <div className="mb-2 text-xs font-bold uppercase tracking-wider text-emerald-400">
            Child (Node #{entry.targetNodeId})
          </div>
          <div className="mb-2 flex items-start gap-3">
            <Thumbnail
              assetId={entry.targetNodeId}
              thumbState={entry.targetNode?.thumbState ?? "PENDING"}
              alt={entry.targetNode?.fileName ?? `Node ${entry.targetNodeId}`}
              className="h-20 w-20 shrink-0 rounded object-cover"
            />
            <div className="min-w-0">
              <div className="truncate text-sm font-semibold text-neutral-200" title={entry.targetNode?.fileName}>
                {entry.targetNode?.fileName || `Node ${entry.targetNodeId}`}
              </div>
              <div className="truncate text-xs text-neutral-500" title={entry.targetNode?.filePath}>
                {entry.targetNode?.filePath || "—"}
              </div>
            </div>
          </div>
          <div className="mt-2 space-y-1 text-xs text-neutral-400">
            <div>
              <span className="text-neutral-500">Captured:</span> {formatTimestamp(entry.targetNode?.capturedAtUnix)}
            </div>
            <div>
              <span className="text-neutral-500">Camera:</span> {entry.targetNode?.cameraModel || "—"}
            </div>
            <div>
              <span className="text-neutral-500">pHash:</span>{" "}
              {entry.targetNode?.phash ? <code className="font-mono text-neutral-300">{entry.targetNode.phash}</code> : "—"}
            </div>
          </div>
        </div>
      </div>
    </div>
  );
}

function ManualLinkModal({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) {
  const [sourceId, setSourceId] = useState("");
  const [targetId, setTargetId] = useState("");
  const [relType, setRelType] = useState<AuditEntry["relationshipType"]>("DERIVED_FROM");
  const [errorMsg, setErrorMsg] = useState("");
  const dialogRef = useRef<HTMLDivElement>(null);
  const previousFocusRef = useRef<HTMLElement | null>(null);
  const wasOpenRef = useRef(false);

  const createEdge = useCreateEdge();

  useEffect(() => {
    if (isOpen) {
      if (!wasOpenRef.current) {
        previousFocusRef.current = document.activeElement as HTMLElement;
        wasOpenRef.current = true;
        const timer = setTimeout(() => {
          dialogRef.current?.querySelector<HTMLInputElement>("input")?.focus();
        }, 50);
        return () => clearTimeout(timer);
      }
    } else {
      if (wasOpenRef.current) {
        wasOpenRef.current = false;
        if (previousFocusRef.current) {
          previousFocusRef.current.focus();
          previousFocusRef.current = null;
        }
      }
    }
  }, [isOpen]);

  const handleKeyDown = useCallback((e: React.KeyboardEvent) => {
    if (e.key === "Escape") {
      e.stopPropagation();
      onClose();
    }
  }, [onClose]);

  const handleTrapFocus = useCallback((e: React.KeyboardEvent) => {
    if (e.key !== "Tab" || !dialogRef.current) return;
    const focusable = dialogRef.current.querySelectorAll<HTMLElement>(
      'input, select, button, [tabindex]:not([tabindex="-1"])'
    );
    if (focusable.length === 0) return;
    const first = focusable[0];
    const last = focusable[focusable.length - 1];
    if (e.shiftKey && document.activeElement === first) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && document.activeElement === last) {
      e.preventDefault();
      first.focus();
    }
  }, []);

  if (!isOpen) return null;

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault();
    setErrorMsg("");

    const src = Number(sourceId);
    const tgt = Number(targetId);

    if (!src || Number.isNaN(src) || !tgt || Number.isNaN(tgt)) {
      setErrorMsg("Please enter valid numeric Source and Target Node IDs.");
      return;
    }

    createEdge.mutate(
      { sourceNodeId: src, targetNodeId: tgt, relationshipType: relType },
      {
        onSuccess: () => {
          onClose();
          setSourceId("");
          setTargetId("");
        },
        onError: (err: unknown) => {
          setErrorMsg((err as { message?: string }).message || "Failed to create manual edge");
        },
      }
    );
  };

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4 backdrop-blur-xs"
      onClick={(e) => { if (e.target === e.currentTarget) onClose(); }}
    >
      <div
        ref={dialogRef}
        role="dialog"
        aria-modal="true"
        aria-labelledby="manual-link-modal-title"
        onKeyDown={(e) => { handleKeyDown(e); handleTrapFocus(e); }}
        className="w-full max-w-md rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow-xl"
      >
        <h2 id="manual-link-modal-title" className="mb-4 text-lg font-semibold text-white">Manual Link Edge</h2>
        {errorMsg && (
          <div className="mb-4 rounded bg-red-900/60 p-3 text-xs text-red-200 border border-red-800">
            {errorMsg}
          </div>
        )}
        <form onSubmit={handleSubmit} className="space-y-4 text-sm">
          <div>
            <label className="mb-1 block font-medium text-neutral-300">Parent (Source) Node ID</label>
            <input
              type="number"
              value={sourceId}
              onChange={(e) => setSourceId(e.target.value)}
              placeholder="e.g. 42"
              className="w-full rounded border border-neutral-800 bg-neutral-950 px-3 py-2 text-white focus:outline-none focus:ring-1 focus:ring-indigo-500"
              required
            />
          </div>

          <div>
            <label className="mb-1 block font-medium text-neutral-300">Child (Target) Node ID</label>
            <input
              type="number"
              value={targetId}
              onChange={(e) => setTargetId(e.target.value)}
              placeholder="e.g. 108"
              className="w-full rounded border border-neutral-800 bg-neutral-950 px-3 py-2 text-white focus:outline-none focus:ring-1 focus:ring-indigo-500"
              required
            />
          </div>

          <div>
            <label className="mb-1 block font-medium text-neutral-300">Relationship Type</label>
            <select
              value={relType}
              onChange={(e) => setRelType(e.target.value as AuditEntry["relationshipType"])}
              className="w-full rounded border border-neutral-800 bg-neutral-950 px-3 py-2 text-white focus:outline-none focus:ring-1 focus:ring-indigo-500"
            >
              <option value="DERIVED_FROM">DERIVED_FROM</option>
              <option value="FINAL_EXPORT">FINAL_EXPORT</option>
              <option value="PROXY_OF">PROXY_OF</option>
              <option value="PROJECT_SIDECAR">PROJECT_SIDECAR</option>
              <option value="DUPLICATE_OF">DUPLICATE_OF</option>
            </select>
          </div>

          <div className="flex justify-end gap-2 pt-2">
            <button
              type="button"
              onClick={onClose}
              className="rounded bg-neutral-800 px-4 py-2 text-neutral-300 hover:bg-neutral-700"
            >
              Cancel
            </button>
            <button
              type="submit"
              disabled={createEdge.isPending}
              className="rounded bg-indigo-600 px-4 py-2 font-medium text-white hover:bg-indigo-500 disabled:opacity-50"
            >
              {createEdge.isPending ? "Linking…" : "Create Link"}
            </button>
          </div>
        </form>
      </div>
    </div>
  );
}

export default function AuditQueuePage() {
  const [searchParams, setSearchParams] = useSearchParams();
  // beforeId=0 means "first page". We store it in the URL so a deep
  // link shares the right cursor; offset is intentionally not in the URL
  // because offset drifts across mutations (confirm/reject on page N
  // shifts the offset window).
  const beforeId = Number(searchParams.get("beforeId") || "0") || 0;
  const { data, isLoading, isError } = useAuditQueue({ limit: PAGE_SIZE, beforeId });
  const [isModalOpen, setIsModalOpen] = useState(false);

  const entries = data?.entries ?? [];
  const total = data?.total ?? 0;
  // hasMore: server returns total consistently with the page (single
  // query with COUNT(*) OVER()). If the page is full, there may be more.
  const hasMore = entries.length === PAGE_SIZE;

  const handleFirst = () => {
    const nextParams = new URLSearchParams(searchParams);
    nextParams.delete("beforeId");
    setSearchParams(nextParams);
  };

  const handleNext = () => {
    if (entries.length === 0) return;
    const lastId = entries[entries.length - 1].id;
    const nextParams = new URLSearchParams(searchParams);
    nextParams.set("beforeId", String(lastId));
    setSearchParams(nextParams);
    window.scrollTo(0, 0);
  };

  if (isLoading) return <div className="p-6 text-neutral-400">Loading…</div>;
  if (isError) return <div className="p-6 text-red-400">Failed to load the audit queue.</div>;

  return (
    <div className="p-6">
      <div className="mb-6 flex items-center justify-between">
        <div>
          <h1 className="text-xl font-semibold text-white">Audit Queue</h1>
          <p className="text-sm text-neutral-400">Review edge resolution candidates or manually link assets.</p>
        </div>
        <button
          type="button"
          onClick={() => setIsModalOpen(true)}
          className="rounded bg-indigo-600 px-4 py-2 text-sm font-medium text-white hover:bg-indigo-500"
        >
          + Manual Link Edge
        </button>
      </div>

      {entries.length === 0 ? (
        <p className="text-neutral-500">Nothing needs review right now.</p>
      ) : (
        <>
          <div className="space-y-4">
            {entries.map((e) => (
              <AuditRow key={e.id} entry={e} />
            ))}
          </div>

          <div className="flex items-center justify-between pt-4 border-t border-neutral-800 text-xs text-neutral-400 mt-4">
            <div>{total} item{total === 1 ? "" : "s"} awaiting review</div>
            <div className="flex items-center space-x-2">
              <button
                aria-label="First page"
                disabled={beforeId === 0}
                onClick={handleFirst}
                className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1 text-neutral-300 hover:bg-neutral-700 disabled:opacity-40 disabled:hover:bg-neutral-800"
              >
                First
              </button>
              <button
                aria-label="Next page"
                disabled={!hasMore}
                onClick={handleNext}
                className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1 text-neutral-300 hover:bg-neutral-700 disabled:opacity-40 disabled:hover:bg-neutral-800"
              >
                Next
              </button>
            </div>
          </div>
        </>
      )}

      <ManualLinkModal isOpen={isModalOpen} onClose={() => setIsModalOpen(false)} />
    </div>
  );
}
