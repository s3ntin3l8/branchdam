import { useCallback, useId, useMemo, useRef, useState } from "react";
import { Link, useSearchParams } from "react-router";
import {
  useAuditQueue,
  useConfirmEdge,
  useCreateEdge,
  useInvalidateEdgeReviewQueries,
  useMe,
  useRejectEdge,
  useResetEdgeReview,
} from "../hooks/queries";
import { api } from "../api/client";
import Thumbnail from "../components/Thumbnail";
import NodePickerModal from "../components/NodePickerModal";
import Dialog from "../components/Dialog";
import ConfirmDialog from "../components/ConfirmDialog";
import type { AuditNode, AuditQueueParams, EdgeAuditEntry } from "../api/types";
import { formatEvidence, type EvidenceRow } from "../lib/formatEvidence";

const PAGE_SIZE = 50;
// "Select all matching" pulls the filtered queue in pages of this size and
// stops at SELECT_ALL_CAP edges, so one click can't queue an unbounded batch.
const SELECT_ALL_PAGE_SIZE = 500;
const SELECT_ALL_CAP = 1000;
// How many recent decisions keep an Undo button -- at least a full
// "select all matching" batch, so every edge of a batch stays undoable.
const RECENT_LIMIT = SELECT_ALL_CAP;

const RELATIONSHIP_TYPES: { value: EdgeAuditEntry["relationshipType"]; label: string }[] = [
  { value: "DERIVED_FROM", label: "Derived From" },
  { value: "FINAL_EXPORT", label: "Final Export" },
  { value: "PROXY_OF", label: "Proxy Of" },
  { value: "PROJECT_SIDECAR", label: "Project Sidecar" },
  { value: "DUPLICATE_OF", label: "Duplicate Of" },
];

const MIN_CONFIDENCE_OPTIONS = ["0.5", "0.6", "0.7", "0.8", "0.9"];

type ReviewAction = "confirm" | "reject";

// A decision made on this page this session, kept so it can be undone
// (POST /edges/{id}/reset-review) after the edge has left the queue.
interface RecentDecision {
  id: number;
  action: ReviewAction;
  summary: string;
}

function edgeSummary(entry: EdgeAuditEntry): string {
  const src = entry.sourceNode?.fileName || `Node ${entry.sourceNodeId}`;
  const tgt = entry.targetNode?.fileName || `Node ${entry.targetNodeId}`;
  return `${src} -> ${tgt} (${entry.relationshipType})`;
}

function formatTimestamp(unix?: number): string {
  if (!unix) return "—";
  return new Date(unix * 1000).toLocaleString();
}

function EvidenceRows({ rows }: { rows: EvidenceRow[] }) {
  return (
    <dl>
      {rows.map((row) => (
        <div key={row.key} className="flex justify-between gap-2">
          <dt className="text-neutral-500">{row.label}:</dt>
          <dd className="min-w-0 break-all text-right text-neutral-300">{row.value}</dd>
        </div>
      ))}
    </dl>
  );
}

function EvidencePanel({ evidenceJson }: { evidenceJson?: string }) {
  const headingId = useId();
  const evidence = useMemo(() => formatEvidence(evidenceJson), [evidenceJson]);
  if (evidence.kind === "empty") return null;
  if (evidence.kind === "raw") {
    return <div className="mt-2 break-all font-mono text-xs text-neutral-400">Evidence: {evidence.raw}</div>;
  }
  return (
    <section
      aria-labelledby={headingId}
      className="mt-2 rounded bg-neutral-950 p-2 font-mono text-xs text-neutral-400"
    >
      <div id={headingId} className="mb-1 font-semibold text-neutral-500">Evidence:</div>
      {evidence.flat.length > 0 && evidence.groups.length === 0 && <EvidenceRows rows={evidence.flat} />}
      {evidence.flat.length > 0 && evidence.groups.length > 0 && (
        // Mixed payload: give the flat rows their own heading so they line up
        // with the resolver groups instead of sitting at heading level.
        <div role="group" aria-labelledby={`${headingId}-flat`} className="mt-1">
          <div id={`${headingId}-flat`} className="font-semibold text-neutral-400">
            Other signals
          </div>
          <div className="pl-2">
            <EvidenceRows rows={evidence.flat} />
          </div>
        </div>
      )}
      {evidence.groups.map((group, i) => (
        <div
          key={group.key}
          role="group"
          aria-labelledby={`${headingId}-g${i}`}
          className="mt-1"
        >
          <div id={`${headingId}-g${i}`} className="font-semibold text-neutral-400">
            {group.label}
          </div>
          {group.rows.length > 0 ? (
            <div className="pl-2">
              <EvidenceRows rows={group.rows} />
            </div>
          ) : (
            <div className="pl-2 text-neutral-500">No signals recorded</div>
          )}
        </div>
      ))}
    </section>
  );
}

