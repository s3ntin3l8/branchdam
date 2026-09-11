import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { useSearchParams } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { useAuditQueue, useConfirmEdge, useCreateEdge, useRejectEdge } from "../hooks/queries";
import Thumbnail from "../components/Thumbnail";
import NodePickerModal from "../components/NodePickerModal";
import type { EdgeAuditEntry } from "../api/types";

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

function AuditRow({ entry }: { entry: EdgeAuditEntry }) {
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
  const [relType, setRelType] = useState<EdgeAuditEntry["relationshipType"]>("DERIVED_FROM");
  const [errorMsg, setErrorMsg] = useState("");
  const [isSourcePickerOpen, setIsSourcePickerOpen] = useState(false);
  const [isTargetPickerOpen, setIsTargetPickerOpen] = useState(false);
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
          if (!dialogRef.current?.contains(document.activeElement)) {
            dialogRef.current?.querySelector<HTMLInputElement>("input")?.focus();
          }
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
    <>
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
          <h2 id="manual-link-modal-title" className="mb-4 text-lg font-semibold text-neutral-100">Manual Link Edge</h2>
          {errorMsg && (
            <div className="mb-4 rounded bg-red-900/60 p-3 text-xs text-red-200 border border-red-800">
              {errorMsg}
            </div>
          )}
          <form onSubmit={handleSubmit} className="space-y-4 text-sm">
            <div>
              <label className="mb-1 block font-medium text-neutral-300">Parent (Source) Node ID</label>
              <div className="flex gap-2">
                <input
                  type="number"
                  value={sourceId}
                  onChange={(e) => setSourceId(e.target.value)}
                  placeholder="e.g. 42"
                  className="w-full rounded border border-neutral-800 bg-neutral-950 px-3 py-2 text-neutral-100 focus:outline-none focus:ring-1 focus:ring-indigo-500"
                  required
                />
                <button
                  type="button"
                  onClick={() => setIsSourcePickerOpen(true)}
                  aria-label="Select Parent Node"
                  className="shrink-0 rounded border border-neutral-700 bg-neutral-800 px-3 py-2 text-xs font-medium text-neutral-200 hover:bg-neutral-700 hover:text-neutral-100"
                >
                  Pick Node
                </button>
              </div>
            </div>

            <div>
              <label className="mb-1 block font-medium text-neutral-300">Child (Target) Node ID</label>
              <div className="flex gap-2">
                <input
                  type="number"
                  value={targetId}
                  onChange={(e) => setTargetId(e.target.value)}
                  placeholder="e.g. 108"
                  className="w-full rounded border border-neutral-800 bg-neutral-950 px-3 py-2 text-neutral-100 focus:outline-none focus:ring-1 focus:ring-indigo-500"
                  required
                />
                <button
                  type="button"
                  onClick={() => setIsTargetPickerOpen(true)}
                  aria-label="Select Target Asset"
                  className="shrink-0 rounded border border-neutral-700 bg-neutral-800 px-3 py-2 text-xs font-medium text-neutral-200 hover:bg-neutral-700 hover:text-neutral-100"
                >
                  Pick Node
                </button>
              </div>
            </div>

            <div>
              <label className="mb-1 block font-medium text-neutral-300">Relationship Type</label>
              <select
                value={relType}
                onChange={(e) => setRelType(e.target.value as EdgeAuditEntry["relationshipType"])}
                className="w-full rounded border border-neutral-800 bg-neutral-950 px-3 py-2 text-neutral-100 focus:outline-none focus:ring-1 focus:ring-indigo-500"
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

      <NodePickerModal
        isOpen={isSourcePickerOpen}
        onClose={() => setIsSourcePickerOpen(false)}
        onSelect={(asset) => {
          setSourceId(String(asset.id));
        }}
        selectedAssetId={sourceId}
        title="Select Parent (Source) Node"
      />

      <NodePickerModal
        isOpen={isTargetPickerOpen}
        onClose={() => setIsTargetPickerOpen(false)}
        onSelect={(asset) => {
          setTargetId(String(asset.id));
        }}
        selectedAssetId={targetId}
        title="Select Child (Target) Node"
      />
    </>
  );
}

