import { useStorageHealth } from "../hooks/queries";
import type { StorageLocationHealth } from "../api/types";

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB", "TB", "PB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return `${parseFloat((bytes / Math.pow(k, i)).toFixed(1))} ${sizes[i]}`;
}

function LocationGaugeCard({ loc }: { loc: StorageLocationHealth }) {
  const percentUsed = loc.totalBytes > 0 ? Math.min(100, Math.round((loc.usedBytes / loc.totalBytes) * 100)) : 0;

  return (
    <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-5 shadow-sm space-y-4">
      <div className="flex items-start justify-between">
        <div>
          <div className="flex items-center space-x-2">
            <h3 className="text-base font-semibold text-neutral-100">{loc.name}</h3>
            <span className="rounded bg-neutral-800 px-2 py-0.5 text-xs font-mono text-neutral-300">{loc.tier}</span>
          </div>
          <p className="mt-1 text-xs font-mono text-neutral-400 break-all">{loc.rootPath}</p>
        </div>
        <div className="flex flex-wrap gap-1.5 justify-end">
          {!loc.isActive && (
            <span className="rounded bg-neutral-800 px-2 py-0.5 text-xs font-medium text-neutral-400 border border-neutral-700">
              INACTIVE
            </span>
          )}
          {loc.isDegraded ? (
            <span className="rounded bg-red-950 px-2 py-0.5 text-xs font-medium text-red-300 border border-red-800">
              DEGRADED
            </span>
          ) : (
            <span className="rounded bg-emerald-950 px-2 py-0.5 text-xs font-medium text-emerald-300 border border-emerald-800">
              HEALTHY
            </span>
          )}
          {loc.readOnly && (
            <span className="rounded bg-amber-950 px-2 py-0.5 text-xs text-amber-300 border border-amber-800">
              READ-ONLY
            </span>
          )}
          {loc.prunable && (
            <span className="rounded bg-blue-950 px-2 py-0.5 text-xs text-blue-300 border border-blue-800">
              PRUNABLE
            </span>
          )}
        </div>
      </div>

      {loc.isDegraded ? (
        <div className="rounded bg-red-950/50 p-3 text-xs text-red-300 border border-red-900/50">
          <p className="font-semibold">Location Access Failed</p>
          <p className="mt-0.5 font-mono text-neutral-400">{loc.degradedMessage || "Failed to query filesystem stats via statfs"}</p>
        </div>
      ) : (
        <>
          <div>
            <div className="flex justify-between text-xs text-neutral-400 mb-1.5">
              <span>Capacity Used: {percentUsed}%</span>
              <span>
                {formatBytes(loc.usedBytes)} / {formatBytes(loc.totalBytes)}
              </span>
            </div>
            <div className="h-2.5 w-full rounded-full bg-neutral-800 overflow-hidden">
              <div
                className={`h-full transition-all duration-300 ${
                  percentUsed > 90 ? "bg-red-500" : percentUsed > 75 ? "bg-amber-500" : "bg-indigo-500"
                }`}
                style={{ width: `${percentUsed}%` }}
              />
            </div>
          </div>

          <div className="grid grid-cols-3 gap-2 pt-2 border-t border-neutral-800 text-center text-xs">
            <div>
              <span className="block text-neutral-400">Indexed Nodes</span>
              <span className="font-semibold text-neutral-200">{loc.nodeCount.toLocaleString()}</span>
            </div>
            <div>
              <span className="block text-neutral-400">Used Space</span>
              <span className="font-semibold text-neutral-200">{formatBytes(loc.usedBytes)}</span>
            </div>
            <div>
              <span className="block text-neutral-400">Free Space</span>
              <span className="font-semibold text-neutral-200">{formatBytes(loc.freeBytes)}</span>
            </div>
          </div>
        </>
      )}
    </div>
  );
}

export default function StorageHealthPage() {
  const { data: health, isLoading, isError } = useStorageHealth();

  if (isLoading) {
    return <div className="p-6 text-neutral-400">Loading storage health metrics…</div>;
  }

  if (isError || !health) {
    return <div className="p-6 text-red-400">Failed to load storage health metrics.</div>;
  }

  const { locations, queues } = health;

  return (
    <div className="p-6 space-y-8 max-w-6xl">
      <div>
        <h1 className="text-2xl font-bold text-neutral-100">Storage Health</h1>
        <p className="mt-1 text-sm text-neutral-400">
          Real-time storage capacity across Tiers 1–3 and background processing queue depths.
        </p>
      </div>

      <section className="space-y-4">
        <h2 className="text-lg font-semibold text-neutral-200">Storage Tier Capacity</h2>
        {locations.length === 0 ? (
          <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-6 text-center text-neutral-400">
            No storage locations registered.
          </div>
        ) : (
          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            {locations.map((loc) => (
              <LocationGaugeCard key={loc.id} loc={loc} />
            ))}
          </div>
        )}
      </section>

      <section className="space-y-4">
        <h2 className="text-lg font-semibold text-neutral-200">Queue & Worker Pool Status</h2>
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
          <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
            <span className="text-xs text-neutral-400 font-medium uppercase tracking-wider block">
              In-Flight Worker Jobs
            </span>
            <span className="mt-2 text-2xl font-bold text-indigo-400 block">
              {queues.workerPoolInFlight}
            </span>
            <span className="text-xs text-neutral-400 mt-1 block">Active background hashing</span>
          </div>

          <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
            <span className="text-xs text-neutral-400 font-medium uppercase tracking-wider block">
              Queued Worker Jobs
            </span>
            <span className="mt-2 text-2xl font-bold text-amber-400 block">
              {queues.workerPoolQueued} / {queues.workerPoolCapacity || "—"}
            </span>
            <span className="text-xs text-neutral-400 mt-1 block">Pending hash queue depth</span>
          </div>

          <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
            <span className="text-xs text-neutral-400 font-medium uppercase tracking-wider block">
              Worker Threads
            </span>
            <span className="mt-2 text-2xl font-bold text-emerald-400 block">
              {queues.workerCount || "—"}
            </span>
            <span className="text-xs text-neutral-400 mt-1 block">Configured worker pool size</span>
          </div>

          <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
            <span className="text-xs text-neutral-400 font-medium uppercase tracking-wider block">
              Running Scan Jobs
            </span>
            <span className="mt-2 text-2xl font-bold text-blue-400 block">
              {queues.runningScanJobs}
            </span>
            <span className="text-xs text-neutral-400 mt-1 block">Active pipeline scans</span>
          </div>
        </div>
      </section>
    </div>
  );
}
