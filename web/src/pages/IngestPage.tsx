import { useId, useRef, useState, type KeyboardEvent } from "react";
import { useProgress, useStartScan, useStorageLocations } from "../hooks/queries";
import ManualUploadZone from "../components/ManualUploadZone";

type IngestTab = "upload" | "scan";

const INGEST_TABS: { id: IngestTab; label: string }[] = [
  { id: "upload", label: "Manual Web Upload" },
  { id: "scan", label: "Storage Scan" },
];

export default function IngestPage() {
  const [activeTab, setActiveTab] = useState<IngestTab>("upload");
  const baseId = useId();
  const tabId = (tab: IngestTab) => `${baseId}-tab-${tab}`;
  const panelId = (tab: IngestTab) => `${baseId}-panel-${tab}`;
  const tabRefs = useRef<(HTMLButtonElement | null)[]>([]);

  // Arrow keys, Home and End move between tabs and select the one they
  // land on (automatic activation); Tab leaves the tablist for the panel.
  const handleTabKeyDown = (e: KeyboardEvent<HTMLButtonElement>, index: number) => {
    const last = INGEST_TABS.length - 1;
    let next: number;
    switch (e.key) {
      case "ArrowRight":
        next = index === last ? 0 : index + 1;
        break;
      case "ArrowLeft":
        next = index === 0 ? last : index - 1;
        break;
      case "Home":
        next = 0;
        break;
      case "End":
        next = last;
        break;
      default:
        return;
    }
    e.preventDefault();
    setActiveTab(INGEST_TABS[next].id);
    tabRefs.current[next]?.focus();
  };
  const { data, isLoading, isError, error } = useStorageLocations();
  const [selected, setSelected] = useState<number | "">("");
  const [differential, setDifferential] = useState(false);
  const startScan = useStartScan();
  const progress = useProgress(10);

  const locations = data?.locations ?? [];
  const hasReadOnly = locations.some((l) => l.readOnly);
  const selectedLocation = selected === "" ? undefined : locations.find((l) => l.id === selected);
  const isTier3 = selectedLocation?.tier === "TIER3_MASTER_ARCHIVE";

  return (
    <div className="p-6">
      <h1 className="mb-4 text-xl font-semibold text-neutral-100">Ingest</h1>

      {/* Panel switcher: the WAI-ARIA tabs pattern. Only the active
          panel is mounted, so only the selected tab carries aria-controls
          (an inactive tab's would point at an id nothing carries). */}
      <div role="tablist" aria-label="Ingest method" className="mb-6 flex border-b border-neutral-800">
        {INGEST_TABS.map((tab, i) => {
          const selected = activeTab === tab.id;
          return (
            <button
              key={tab.id}
              ref={(el) => {
                tabRefs.current[i] = el;
              }}
              type="button"
              role="tab"
              id={tabId(tab.id)}
              aria-selected={selected}
              aria-controls={selected ? panelId(tab.id) : undefined}
              tabIndex={selected ? 0 : -1}
              onClick={() => setActiveTab(tab.id)}
              onKeyDown={(e) => handleTabKeyDown(e, i)}
              className={`border-b-2 px-4 py-2 text-sm font-medium transition-colors ${
                selected
                  ? "border-brand text-neutral-100"
                  : "border-transparent text-neutral-400 hover:border-neutral-700 hover:text-neutral-200"
              }`}
            >
              {tab.label}
            </button>
          );
        })}
      </div>

      <div role="tabpanel" id={panelId(activeTab)} aria-labelledby={tabId(activeTab)}>
      {activeTab === "upload" ? (
        <ManualUploadZone />
      ) : (
        <div>
          <div className="mb-2 flex max-w-xl flex-wrap items-center gap-3 rounded border border-neutral-800 p-4">
            <select
              aria-label="Storage location to scan"
              value={selected}
              disabled={isLoading || startScan.isPending}
              onChange={(e) => {
                setSelected(e.target.value ? Number(e.target.value) : "");
                setDifferential(false);
              }}
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
              onClick={() => selected !== "" && startScan.mutate({ storageLocationId: selected, differential })}
              className="rounded bg-sky-700 px-4 py-2 text-sm font-medium text-white hover:bg-sky-600 disabled:opacity-50"
            >
              {startScan.isPending ? "Starting…" : "Scan"}
            </button>
          </div>
          {isTier3 && (
            <label className="mb-4 flex items-center gap-2 text-xs text-neutral-400">
              <input
                type="checkbox"
                checked={differential}
                disabled={startScan.isPending}
                onChange={(e) => setDifferential(e.target.checked)}
              />
              Differential (skip re-hashing files whose size and modified time are unchanged)
            </label>
          )}
          {startScan.isError && (
            <p className="mb-4 text-sm text-red-400">Failed to start scan: {String(startScan.error)}</p>
          )}
          {isError && (
            <p className="mb-4 text-sm text-red-400">Failed to load storage locations: {String(error)}</p>
          )}
          {hasReadOnly && (
            <p className="mb-4 text-xs text-neutral-500">
              Read-only locations can be scanned, but branchDAM never writes to them. Tier 3 (master archive) is
              writable by default; a location is read-only only when configured with{" "}
              <code className="text-neutral-400">readOnly: true</code>.
            </p>
          )}

          <h2 className="mb-2 text-lg font-semibold text-neutral-200">Recent jobs</h2>
          {progress.isLoading ? (
            <p className="text-neutral-500">Loading…</p>
          ) : progress.isError ? (
            <p className="text-red-400">Failed to load recent scan jobs.</p>
          ) : (progress.data?.jobs.length ?? 0) === 0 ? (
            <p className="text-neutral-500">No scans yet.</p>
          ) : (
            <div className="overflow-x-auto">
            <table className="w-full min-w-[640px] text-left text-sm">
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
                {progress.data?.jobs.map((j) => {
                  const isWatch = j.kind === "WATCH";
                  const label = isWatch && j.state === "CANCELLED" ? "STOPPED" : isWatch && j.state === "RUNNING" ? "ACTIVE" : j.state;
                  const title = isWatch && j.state === "CANCELLED"
                    ? "Continuous background watcher stopped cleanly on server shutdown or restart"
                    : isWatch && j.state === "RUNNING"
                    ? "Continuous filesystem watcher is actively monitoring storage"
                    : undefined;

                  const colorClass = j.state === "FAILED"
                    ? "text-red-400"
                    : isWatch && j.state === "RUNNING"
                    ? "text-emerald-400"
                    : j.state === "RUNNING"
                    ? "text-amber-400"
                    : isWatch && j.state === "CANCELLED"
                    ? "text-neutral-400"
                    : "text-neutral-300";

                  return (
                    <tr key={j.id} className="border-b border-neutral-900">
                      <td className="py-2 pr-4">{j.kind}</td>
                      <td
                        title={title}
                        className={`py-2 pr-4 ${colorClass}`}
                      >
                        {label}
                      </td>
                      <td className="py-2 pr-4 text-right">{j.filesSeen.toLocaleString()}</td>
                      <td className="py-2 pr-4 text-right">{j.filesHashed.toLocaleString()}</td>
                      <td className="py-2 pr-4 text-right">{j.filesFailed.toLocaleString()}</td>
                      <td className="py-2 pr-4 text-right">{j.edgesCreated.toLocaleString()}</td>
                      <td className="py-2 pr-4 font-mono text-xs text-neutral-500">{j.lastError ?? "—"}</td>
                    </tr>
                  );
                })}
              </tbody>
            </table>
            </div>
          )}
        </div>
      )}
      </div>
    </div>
  );
}