function BatchConfirmModal({
  isOpen,
  action,
  edges,
  onClose,
  onComplete,
}: {
  isOpen: boolean;
  action: "confirm" | "reject";
  edges: EdgeAuditEntry[];
  onClose: () => void;
  onComplete: () => void;
}) {
  const [running, setRunning] = useState(false);
  const [progress, setProgress] = useState(0);
  const [errorMsg, setErrorMsg] = useState("");
  const confirmMutation = useConfirmEdge();
  const rejectMutation = useRejectEdge();

  useEffect(() => {
    if (!isOpen) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !running) {
        onClose();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [isOpen, running, onClose]);

  if (!isOpen) return null;

  const handleExecute = async () => {
    setRunning(true);
    setErrorMsg("");
    let succeeded = 0;
    let failed = 0;
    try {
      for (const edge of edges) {
        try {
          if (action === "confirm") {
            await confirmMutation.mutateAsync(edge.id);
          } else {
            await rejectMutation.mutateAsync(edge.id);
          }
          succeeded++;
        } catch {
          failed++;
        }
        setProgress(succeeded + failed);
      }
    } finally {
      setRunning(false);
      if (failed > 0) {
        setErrorMsg(`${failed} of ${edges.length} edges failed to ${action}.`);
      } else {
        onComplete();
      }
    }
  };

  const percent = edges.length > 0 ? Math.round((progress / edges.length) * 100) : 0;

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4"
      role="dialog"
      aria-modal="true"
      aria-labelledby="batch-confirm-modal-title"
    >
      <div className="max-w-md w-full rounded-lg border border-neutral-800 bg-neutral-900 p-6 space-y-4">
        <h3 id="batch-confirm-modal-title" className="text-base font-semibold text-neutral-100">
          Batch {action === "confirm" ? "Confirm" : "Reject"} Edges
        </h3>
        <p className="text-xs text-neutral-300">
          Are you sure you want to <strong>{action}</strong> all{" "}
          <strong className="text-white">{edges.length}</strong> currently filtered edge candidates on this page?
        </p>

        {running && (
          <div className="space-y-1">
            <div className="flex justify-between text-xs text-neutral-400">
              <span>Progress</span>
              <span>{progress} / {edges.length}</span>
            </div>
            <div className="h-1.5 w-full bg-neutral-800 rounded-full overflow-hidden">
              <div
                className={`h-full ${action === "confirm" ? "bg-emerald-500" : "bg-red-500"}`}
                style={{ width: `${percent}%` }}
              />
            </div>
          </div>
        )}

        {errorMsg && (
          <p className="text-xs text-red-400">{errorMsg}</p>
        )}

        <div className="flex justify-end gap-2 pt-2">
          <button
            type="button"
            disabled={running}
            onClick={onClose}
            className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-xs text-neutral-300 hover:bg-neutral-700 disabled:opacity-50"
          >
            {errorMsg ? "Close" : "Cancel"}
          </button>
          {!errorMsg && (
            <button
              type="button"
              disabled={running || edges.length === 0}
              onClick={handleExecute}
              className={`rounded px-3 py-1.5 text-xs font-medium text-white disabled:opacity-50 ${
                action === "confirm"
                  ? "bg-emerald-600 hover:bg-emerald-500"
                  : "bg-red-600 hover:bg-red-500"
              }`}
            >
              {running ? "Processing…" : action === "confirm" ? "Confirm All" : "Reject All"}
            </button>
          )}
        </div>
      </div>
    </div>
  );
}

