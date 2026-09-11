import { useEffect, useState } from "react";
import { useAuditEntries, useMe } from "../hooks/queries";
import type { AuditEntry } from "../api/types";

const PAGE_SIZE = 50;

function formatTimestamp(unix?: number): string {
  if (!unix) return "—";
  return new Date(unix * 1000).toLocaleString();
}

function DetailsCell({ detailsJson }: { detailsJson?: string }) {
  const [expanded, setExpanded] = useState(false);
  if (!detailsJson || detailsJson === "{}" || detailsJson === "") {
    return <span className="text-neutral-500">—</span>;
  }

  let pretty = detailsJson;
  try {
    pretty = JSON.stringify(JSON.parse(detailsJson), null, 2);
  } catch {
    // Keep raw string on JSON parse error
  }

  return (
    <div>
      <button
        type="button"
        onClick={() => setExpanded(!expanded)}
        className="text-xs text-indigo-400 hover:text-indigo-300 underline font-mono"
      >
        {expanded ? "Hide Details" : "View Details"}
      </button>
      {expanded && (
        <pre className="mt-2 max-h-48 overflow-auto rounded bg-neutral-950 p-2 font-mono text-[11px] text-neutral-300 border border-neutral-800">
          {pretty}
        </pre>
      )}
    </div>
  );
}