function NodePanel({
  heading,
  headingClass,
  nodeId,
  node,
}: {
  heading: string;
  headingClass: string;
  nodeId: number;
  node?: AuditNode;
}) {
  const name = node?.fileName || `Node ${nodeId}`;
  return (
    <div className="rounded border border-neutral-800/60 bg-neutral-950/40 p-3">
      <div className={`mb-2 text-xs font-bold uppercase tracking-wider ${headingClass}`}>
        {heading} (Node #{nodeId})
      </div>
      <div className="mb-2 flex items-start gap-3">
        <Link to={`/assets/${nodeId}`} aria-hidden="true" tabIndex={-1} className="shrink-0">
          <Thumbnail
            assetId={nodeId}
            thumbState={node?.thumbState ?? "PENDING"}
            alt={name}
            className="h-20 w-20 shrink-0 rounded object-cover"
          />
        </Link>
        <div className="min-w-0">
          <Link
            to={`/assets/${nodeId}`}
            className="block truncate text-sm font-semibold text-sky-400 hover:underline"
            title={node?.fileName}
          >
            {name}
          </Link>
          <div className="truncate text-xs text-neutral-500" title={node?.filePath}>
            {node?.filePath || "—"}
          </div>
        </div>
      </div>
      <div className="mt-2 space-y-1 text-xs text-neutral-400">
        <div>
          <span className="text-neutral-500">Captured:</span> {formatTimestamp(node?.capturedAtUnix)}
        </div>
        <div>
          <span className="text-neutral-500">Camera:</span> {node?.cameraModel || "—"}
        </div>
        <div>
          <span className="text-neutral-500">pHash:</span>{" "}
          {node?.phash ? <code className="font-mono text-neutral-300">{node.phash}</code> : "—"}
        </div>
      </div>
    </div>
  );
}

function AuditRow({
  entry,
  selected,
  onToggleSelected,
  onReviewed,
}: {
  entry: EdgeAuditEntry;
  selected: boolean;
  onToggleSelected: (entry: EdgeAuditEntry) => void;
  onReviewed: (decision: RecentDecision) => void;
}) {
  const confirm = useConfirmEdge();
  const reject = useRejectEdge();
  const [rejectOpen, setRejectOpen] = useState(false);
  const busy = confirm.isPending || reject.isPending;

  const record = (action: ReviewAction) => onReviewed({ id: entry.id, action, summary: edgeSummary(entry) });

  return (
    <div
      className={`rounded border bg-neutral-900/50 p-4 ${selected ? "border-indigo-600/70" : "border-neutral-800"}`}
      data-testid={`audit-row-${entry.id}`}
    >
      {/* Top Bar: Edge Meta & Action Buttons */}
      <div className="mb-3 flex items-center justify-between border-b border-neutral-800/80 pb-3">
        <div className="flex flex-wrap items-center gap-2 text-sm">
          <input
            type="checkbox"
            aria-label={`Select edge #${entry.id}`}
            checked={selected}
            onChange={() => onToggleSelected(entry)}
            className="rounded border-neutral-700 bg-neutral-800 text-indigo-500 focus:ring-0"
          />
          <span className="font-semibold text-neutral-200">Edge #{entry.id}</span>
          <span className="rounded bg-indigo-900 px-2 py-0.5 text-xs text-indigo-200 font-medium">
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
            <span className="rounded bg-red-900 px-2 py-0.5 text-xs text-red-200">parent missing</span>
          )}
        </div>
        <div className="flex shrink-0 gap-2">
          <button
            type="button"
            disabled={busy}
            onClick={() => confirm.mutate(entry.id, { onSuccess: () => record("confirm") })}
            className="rounded bg-emerald-700 px-3 py-1.5 text-sm font-medium text-white hover:bg-emerald-600 disabled:opacity-50"
          >
            Confirm
          </button>
          <button
            type="button"
            disabled={busy}
            onClick={() => {
              reject.reset();
              setRejectOpen(true);
            }}
            className="rounded bg-neutral-800 px-3 py-1.5 text-sm font-medium text-neutral-200 hover:bg-neutral-700 disabled:opacity-50"
          >
            Reject
          </button>
        </div>
      </div>

      {confirm.isError && (
        <div className="mb-3 text-xs text-red-400">Failed to confirm edge: {String(confirm.error)}</div>
      )}

      {/* Side-by-side comparison */}
      <div className="grid grid-cols-1 gap-4 md:grid-cols-3">
        <NodePanel heading="Parent" headingClass="text-blue-400" nodeId={entry.sourceNodeId} node={entry.sourceNode} />

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
          <EvidencePanel evidenceJson={entry.evidenceJson} />
        </div>

        <NodePanel heading="Child" headingClass="text-emerald-400" nodeId={entry.targetNodeId} node={entry.targetNode} />
      </div>

      {rejectOpen && (
        <ConfirmDialog
          titleId={`reject-edge-${entry.id}-title`}
          title={`Reject Edge #${entry.id}`}
          body={
            <>
              Reject <strong className="text-neutral-100">{edgeSummary(entry)}</strong>? The edge leaves the queue
              and automated resolvers will not re-propose it. An admin can undo this from{" "}
              <em>Recently reviewed</em> on this page.
            </>
          }
          confirmLabel="Reject Edge"
          pendingLabel="Rejecting…"
          isPending={reject.isPending}
          error={reject.isError ? reject.error : undefined}
          errorLabel="Failed to reject edge"
          onConfirm={() =>
            reject.mutate(entry.id, {
              onSuccess: () => {
                setRejectOpen(false);
                record("reject");
              },
            })
          }
          onCancel={() => setRejectOpen(false)}
        />
      )}
    </div>
  );
}