export default function AuditQueuePage() {
  const queryClient = useQueryClient();
  const [searchParams, setSearchParams] = useSearchParams();
  // beforeId=0 means "first page". We store it in the URL so a deep
  // link shares the right cursor; offset is intentionally not in the URL
  // because offset drifts across mutations (confirm/reject on page N
  // shifts the offset window).
  const beforeId = Number(searchParams.get("beforeId") || "0") || 0;
  const { data, isLoading, isError } = useAuditQueue({ limit: PAGE_SIZE, beforeId });
  const [isModalOpen, setIsModalOpen] = useState(false);
  const [tierFilter, setTierFilter] = useState("");
  const [resolverFilter, setResolverFilter] = useState("");
  const [relFilter, setRelFilter] = useState("");
  const [batchAction, setBatchAction] = useState<"confirm" | "reject" | null>(null);

  const entries = useMemo(() => data?.entries ?? [], [data?.entries]);
  const total = data?.total ?? 0;
  // hasMore: server returns total consistently with the page (single
  // query with COUNT(*) OVER()). If the page is full, there may be more.
  const hasMore = entries.length === PAGE_SIZE;

  const resolvers = useMemo(() => {
    return Array.from(new Set(entries.map((e) => e.resolver))).sort();
  }, [entries]);

  const filteredEntries = useMemo(() => {
    return entries.filter((e) => {
      if (tierFilter && String(e.tier) !== tierFilter) return false;
      if (resolverFilter && e.resolver !== resolverFilter) return false;
      if (relFilter && e.relationshipType !== relFilter) return false;
      return true;
    });
  }, [entries, tierFilter, resolverFilter, relFilter]);

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
    <div className="p-6 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-xl font-semibold text-neutral-100">Audit Queue</h1>
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

      {entries.length > 0 && (
        <div className="flex flex-wrap items-center justify-between gap-4 rounded-lg border border-neutral-800 bg-neutral-900/80 p-4 text-xs">
          <div className="flex flex-wrap items-center gap-3">
            <div>
              <label htmlFor="tier-filter" className="block text-neutral-400 mb-1">Tier</label>
              <select
                id="tier-filter"
                value={tierFilter}
                onChange={(e) => setTierFilter(e.target.value)}
                className="rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 focus:outline-none"
              >
                <option value="">All Tiers</option>
                <option value="1">Tier 1 (Sidecars)</option>
                <option value="2">Tier 2 (Stems / XMP)</option>
                <option value="3">Tier 3 (Perceptual)</option>
              </select>
            </div>

            <div>
              <label htmlFor="resolver-filter" className="block text-neutral-400 mb-1">Resolver</label>
              <select
                id="resolver-filter"
                value={resolverFilter}
                onChange={(e) => setResolverFilter(e.target.value)}
                className="rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 focus:outline-none"
              >
                <option value="">All Resolvers</option>
                {resolvers.map((r) => (
                  <option key={r} value={r}>{r}</option>
                ))}
              </select>
            </div>

            <div>
              <label htmlFor="relationship-filter" className="block text-neutral-400 mb-1">Relationship</label>
              <select
                id="relationship-filter"
                value={relFilter}
                onChange={(e) => setRelFilter(e.target.value)}
                className="rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 focus:outline-none"
              >
                <option value="">All Relationships</option>
                <option value="DERIVED_FROM">Derived From</option>
                <option value="FINAL_EXPORT">Final Export</option>
                <option value="PROXY_OF">Proxy Of</option>
                <option value="PROJECT_SIDECAR">Project Sidecar</option>
                <option value="DUPLICATE_OF">Duplicate Of</option>
              </select>
            </div>

            {(tierFilter || resolverFilter || relFilter) && (
              <button
                type="button"
                onClick={() => { setTierFilter(""); setResolverFilter(""); setRelFilter(""); }}
                className="self-end text-xs text-amber-400 hover:text-amber-300 border border-amber-800/60 rounded px-2.5 py-1.5 bg-amber-950/40"
              >
                Clear Filters
              </button>
            )}
          </div>

          {filteredEntries.length > 0 && (
            <div className="flex items-center gap-2 self-end">
              <button
                type="button"
                onClick={() => setBatchAction("confirm")}
                className="rounded bg-emerald-700 px-3 py-1.5 text-xs font-medium text-white hover:bg-emerald-600 shadow"
              >
                Accept Filtered ({filteredEntries.length} on page)
              </button>
              <button
                type="button"
                onClick={() => setBatchAction("reject")}
                className="rounded bg-neutral-800 px-3 py-1.5 text-xs font-medium text-red-300 hover:bg-neutral-700 border border-red-900/60"
              >
                Decline Filtered ({filteredEntries.length} on page)
              </button>
            </div>
          )}
        </div>
      )}

      {entries.length === 0 ? (
        <p className="text-neutral-500">Nothing needs review right now.</p>
      ) : filteredEntries.length === 0 ? (
        <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-8 text-center text-neutral-500">
          No edge candidates match the selected filters.
        </div>
      ) : (
        <>
          <div className="space-y-4">
            {filteredEntries.map((e) => (
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

      {batchAction && (
        <BatchConfirmModal
          isOpen={true}
          action={batchAction}
          edges={filteredEntries}
          onClose={() => {
            setBatchAction(null);
            void queryClient.invalidateQueries({ queryKey: ["audit-queue"] });
          }}
          onComplete={() => {
            setBatchAction(null);
            void queryClient.invalidateQueries({ queryKey: ["audit-queue"] });
          }}
        />
      )}
    </div>
  );
}