export default function AuditLogPage() {
  const [type, setType] = useState<"activity" | "login">("activity");
  const [page, setPage] = useState(1);
  const [eventFilter, setEventFilter] = useState("");
  const [resourceTypeFilter, setResourceTypeFilter] = useState("");
  const [debouncedEventFilter, setDebouncedEventFilter] = useState("");
  const [debouncedResourceTypeFilter, setDebouncedResourceTypeFilter] = useState("");

  useEffect(() => {
    const t = setTimeout(() => {
      setDebouncedEventFilter(eventFilter);
    }, 250);
    return () => clearTimeout(t);
  }, [eventFilter]);

  useEffect(() => {
    const t = setTimeout(() => {
      setDebouncedResourceTypeFilter(resourceTypeFilter);
    }, 250);
    return () => clearTimeout(t);
  }, [resourceTypeFilter]);

  const { data: me, isLoading: isMeLoading } = useMe();
  const isAdmin = Boolean(me?.isAdmin);

  const offset = (page - 1) * PAGE_SIZE;

  const { data, isLoading, isError, error } = useAuditEntries({
    type,
    limit: PAGE_SIZE,
    offset,
    event: debouncedEventFilter.trim() || undefined,
    resourceType: debouncedResourceTypeFilter.trim() || undefined,
  }, isAdmin);

  const entries = data?.entries ?? [];
  const total = data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  const hasActiveFilters = Boolean(eventFilter.trim() || resourceTypeFilter.trim());

  if (!isMeLoading && !isAdmin) {
    return (
      <div className="p-6">
        <div className="rounded-lg border border-neutral-800 bg-neutral-900/80 p-8 text-center text-sm text-neutral-400">
          <p className="font-medium text-neutral-200 mb-1">Access Restricted</p>
          <p className="text-xs">Administrator privileges are required to view the audit log.</p>
        </div>
      </div>
    );
  }

  return (
    <div className="p-6 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-xl font-semibold text-neutral-100">Audit Log</h1>
          <p className="text-xs text-neutral-400 mt-0.5">
            Immutable log of server activity, administrative changes, and authentication events.
          </p>
        </div>
      </div>

      {/* FILTER CONTROLS */}
      <div className="flex flex-wrap items-center justify-between gap-4 rounded-lg border border-neutral-800 bg-neutral-900/80 p-4 text-xs">
        <div className="flex flex-wrap items-center gap-4">
          <div>
            <label htmlFor="log-type-select" className="block text-neutral-400 mb-1">Log Stream</label>
            <select
              id="log-type-select"
              value={type}
              onChange={(e) => {
                setType(e.target.value as "activity" | "login");
                setPage(1);
              }}
              className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-neutral-200 focus:outline-none focus:border-neutral-500"
            >
              <option value="activity">Admin & System Activity</option>
              <option value="login">Authentication & Logins</option>
            </select>
          </div>

          <div>
            <label htmlFor="log-event-filter" className="block text-neutral-400 mb-1">Filter Event</label>
            <input
              id="log-event-filter"
              type="text"
              placeholder="e.g. scan.started, asset.archived"
              value={eventFilter}
              onChange={(e) => {
                setEventFilter(e.target.value);
                setPage(1);
              }}
              className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-neutral-200 placeholder:text-neutral-500 focus:outline-none focus:border-neutral-500 min-w-[200px]"
            />
          </div>

          <div>
            <label htmlFor="log-resource-filter" className="block text-neutral-400 mb-1">Filter Resource Type</label>
            <input
              id="log-resource-filter"
              type="text"
              placeholder="e.g. asset, scan_job"
              value={resourceTypeFilter}
              onChange={(e) => {
                setResourceTypeFilter(e.target.value);
                setPage(1);
              }}
              className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-neutral-200 placeholder:text-neutral-500 focus:outline-none focus:border-neutral-500 min-w-[160px]"
            />
          </div>

          {hasActiveFilters && (
            <button
              type="button"
              onClick={() => {
                setEventFilter("");
                setDebouncedEventFilter("");
                setResourceTypeFilter("");
                setDebouncedResourceTypeFilter("");
                setPage(1);
              }}
              className="self-end text-xs text-amber-400 hover:text-amber-300 border border-amber-800/60 rounded px-2.5 py-1.5 bg-amber-950/40"
            >
              Clear Filters
            </button>
          )}
        </div>

        <div className="text-neutral-400">
          Total: <strong className="text-neutral-200">{total}</strong> records
        </div>
      </div>

      {/* TABLE */}
      {isLoading ? (
        <div className="p-6 text-neutral-400">Loading audit log…</div>
      ) : isError ? (
        <div className="p-6 text-red-400">Failed to load audit entries: {String(error)}</div>
      ) : entries.length === 0 ? (
        <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-8 text-center text-neutral-500">
          {hasActiveFilters ? "No audit entries match the specified filters." : "No audit entries recorded for this stream."}
        </div>
      ) : (
        <>
          <table className="w-full text-left text-sm">
            <thead className="border-b border-neutral-800 text-neutral-400">
              <tr>
                <th className="py-2 pr-4">Timestamp</th>
                <th className="py-2 pr-4">Actor</th>
                <th className="py-2 pr-4">Event</th>
                <th className="py-2 pr-4">Resource</th>
                <th className="py-2">Details</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-900">
              {entries.map((e: AuditEntry) => (
                <tr key={e.id} className="hover:bg-neutral-900/50">
                  <td className="py-2 pr-4 font-mono text-xs text-neutral-400 whitespace-nowrap">
                    {formatTimestamp(e.createdAt)}
                  </td>
                  <td className="py-2 pr-4 text-xs">
                    <span className="font-medium text-neutral-200">{e.actorName || "—"}</span>
                    <span className="ml-1.5 rounded bg-neutral-800 px-1.5 py-0.5 text-[10px] text-neutral-400 font-mono">
                      {e.actorKind}
                    </span>
                  </td>
                  <td className="py-2 pr-4 text-xs font-mono text-indigo-300 font-semibold">
                    {e.event}
                  </td>
                  <td className="py-2 pr-4 text-xs font-mono text-neutral-400">
                    {e.resourceType ? (
                      <span>
                        <span className="text-neutral-500">{e.resourceType}: </span>
                        {e.resourceId || "—"}
                      </span>
                    ) : (
                      "—"
                    )}
                  </td>
                  <td className="py-2 text-xs">
                    <DetailsCell detailsJson={e.detailsJson} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>

          {/* PAGINATION */}
          <div className="flex items-center justify-between pt-4 border-t border-neutral-800 text-xs text-neutral-400">
            <div>
              Page {page} of {totalPages}
            </div>
            <div className="flex items-center space-x-2">
              <button
                type="button"
                aria-label="First page"
                disabled={page === 1}
                onClick={() => setPage(1)}
                className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1 text-neutral-300 hover:bg-neutral-700 disabled:opacity-40 disabled:hover:bg-neutral-800"
              >
                First
              </button>
              <button
                type="button"
                aria-label="Previous page"
                disabled={page === 1}
                onClick={() => setPage((p) => Math.max(1, p - 1))}
                className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1 text-neutral-300 hover:bg-neutral-700 disabled:opacity-40 disabled:hover:bg-neutral-800"
              >
                Previous
              </button>
              <button
                type="button"
                aria-label="Next page"
                disabled={page >= totalPages}
                onClick={() => setPage((p) => Math.min(totalPages, p + 1))}
                className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1 text-neutral-300 hover:bg-neutral-700 disabled:opacity-40 disabled:hover:bg-neutral-800"
              >
                Next
              </button>
            </div>
          </div>
        </>
      )}
    </div>
  );
}
