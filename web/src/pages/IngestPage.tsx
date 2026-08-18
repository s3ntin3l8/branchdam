import { useState } from "react";
import { useProgress, useStartScan, useStorageLocations } from "../hooks/queries";

export default function IngestPage() {
  const { data, isLoading, isError, error } = useStorageLocations();
  const [selected, setSelected] = useState<number | "">("");
  const startScan = useStartScan();
  const progress = useProgress(10);

  const locations = data?.locations ?? [];
  const hasReadOnly = locations.some((l) => l.readOnly);

  return (
    <div className="p-6">
      <h1 className="mb-4 text-xl font-semibold">Ingest</h1>

      <div className="mb-2 flex max-w-xl items-center gap-3 rounded border border-neutral-800 p-4">
        <select
          value={selected}
          disabled={isLoading || startScan.isPending}
          onChange={(e) => setSelected(e.target.value ? Number(e.target.value) : "")}
          className="flex-1 rounded bg-neutral-900 px-3 py-2 text-sm"
        >
          <option value="">{isLoading ? "Loading locations…" : "Choose a storage location"}</option>
          {locations.map((l) => (
            <option key={l.id} value={l.id}>
              {l.name} · {l.tier}
              {l.readOnly ? " · read-only" : ""}
            </option>
          ))}
        </select>
        <button
          type="button"
          disabled={selected === "" || startScan.isPending}
          onClick={() => selected !== "" && startScan.mutate(selected)}
          className="rounded bg-sky-700 px-4 py-2 text-sm font-medium text-white hover:bg-sky-600 disabled:opacity-50"
        >
          {startScan.isPending ? "Starting…" : "Scan"}
        </button>
      </div>
      {startScan.isError && (
        <p className="mb-4 text-sm text-red-400">Failed to start scan: {String(startScan.error)}</p>
      )}
      {isError && (
        <p className="mb-4 text-sm text-red-400">Failed to load storage locations: {String(error)}</p>
      )}
      {hasReadOnly && (
        <p className="mb-4 text-xs text-neutral-500">
          Read-only locations (e.g. TIER3_MASTER_ARCHIVE) can be scanned, but no writes are ever made to them.
        </p>
      )}

      <h2 className="mb-2 text-lg font-semibold">Recent jobs</h2>
      {progress.isLoading ? (
        <p className="text-neutral-500">Loading…</p>
      ) : progress.isError ? (
        <p className="text-red-400">Failed to load recent scan jobs.</p>
      ) : (progress.data?.jobs.length ?? 0) === 0 ? (
        <p className="text-neutral-500">No scans yet.</p>
      ) : (
        <table className="w-full text-left text-sm">
          <thead className="border-b border-neutral-800 text-neutral-400">
            <tr>
              <th className="py-2 pr-4">Kind</th>
              <th className="py-2 pr-4">State</th>
              <th className="py-2 pr-4 text-right">Seen</th>
              <th className="py-2 pr-4 text-right">Hashed</th>
              <th className="py-2 pr-4 text-right">Failed</th>
              <th className="py-2 pr-4 text-right">Edges</th>
              <th className="py-2 pr-4">Last error</th>
            </tr>
          </thead>
          <tbody>
            {progress.data?.jobs.map((j) => (
              <tr key={j.id} className="border-b border-neutral-900">
                <td className="py-2 pr-4">{j.kind}</td>
                <td
                  className={`py-2 pr-4 ${
                    j.state === "FAILED" ? "text-red-400" : j.state === "RUNNING" ? "text-amber-400" : "text-neutral-300"
                  }`}
                >
                  {j.state}
                </td>
                <td className="py-2 pr-4 text-right">{j.filesSeen}</td>
                <td className="py-2 pr-4 text-right">{j.filesHashed}</td>
                <td className="py-2 pr-4 text-right">{j.filesFailed}</td>
                <td className="py-2 pr-4 text-right">{j.edgesCreated}</td>
                <td className="py-2 pr-4 font-mono text-xs text-neutral-500">{j.lastError ?? "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