// RecentlyReviewed lists this session's decisions with an Undo each: the
// reset-review endpoint hands a CONFIRMED/REJECTED edge back to the queue.
// That endpoint is admin-only, so the page renders this only for admins.
// Undo changes the review state only: metadata a confirm already copied
// into the child's file (auto-inherit) stays, and the panel says so.
function RecentlyReviewed({
  decisions,
  onUndone,
  onDismiss,
}: {
  decisions: RecentDecision[];
  onUndone: (id: number) => void;
  onDismiss: () => void;
}) {
  const reset = useResetEdgeReview();
  const [targetId, setTargetId] = useState<number | null>(null);
  // "Undo all" loops the single-edge endpoint (one actor_audit row each)
  // and refreshes once at the end, like the batch confirm/reject.
  const invalidateEdgeReview = useInvalidateEdgeReviewQueries();
  const [undoingAll, setUndoingAll] = useState(false);
  const [undoAllError, setUndoAllError] = useState("");
  if (decisions.length === 0) return null;

  const undoAll = async () => {
    setUndoingAll(true);
    setUndoAllError("");
    let failed = 0;
    for (const d of decisions) {
      try {
        await api.resetEdgeReview(d.id);
        onUndone(d.id);
      } catch {
        failed++;
      }
    }
    setUndoingAll(false);
    invalidateEdgeReview();
    if (failed > 0) setUndoAllError(`${failed} of ${decisions.length} could not be undone.`);
  };
  return (
    <section
      aria-label="Recently reviewed"
      className="rounded-lg border border-neutral-800 bg-neutral-900/80 p-4 text-xs space-y-2"
    >
      <div className="flex items-center justify-between">
        <h2 className="font-semibold text-neutral-300 uppercase tracking-wider text-[11px]">Recently reviewed</h2>
        <div className="flex items-center gap-3">
          {undoAllError && <span className="text-red-400">{undoAllError}</span>}
          {decisions.length > 1 && (
            <button
              type="button"
              disabled={undoingAll || reset.isPending}
              onClick={() => void undoAll()}
              className="rounded border border-neutral-700 bg-neutral-800 px-2 py-0.5 text-neutral-200 hover:bg-neutral-700 disabled:opacity-50"
            >
              {undoingAll ? "Undoing…" : `Undo all (${decisions.length})`}
            </button>
          )}
          <button type="button" onClick={onDismiss} className="text-neutral-500 hover:text-neutral-300">
            Dismiss
          </button>
        </div>
      </div>
      <p className="text-neutral-500">
        Undo returns an edge to the queue. It does not remove metadata a confirm already copied into the file.
      </p>
      <ul className="max-h-48 space-y-1 overflow-y-auto">
        {decisions.map((d) => (
          <li key={d.id} className="flex items-center justify-between gap-3">
            <span className="truncate text-neutral-300">
              <span className={d.action === "confirm" ? "text-emerald-400" : "text-red-400"}>
                {d.action === "confirm" ? "Confirmed" : "Rejected"}
              </span>{" "}
              edge #{d.id}: {d.summary}
            </span>
            <span className="flex shrink-0 items-center gap-2">
              {reset.isError && targetId === d.id && (
                <span className="text-red-400">Undo failed: {String(reset.error)}</span>
              )}
              <button
                type="button"
                aria-label={`Undo review of edge #${d.id}`}
                disabled={reset.isPending || undoingAll}
                onClick={() => {
                  reset.reset();
                  setTargetId(d.id);
                  reset.mutate(d.id, { onSuccess: () => onUndone(d.id) });
                }}
                className="rounded border border-neutral-700 bg-neutral-800 px-2 py-0.5 text-neutral-200 hover:bg-neutral-700 disabled:opacity-50"
              >
                {reset.isPending && targetId === d.id ? "Undoing…" : "Undo"}
              </button>
            </span>
          </li>
        ))}
      </ul>
    </section>
  );
}

