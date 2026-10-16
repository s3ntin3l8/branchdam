import { useEffect, useState } from "react";
import { useAuditEntries, useMe, useUsers } from "../hooks/queries";
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

// dayStartUnix turns a yyyy-mm-dd date input into unix seconds at local
// midnight that day, plus `addDays`. The server's untilUnix is exclusive,
// so an inclusive "to" date is sent as the next day's midnight.
function dayStartUnix(date: string, addDays = 0): number | undefined {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(date);
  if (!m) return undefined;
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]) + addDays);
  return Number.isNaN(d.getTime()) ? undefined : Math.floor(d.getTime() / 1000);
}

interface TextFilters {
  event: string;
  resourceType: string;
  resourceId: string;
}

const EMPTY_TEXT_FILTERS: TextFilters = { event: "", resourceType: "", resourceId: "" };

export default function AuditLogPage() {
  const [type, setType] = useState<"activity" | "login">("activity");
  const [page, setPage] = useState(1);
  // Text inputs are debounced together so typing doesn't fire a request
  // per keystroke; the selects and dates apply immediately.
  const [text, setText] = useState<TextFilters>(EMPTY_TEXT_FILTERS);
  const [debouncedText, setDebouncedText] = useState<TextFilters>(EMPTY_TEXT_FILTERS);
  const [actorUserId, setActorUserId] = useState("");
  const [sinceDate, setSinceDate] = useState("");
  const [untilDate, setUntilDate] = useState("");

  useEffect(() => {
    const t = setTimeout(() => setDebouncedText(text), 250);
    return () => clearTimeout(t);
  }, [text]);

  const { data: me, isLoading: isMeLoading } = useMe();
  const isAdmin = Boolean(me?.isAdmin);
  const { data: usersData } = useUsers({ limit: 200 }, { enabled: isAdmin });
  const users = usersData?.users ?? [];

  const offset = (page - 1) * PAGE_SIZE;
  const sinceUnix = dayStartUnix(sinceDate);
  const untilUnix = dayStartUnix(untilDate, 1);
  const rangeInvalid = sinceUnix !== undefined && untilUnix !== undefined && untilUnix <= sinceUnix;

  const { data, isPending, isError, error, isFetching, isPlaceholderData } = useAuditEntries(
    {
      type,
      limit: PAGE_SIZE,
      offset,
      event: debouncedText.event.trim() || undefined,
      // Every login entry has the same resource type, so the stream
      // hides that filter; don't let a value typed for activity linger.
      resourceType: (type === "activity" && debouncedText.resourceType.trim()) || undefined,
      resourceId: debouncedText.resourceId.trim() || undefined,
      actorUserId: actorUserId ? Number(actorUserId) : undefined,
      sinceUnix,
      untilUnix,
    },
    isAdmin && !rangeInvalid,
  );

  const entries = data?.entries ?? [];
  const total = data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  const hasActiveFilters = Boolean(
    text.event.trim() || (type === "activity" && text.resourceType.trim()) || text.resourceId.trim() || actorUserId || sinceDate || untilDate,
  );

  const setTextFilter = (key: keyof TextFilters, value: string) => {
    setText((prev) => ({ ...prev, [key]: value }));
    setPage(1);
  };

  const clearFilters = () => {
    setText(EMPTY_TEXT_FILTERS);
    setDebouncedText(EMPTY_TEXT_FILTERS);
    setActorUserId("");
    setSinceDate("");
    setUntilDate("");
    setPage(1);
  };

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

  const inputClass =
    "rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-neutral-200 placeholder:text-neutral-500 focus:outline-none focus:border-neutral-500";
  const isLogin = type === "login";

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
      <div className="space-y-3 rounded-lg border border-neutral-800 bg-neutral-900/80 p-4 text-xs">
        <div className="flex flex-wrap items-end gap-4">
          <div>
            <label htmlFor="log-type-select" className="block text-neutral-400 mb-1">Log Stream</label>
            <select
              id="log-type-select"
              value={type}
              onChange={(e) => {
                setType(e.target.value as "activity" | "login");
                setPage(1);
              }}
              className={inputClass}
            >
              <option value="activity">Admin & System Activity</option>
              <option value="login">Authentication & Logins</option>
            </select>
          </div>

          <div>
            <label htmlFor="log-actor-filter" className="block text-neutral-400 mb-1">Actor</label>
            <select
              id="log-actor-filter"
              value={actorUserId}
              onChange={(e) => {
                setActorUserId(e.target.value);
                setPage(1);
              }}
              className={`${inputClass} min-w-[160px]`}
            >
              <option value="">Anyone</option>
              {users.map((u) => (
                <option key={u.id} value={String(u.id)}>
                  {u.username}
                </option>
              ))}
            </select>
          </div>

          <div>
            <label htmlFor="log-since-filter" className="block text-neutral-400 mb-1">From</label>
            <input
              id="log-since-filter"
              type="date"
              value={sinceDate}
              onChange={(e) => {
                setSinceDate(e.target.value);
                setPage(1);
              }}
              className={inputClass}
            />
          </div>

          <div>
            <label htmlFor="log-until-filter" className="block text-neutral-400 mb-1">To</label>
            <input
              id="log-until-filter"
              type="date"
              value={untilDate}
              onChange={(e) => {
                setUntilDate(e.target.value);
                setPage(1);
              }}
              aria-invalid={rangeInvalid}
              className={inputClass}
            />
          </div>
        </div>

        <div className="flex flex-wrap items-end justify-between gap-4">
          <div className="flex flex-wrap items-end gap-4">
            <div>
              <label htmlFor="log-event-filter" className="block text-neutral-400 mb-1">
                {isLogin ? "Filter Event (outcome, exact)" : "Filter Event (exact)"}
              </label>
              <input
                id="log-event-filter"
                type="text"
                placeholder={isLogin ? "e.g. ok, bad-password" : "e.g. scan.started, asset.archived"}
                value={text.event}
                onChange={(e) => setTextFilter("event", e.target.value)}
                className={`${inputClass} min-w-[200px]`}
              />
            </div>

            {!isLogin && (
              <div>
                <label htmlFor="log-resource-filter" className="block text-neutral-400 mb-1">Filter Resource Type</label>
                <input
                  id="log-resource-filter"
                  type="text"
                  placeholder="e.g. asset, scan_job"
                  value={text.resourceType}
                  onChange={(e) => setTextFilter("resourceType", e.target.value)}
                  className={`${inputClass} min-w-[160px]`}
                />
              </div>
            )}

            <div>
              <label htmlFor="log-resource-id-filter" className="block text-neutral-400 mb-1">
                {isLogin ? "Login Source" : "Resource ID"}
              </label>
              <input
                id="log-resource-id-filter"
                type="text"
                placeholder={isLogin ? "e.g. local" : "e.g. 42"}
                value={text.resourceId}
                onChange={(e) => setTextFilter("resourceId", e.target.value)}
                className={`${inputClass} min-w-[120px]`}
              />
            </div>

            {hasActiveFilters && (
              <button
                type="button"
                onClick={clearFilters}
                className="text-xs text-amber-400 hover:text-amber-300 border border-amber-800/60 rounded px-2.5 py-1.5 bg-amber-950/40"
              >
                Clear Filters
              </button>
            )}
          </div>

          {/* No total while the range is invalid: it would be the last
              range's count, for a query that was never sent. */}
          {!rangeInvalid && (
            <div className="text-neutral-400" aria-live="polite">
              {isFetching && isPlaceholderData ? "Updating… " : null}
              Total: <strong className="text-neutral-200">{total}</strong> records
            </div>
          )}
        </div>
        {rangeInvalid && (
          <p role="alert" className="text-red-400">
            The "To" date must not be before the "From" date.
          </p>
        )}
      </div>

      {/* TABLE */}
      {/* While the range is invalid the query is disabled, so there is no
          result for it: show only the range alert above, not an empty state
          (or the last range's rows, kept as placeholder data) that would
          claim something about a request never sent.
          isPending, not isLoading: the query is disabled until /me says
          admin, and a disabled query isn't "loading" -- the page used to
          flash "No audit entries" in that window. */}
      {rangeInvalid ? null : isMeLoading || isPending ? (
        <div className="p-6 text-neutral-400">Loading audit log…</div>
      ) : isError ? (
        <div className="p-6 text-red-400">Failed to load audit entries: {String(error)}</div>
      ) : entries.length === 0 ? (
        <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-8 text-center text-neutral-500">
          {hasActiveFilters ? "No audit entries match the specified filters." : "No audit entries recorded for this stream."}
        </div>
      ) : (
        <>
          <div className="overflow-x-auto">
          <table className="w-full min-w-[720px] text-left text-sm">
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
          </div>

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
