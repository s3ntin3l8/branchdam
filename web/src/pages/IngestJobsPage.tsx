import { useSearchParams } from "react-router";
import { useJobs } from "../hooks/queries";
import type { ScanJob } from "../api/types";

const kindBadge: Record<ScanJob["kind"], string> = {
  FULL_SCAN: "bg-indigo-950 text-indigo-300 border-indigo-800",
  INCREMENTAL: "bg-amber-950 text-amber-300 border-amber-800",
  WATCH: "bg-purple-950 text-purple-300 border-purple-800",
};

const stateBadge: Record<ScanJob["state"], string> = {
  RUNNING: "bg-blue-950 text-blue-300 border-blue-800 animate-pulse",
  COMPLETED: "bg-emerald-950 text-emerald-300 border-emerald-800",
  FAILED: "bg-red-950 text-red-300 border-red-800",
  CANCELLED: "bg-neutral-800 text-neutral-400 border-neutral-700",
};

const PAGE_SIZE = 25;

export default function IngestJobsPage() {
  const [searchParams, setSearchParams] = useSearchParams();

  const kind = (searchParams.get("kind") as ScanJob["kind"]) || "";
  const state = (searchParams.get("state") as ScanJob["state"]) || "";
  const page = Math.max(1, Number(searchParams.get("page") || "1"));

  const offset = (page - 1) * PAGE_SIZE;

  const { data, isLoading, isError, error } = useJobs({
    limit: PAGE_SIZE,
    offset,
    kind: kind || undefined,
    state: state || undefined,
  });

  const jobs = data?.jobs ?? [];
  const total = data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  const updateFilters = (updates: Record<string, string | null>) => {
    const nextParams = new URLSearchParams(searchParams);
    nextParams.set("page", "1");
    Object.entries(updates).forEach(([key, value]) => {
      if (value === null || value === "") {
        nextParams.delete(key);
      } else {
        nextParams.set(key, value);
      }
    });
    setSearchParams(nextParams);
  };

  const handlePageChange = (newPage: number) => {
    const nextParams = new URLSearchParams(searchParams);
    nextParams.set("page", String(newPage));
    setSearchParams(nextParams);
  };

  const clearFilters = () => {
    setSearchParams(new URLSearchParams());
  };

  const hasActiveFilters = Boolean(kind || state);

  return (
    <div className="p-6 space-y-6 max-w-6xl">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-xl font-semibold text-neutral-100">Ingest Jobs History</h1>
          <p className="text-xs text-neutral-400 mt-0.5">
            Complete audit record of background filesystem scans and index jobs
          </p>
        </div>

        {hasActiveFilters && (
          <button
            onClick={clearFilters}
            className="text-xs text-amber-400 hover:text-amber-300 border border-amber-800/60 rounded px-2.5 py-1 bg-amber-950/40"
          >
            Clear Filters
          </button>
        )}
      </div>

      {/* FILTER BAR */}
      <div className="rounded-lg border border-neutral-800 bg-neutral-900/80 p-4 text-xs space-y-2">
        <div className="font-semibold text-neutral-300 uppercase tracking-wider text-[11px]">Filter Jobs</div>
        <div className="flex flex-wrap gap-4 items-center">
          <div>
            <label htmlFor="kind-filter" className="block text-neutral-400 mb-1">Scan Kind</label>
            <select
              id="kind-filter"
              value={kind}
              onChange={(e) => updateFilters({ kind: e.target.value })}
              className="rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 focus:outline-none focus:border-neutral-500 min-w-[140px]"
            >
              <option value="">All Kinds</option>
              <option value="FULL_SCAN">FULL_SCAN</option>
              <option value="INCREMENTAL">INCREMENTAL</option>
              <option value="WATCH">WATCH</option>
            </select>
          </div>

          <div>
            <label htmlFor="state-filter" className="block text-neutral-400 mb-1">State</label>
            <select
              id="state-filter"
              value={state}
              onChange={(e) => updateFilters({ state: e.target.value })}
              className="rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 focus:outline-none focus:border-neutral-500 min-w-[140px]"
            >
              <option value="">All States</option>
              <option value="RUNNING">RUNNING</option>
              <option value="COMPLETED">COMPLETED</option>
              <option value="FAILED">FAILED</option>
              <option value="CANCELLED">CANCELLED</option>
            </select>
          </div>
        </div>
      </div>

      {isLoading ? (
        <div className="p-6 text-neutral-400">Loading scan jobs history…</div>
      ) : isError ? (
        <div className="p-6 text-red-400">Failed to load scan jobs: {String(error)}</div>
      ) : jobs.length === 0 ? (
        <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-8 text-center text-neutral-500">
          No scan jobs found.
        </div>
      ) : (
        <>
          <div className="rounded-lg border border-neutral-800 overflow-hidden">
            <table className="w-full text-left text-sm">
              <thead className="border-b border-neutral-800 bg-neutral-900/60 text-neutral-400 text-xs">
                <tr>
                  <th className="py-2.5 px-4 font-semibold">Job ID</th>
                  <th className="py-2.5 px-4 font-semibold">Kind</th>
                  <th className="py-2.5 px-4 font-semibold">State</th>
                  <th className="py-2.5 px-4 font-semibold text-right">Files Seen</th>
                  <th className="py-2.5 px-4 font-semibold text-right">Files Hashed</th>
                  <th className="py-2.5 px-4 font-semibold text-right">Failed</th>
                  <th className="py-2.5 px-4 font-semibold text-right">Edges Created</th>
                  <th className="py-2.5 px-4 font-semibold">Error / Details</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-neutral-900">
                {jobs.map((j) => (
                  <tr key={j.id} className="hover:bg-neutral-900/50 text-xs">
                    <td className="py-2.5 px-4 font-mono font-medium text-neutral-300">#{j.id}</td>
                    <td className="py-2.5 px-4">
                      <span className={`inline-block rounded px-2 py-0.5 font-mono text-[11px] border ${kindBadge[j.kind] || "bg-neutral-800 text-neutral-300"}`}>
                        {j.kind}
                      </span>
                    </td>
                    <td className="py-2.5 px-4">
                      <span className={`inline-block rounded px-2 py-0.5 font-mono text-[11px] border ${stateBadge[j.state] || "bg-neutral-800 text-neutral-300"}`}>
                        {j.state}
                      </span>
                    </td>
                    <td className="py-2.5 px-4 text-right font-mono text-neutral-300">{j.filesSeen.toLocaleString()}</td>
                    <td className="py-2.5 px-4 text-right font-mono text-neutral-300">{j.filesHashed.toLocaleString()}</td>
                    <td className="py-2.5 px-4 text-right font-mono text-red-400">{j.filesFailed > 0 ? j.filesFailed.toLocaleString() : "0"}</td>
                    <td className="py-2.5 px-4 text-right font-mono text-emerald-400">{j.edgesCreated.toLocaleString()}</td>
                    <td className="py-2.5 px-4 font-mono text-red-400 max-w-xs truncate">
                      {j.lastError || "—"}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>

          {/* PAGINATION CONTROLS */}
          <div className="flex items-center justify-between pt-2 text-xs text-neutral-400">
            <div>
              Showing {Math.min(offset + 1, total)} to {Math.min(offset + jobs.length, total)} of {total} jobs
            </div>
            <div className="flex items-center space-x-2">
              <button
                disabled={page <= 1}
                onClick={() => handlePageChange(page - 1)}
                className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1 text-neutral-300 hover:bg-neutral-700 disabled:opacity-40 disabled:hover:bg-neutral-800"
              >
                Previous
              </button>
              <span className="px-2 font-mono">
                Page {page} of {totalPages}
              </span>
              <button
                disabled={page >= totalPages}
                onClick={() => handlePageChange(page + 1)}
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