function ManualLinkModal({ isOpen, onClose }: { isOpen: boolean; onClose: () => void }) {
  const [sourceId, setSourceId] = useState("");
  const [targetId, setTargetId] = useState("");
  const [relType, setRelType] = useState<EdgeAuditEntry["relationshipType"]>("DERIVED_FROM");
  const [errorMsg, setErrorMsg] = useState("");
  const [isSourcePickerOpen, setIsSourcePickerOpen] = useState(false);
  const [isTargetPickerOpen, setIsTargetPickerOpen] = useState(false);
  const createEdge = useCreateEdge();

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
      <Dialog
        title="Manual Link Edge"
        onClose={onClose}
        dismissible={!createEdge.isPending}
        overlayClassName="z-50 bg-black/60 backdrop-blur-xs"
        titleClassName="mb-4 text-lg font-semibold text-neutral-100"
      >
          {errorMsg && (
            <div className="mb-4 rounded bg-red-900 p-3 text-xs text-red-200 border border-red-800">
              {errorMsg}
            </div>
          )}
          <form onSubmit={handleSubmit} className="space-y-4 text-sm">
            <div>
              <label htmlFor="manual-link-source" className="mb-1 block font-medium text-neutral-300">Parent (Source) Node ID</label>
              <div className="flex gap-2">
                <input
                  id="manual-link-source"
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
              <label htmlFor="manual-link-target" className="mb-1 block font-medium text-neutral-300">Child (Target) Node ID</label>
              <div className="flex gap-2">
                <input
                  id="manual-link-target"
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
              <label htmlFor="manual-link-relationship" className="mb-1 block font-medium text-neutral-300">Relationship Type</label>
              <select
                id="manual-link-relationship"
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
      </Dialog>

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
  action,
  edges,
  onClose,
  onComplete,
}: {
  action: ReviewAction;
  edges: EdgeAuditEntry[];
  onClose: () => void;
  // Called once the run settles, with the ids that succeeded (failed ones
  // stay selected so they can be retried).
  onComplete: (succeeded: EdgeAuditEntry[], failedCount: number) => void;
}) {
  const [running, setRunning] = useState(false);
  const [progress, setProgress] = useState(0);
  const [errorMsg, setErrorMsg] = useState("");
  // The batch calls the API directly and refreshes once at the end: going
  // through useConfirmEdge/useRejectEdge would run the full invalidation
  // fan-out (queue, list, 3 detail keys, unlinked count) after EVERY edge.
  const invalidateEdgeReview = useInvalidateEdgeReviewQueries();

  const handleExecute = async () => {
    setRunning(true);
    setErrorMsg("");
    const succeeded: EdgeAuditEntry[] = [];
    let failed = 0;
    try {
      for (const edge of edges) {
        try {
          if (action === "confirm") {
            await api.confirmEdge(edge.id);
          } else {
            await api.rejectEdge(edge.id);
          }
          succeeded.push(edge);
        } catch {
          failed++;
        }
        setProgress(succeeded.length + failed);
      }
    } finally {
      setRunning(false);
      invalidateEdgeReview();
      if (failed > 0) {
        setErrorMsg(`${failed} of ${edges.length} edges failed to ${action}.`);
      }
      onComplete(succeeded, failed);
    }
  };

  const percent = edges.length > 0 ? Math.round((progress / edges.length) * 100) : 0;
  const verb = action === "confirm" ? "confirm" : "reject";

  return (
    <Dialog
      title={`Batch ${action === "confirm" ? "Confirm" : "Reject"} Edges`}
      onClose={onClose}
      dismissible={!running}
      titleClassName="text-base font-semibold text-neutral-100"
      panelClassName="max-w-md w-full rounded-lg border border-neutral-800 bg-neutral-900 p-6 space-y-4"
    >
        <p className="text-xs text-neutral-300">
          {verb === "confirm" ? "Confirm" : "Reject"} the{" "}
          <strong className="text-neutral-100">{edges.length}</strong> selected edge
          {edges.length === 1 ? "" : "s"}? Only the selected edges are changed, not the rest of the queue. An admin
          can undo each one from <em>Recently reviewed</em> afterwards.
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
              className={`rounded px-3 py-1.5 text-xs font-medium disabled:opacity-50 ${
                action === "confirm"
                  ? "bg-emerald-600 text-white hover:bg-emerald-500"
                  : "bg-red-600 text-white hover:bg-red-500"
              }`}
            >
              {running ? "Processing…" : action === "confirm" ? "Confirm Selected" : "Reject Selected"}
            </button>
          )}
        </div>
    </Dialog>
  );
}

function parseTrail(raw: string | null): number[] {
  if (!raw) return [];
  return raw
    .split(",")
    .map((v) => Number(v))
    .filter((v) => Number.isInteger(v) && v > 0);
}

export default function AuditQueuePage() {
  const [searchParams, setSearchParams] = useSearchParams();

  // Filters live in the URL and are applied server-side, so they cover the
  // whole queue (and its total), not just the loaded page.
  // Every URL filter is checked against the server's bounds (AuditQueueInput:
  // tier 1-3, resolver <= 128 chars, a known relationshipType, minConfidence
  // in (0, 1]) and dropped if outside them: an out-of-range value would 422
  // and leave the page without a filter bar to clear it.
  const tierParam = searchParams.get("tier") || "";
  const tierFilter = ["1", "2", "3"].includes(tierParam) ? tierParam : "";
  const resolverParam = searchParams.get("resolver") || "";
  const resolverFilter = resolverParam.length <= 128 ? resolverParam : "";
  const relParam = searchParams.get("relationshipType") || "";
  const relFilter: EdgeAuditEntry["relationshipType"] | "" =
    RELATIONSHIP_TYPES.find((r) => r.value === relParam)?.value ?? "";
  const minConfParam = searchParams.get("minConfidence") || "";
  const minConfNum = Number(minConfParam);
  const minConfFilter = minConfParam && minConfNum > 0 && minConfNum <= 1 ? minConfParam : "";
  const hasActiveFilters = Boolean(tierFilter || resolverFilter || relFilter || minConfFilter);

  // Keyset paging: `trail` is the stack of cursors (the last id of each
  // previous page) that led here, so Previous pops one instead of only
  // being able to jump back to the first page. A legacy ?beforeId= link is
  // a one-step trail.
  const trail = useMemo(() => {
    const t = parseTrail(searchParams.get("trail"));
    if (t.length > 0) return t;
    return parseTrail(searchParams.get("beforeId"));
  }, [searchParams]);
  const beforeId = trail.length > 0 ? trail[trail.length - 1] : 0;

  const filterParams = useMemo<Omit<AuditQueueParams, "limit" | "beforeId">>(
    () => ({
      tier: tierFilter ? Number(tierFilter) : undefined,
      resolver: resolverFilter || undefined,
      relationshipType: relFilter || undefined,
      minConfidence: minConfFilter ? Number(minConfFilter) : undefined,
    }),
    [tierFilter, resolverFilter, relFilter, minConfFilter]
  );
  const { data, isLoading, isError, isFetching } = useAuditQueue({ limit: PAGE_SIZE, beforeId, ...filterParams });
  const [isModalOpen, setIsModalOpen] = useState(false);
  const [batchAction, setBatchAction] = useState<ReviewAction | null>(null);

  const entries = useMemo(() => data?.entries ?? [], [data?.entries]);
  const total = data?.total ?? 0;
  const hasMore = entries.length === PAGE_SIZE;

  const resolvers = useMemo(() => {
    const fromServer = data?.resolvers ?? Array.from(new Set(entries.map((e) => e.resolver)));
    // Keep a URL-selected resolver selectable even if it has since drained.
    const all = new Set(fromServer);
    if (resolverFilter) all.add(resolverFilter);
    return Array.from(all).sort();
  }, [data?.resolvers, entries, resolverFilter]);

  // Selection spans pages within one filter set (so a batch can cover
  // more than the loaded page) and is cleared when the filters change --
  // a selection made under other filters would no longer be what the
  // batch buttons claim.
  const [selected, setSelected] = useState<Map<number, EdgeAuditEntry>>(() => new Map());
  const filterKey = JSON.stringify(filterParams);
  const [prevFilterKey, setPrevFilterKey] = useState(filterKey);
  if (filterKey !== prevFilterKey) {
    setPrevFilterKey(filterKey);
    setSelected(new Map());
  }
  const selectedEdges = useMemo(() => Array.from(selected.values()), [selected]);
  const pageAllSelected = entries.length > 0 && entries.every((e) => selected.has(e.id));

  const toggleSelected = useCallback((entry: EdgeAuditEntry) => {
    setSelected((prev) => {
      const next = new Map(prev);
      if (next.has(entry.id)) next.delete(entry.id);
      else next.set(entry.id, entry);
      return next;
    });
  }, []);

  const togglePage = () => {
    setSelected((prev) => {
      const next = new Map(prev);
      if (pageAllSelected) entries.forEach((e) => next.delete(e.id));
      else entries.forEach((e) => next.set(e.id, e));
      return next;
    });
  };

  const [selectingAll, setSelectingAll] = useState(false);
  const [selectAllError, setSelectAllError] = useState("");
  const selectAllMatching = async () => {
    setSelectingAll(true);
    setSelectAllError("");
    try {
      const next = new Map<number, EdgeAuditEntry>();
      let cursor = 0;
      while (next.size < SELECT_ALL_CAP) {
        const res = await api.listAuditQueue({
          limit: Math.min(SELECT_ALL_PAGE_SIZE, SELECT_ALL_CAP - next.size),
          beforeId: cursor,
          ...filterParams,
        });
        res.entries.forEach((e) => next.set(e.id, e));
        if (res.entries.length === 0 || next.size >= res.total) break;
        cursor = res.entries[res.entries.length - 1].id;
      }
      setSelected(next);
    } catch (err) {
      setSelectAllError(err instanceof Error ? err.message : String(err));
    } finally {
      setSelectingAll(false);
    }
  };

  // Undo (reset-review) is admin-only; the queue itself is not.
  const { data: me } = useMe();
  const [recent, setRecent] = useState<RecentDecision[]>([]);
  const recordDecisions = useCallback((decisions: RecentDecision[]) => {
    setRecent((prev) => {
      const ids = new Set(decisions.map((d) => d.id));
      return [...decisions, ...prev.filter((d) => !ids.has(d.id))].slice(0, RECENT_LIMIT);
    });
    setSelected((prev) => {
      if (!decisions.some((d) => prev.has(d.id))) return prev;
      const next = new Map(prev);
      decisions.forEach((d) => next.delete(d.id));
      return next;
    });
  }, []);
  const onReviewed = useCallback((d: RecentDecision) => recordDecisions([d]), [recordDecisions]);

  const updateFilters = (updates: Record<string, string>) => {
    const next = new URLSearchParams(searchParams);
    // A filter change starts again from the first page of the new set.
    next.delete("trail");
    next.delete("beforeId");
    Object.entries(updates).forEach(([k, v]) => {
      if (v) next.set(k, v);
      else next.delete(k);
    });
    setSearchParams(next);
  };

  // The page scrolls inside the layout's <main overflow-auto>, not the
  // window, so window.scrollTo was a no-op; scroll the page's own top into
  // view instead.
  const topRef = useRef<HTMLDivElement>(null);
  const goToTrail = (nextTrail: number[]) => {
    const next = new URLSearchParams(searchParams);
    next.delete("beforeId");
    if (nextTrail.length > 0) next.set("trail", nextTrail.join(","));
    else next.delete("trail");
    setSearchParams(next);
    topRef.current?.scrollIntoView?.({ block: "start" });
  };
  const handleFirst = () => goToTrail([]);
  const handlePrev = () => goToTrail(trail.slice(0, -1));
  const handleNext = () => {
    if (entries.length === 0) return;
    goToTrail([...trail, entries[entries.length - 1].id]);
  };

  if (isLoading) return <div className="p-6 text-neutral-400">Loading…</div>;
  if (isError) return <div className="p-6 text-red-400">Failed to load the audit queue.</div>;

  const pageNumber = trail.length + 1;
  const firstOnPage = trail.length * PAGE_SIZE + 1;

  return (
    <div ref={topRef} className="p-6 space-y-6">
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

      {me?.isAdmin && (
        <RecentlyReviewed
          decisions={recent}
          onUndone={(id) => setRecent((prev) => prev.filter((d) => d.id !== id))}
          onDismiss={() => setRecent([])}
        />
      )}

      {(entries.length > 0 || hasActiveFilters) && (
        <div className="space-y-3 rounded-lg border border-neutral-800 bg-neutral-900/80 p-4 text-xs">
          <div className="flex flex-wrap items-end gap-3">
            <div>
              <label htmlFor="tier-filter" className="block text-neutral-400 mb-1">Tier</label>
              <select
                id="tier-filter"
                value={tierFilter}
                onChange={(e) => updateFilters({ tier: e.target.value })}
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
                onChange={(e) => updateFilters({ resolver: e.target.value })}
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
                onChange={(e) => updateFilters({ relationshipType: e.target.value })}
                className="rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 focus:outline-none"
              >
                <option value="">All Relationships</option>
                {RELATIONSHIP_TYPES.map((r) => (
                  <option key={r.value} value={r.value}>{r.label}</option>
                ))}
              </select>
            </div>

            <div>
              <label htmlFor="min-confidence-filter" className="block text-neutral-400 mb-1">Min confidence</label>
              <select
                id="min-confidence-filter"
                value={minConfFilter}
                onChange={(e) => updateFilters({ minConfidence: e.target.value })}
                className="rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 focus:outline-none"
              >
                <option value="">Any</option>
                {MIN_CONFIDENCE_OPTIONS.map((v) => (
                  <option key={v} value={v}>{`≥ ${Math.round(Number(v) * 100)}%`}</option>
                ))}
              </select>
            </div>

            {hasActiveFilters && (
              <button
                type="button"
                onClick={() => updateFilters({ tier: "", resolver: "", relationshipType: "", minConfidence: "" })}
                className="text-xs text-amber-400 hover:text-amber-300 border border-amber-800/60 rounded px-2.5 py-1.5 bg-amber-950/40"
              >
                Clear Filters
              </button>
            )}
          </div>

          {entries.length > 0 && (
            <div className="flex flex-wrap items-center gap-3 border-t border-neutral-800 pt-3">
              <label className="flex items-center gap-2 text-neutral-300">
                <input
                  type="checkbox"
                  checked={pageAllSelected}
                  onChange={togglePage}
                  className="rounded border-neutral-700 bg-neutral-800 text-indigo-500 focus:ring-0"
                />
                Select this page
              </label>
              {total > entries.length && selected.size < Math.min(total, SELECT_ALL_CAP) && (
                <button
                  type="button"
                  disabled={selectingAll}
                  onClick={() => void selectAllMatching()}
                  className="rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1 text-neutral-200 hover:bg-neutral-700 disabled:opacity-50"
                >
                  {selectingAll
                    ? "Selecting…"
                    : total > SELECT_ALL_CAP
                      ? `Select first ${SELECT_ALL_CAP} of ${total} matching`
                      : `Select all ${total} matching`}
                </button>
              )}
              {selectAllError && <span className="text-red-400">Could not select all: {selectAllError}</span>}
              {selected.size > 0 && (
                <div className="ml-auto flex items-center gap-2">
                  <span className="text-indigo-200">
                    {selected.size} selected{hasActiveFilters ? " (filtered)" : ""}
                  </span>
                  <button
                    type="button"
                    onClick={() => setSelected(new Map())}
                    className="rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1 text-neutral-300 hover:bg-neutral-700"
                  >
                    Clear selection
                  </button>
                  <button
                    type="button"
                    onClick={() => setBatchAction("confirm")}
                    className="rounded bg-emerald-700 px-3 py-1.5 text-xs font-medium text-white hover:bg-emerald-600 shadow"
                  >
                    Confirm selected ({selected.size})
                  </button>
                  <button
                    type="button"
                    onClick={() => setBatchAction("reject")}
                    className="rounded bg-neutral-800 px-3 py-1.5 text-xs font-medium text-red-300 hover:bg-neutral-700 border border-red-900/60"
                  >
                    Reject selected ({selected.size})
                  </button>
                </div>
              )}
            </div>
          )}
        </div>
      )}

      {entries.length === 0 ? (
        hasActiveFilters ? (
          <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-8 text-center text-neutral-500">
            No edge candidates match the selected filters.
          </div>
        ) : trail.length > 0 ? (
          <p className="text-neutral-500">
            No more edges past this point.{" "}
            <button type="button" onClick={handleFirst} className="text-sky-400 hover:underline">
              Back to the first page
            </button>
          </p>
        ) : (
          <p className="text-neutral-500">Nothing needs review right now.</p>
        )
      ) : (
        <>
          <div className={`space-y-4 ${isFetching ? "opacity-70" : ""}`}>
            {entries.map((e) => (
              <AuditRow
                key={e.id}
                entry={e}
                selected={selected.has(e.id)}
                onToggleSelected={toggleSelected}
                onReviewed={onReviewed}
              />
            ))}
          </div>

          <div className="flex items-center justify-between pt-4 border-t border-neutral-800 text-xs text-neutral-400 mt-4">
            <div>
              {total} item{total === 1 ? "" : "s"} awaiting review{hasActiveFilters ? " (filtered)" : ""}
              {" · "}showing {firstOnPage}–{firstOnPage + entries.length - 1} (page {pageNumber})
            </div>
            <div className="flex items-center space-x-2">
              <button
                aria-label="First page"
                disabled={trail.length === 0 || isFetching}
                onClick={handleFirst}
                className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1 text-neutral-300 hover:bg-neutral-700 disabled:opacity-40 disabled:hover:bg-neutral-800"
              >
                First
              </button>
              <button
                aria-label="Previous page"
                disabled={trail.length === 0 || isFetching}
                onClick={handlePrev}
                className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1 text-neutral-300 hover:bg-neutral-700 disabled:opacity-40 disabled:hover:bg-neutral-800"
              >
                Previous
              </button>
              <button
                aria-label="Next page"
                // Disabled while a page loads: the cursor comes from the
                // rows on screen, so a second click would repeat it.
                disabled={!hasMore || isFetching}
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
          action={batchAction}
          edges={selectedEdges}
          onClose={() => setBatchAction(null)}
          onComplete={(succeeded, failedCount) => {
            recordDecisions(succeeded.map((e) => ({ id: e.id, action: batchAction, summary: edgeSummary(e) })));
            if (failedCount === 0) setBatchAction(null);
          }}
        />
      )}
    </div>
  );
}
