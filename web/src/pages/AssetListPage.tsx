import { useSearchParams, Link } from "react-router";
import { useAssetFacets, useAssets, useStorageLocations } from "../hooks/queries";
import type { Asset } from "../api/types";

const statusColor: Record<string, string> = {
  LINKED: "text-emerald-400 font-medium",
  UNLINKED: "text-neutral-500",
  NEEDS_REVIEW: "text-amber-400 font-medium",
  ROOT: "text-sky-400 font-medium",
};

const PAGE_SIZE = 50;

export default function AssetListPage() {
  const [searchParams, setSearchParams] = useSearchParams();

  const cameraModel = searchParams.get("cameraModel") || "";
  const graphStatus = (searchParams.get("graphStatus") as Asset["graphStatus"]) || "";
  const storageLocationId = searchParams.get("storageLocationId") ? Number(searchParams.get("storageLocationId")) : undefined;
  const unlinkedOnly = searchParams.get("unlinkedOnly") === "true";
  const page = Math.max(1, Number(searchParams.get("page") || "1"));

  const offset = (page - 1) * PAGE_SIZE;

  const { data: facetsData } = useAssetFacets();
  const { data: locsData } = useStorageLocations();
  const { data, isLoading, isError, error } = useAssets({
    limit: PAGE_SIZE,
    offset,
    cameraModel: cameraModel || undefined,
    graphStatus: graphStatus || undefined,
    storageLocationId,
    unlinkedOnly: unlinkedOnly || undefined,
  });

  const cameraModels = facetsData?.cameraModels ?? [];
  const locations = locsData?.locations ?? [];
  const assets = data?.assets ?? [];
  const total = data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / PAGE_SIZE));

  const updateFilters = (updates: Record<string, string | null>) => {
    const nextParams = new URLSearchParams(searchParams);
    nextParams.set("page", "1"); // Reset to first page on filter change
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

  const hasActiveFilters = Boolean(cameraModel || graphStatus || storageLocationId || unlinkedOnly);

  return (
    <div className="p-6 space-y-6">
      <div className="flex items-center justify-between">
        <div>
          <h1 className="text-xl font-semibold text-neutral-100">Assets</h1>
          <p className="text-xs text-neutral-400 mt-0.5">
            {total} total asset{total === 1 ? "" : "s"} indexed
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

      {/* FILTERS PANEL */}
      <div className="rounded-lg border border-neutral-800 bg-neutral-900/80 p-4 space-y-4 text-xs">
        <div className="font-semibold text-neutral-300 uppercase tracking-wider text-[11px]">Filters</div>
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4 items-end">
          {/* Camera Model */}
          <div>
            <label htmlFor="camera-model-filter" className="block text-neutral-400 mb-1">Camera Model</label>
            <select
              id="camera-model-filter"
              value={cameraModel}
              onChange={(e) => updateFilters({ cameraModel: e.target.value })}
              className="w-full rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 focus:outline-none focus:border-neutral-500"
            >
              <option value="">All Cameras</option>
              {cameraModels.map((m) => (
                <option key={m} value={m}>
                  {m}
                </option>
              ))}
            </select>
          </div>

          {/* Graph Status */}
          <div>
            <label htmlFor="graph-status-filter" className="block text-neutral-400 mb-1">Graph Status</label>
            <select
              id="graph-status-filter"
              value={unlinkedOnly ? "UNLINKED" : graphStatus}
              disabled={unlinkedOnly}
              onChange={(e) => updateFilters({ graphStatus: e.target.value, unlinkedOnly: null })}
              className="w-full rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 focus:outline-none focus:border-neutral-500 disabled:opacity-50"
            >
              <option value="">All Statuses</option>
              <option value="UNLINKED">UNLINKED</option>
              <option value="LINKED">LINKED</option>
              <option value="NEEDS_REVIEW">NEEDS_REVIEW</option>
              <option value="ROOT">ROOT</option>
            </select>
          </div>

          {/* Storage Location */}
          <div>
            <label htmlFor="storage-location-filter" className="block text-neutral-400 mb-1">Storage Location</label>
            <select
              id="storage-location-filter"
              value={storageLocationId ?? ""}
              onChange={(e) => updateFilters({ storageLocationId: e.target.value })}
              className="w-full rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 focus:outline-none focus:border-neutral-500"
            >
              <option value="">All Locations</option>
              {locations.map((loc) => (
                <option key={loc.id} value={loc.id}>
                  {loc.name} ({loc.tier})
                </option>
              ))}
            </select>
          </div>

          {/* Unlinked Only Checkbox */}
          <div className="flex items-center pb-2 space-x-2">
            <input
              type="checkbox"
              id="unlinked-only-checkbox"
              checked={unlinkedOnly}
              onChange={(e) =>
                updateFilters({
                  unlinkedOnly: e.target.checked ? "true" : null,
                  graphStatus: null,
                })
              }
              className="rounded border-neutral-700 bg-neutral-800 text-indigo-500 focus:ring-0"
            />
            <label htmlFor="unlinked-only-checkbox" className="text-neutral-300 font-medium select-none cursor-pointer">
              Unlinked Only
            </label>
          </div>
        </div>
      </div>

      {isLoading ? (
        <div className="p-6 text-neutral-400">Loading assets…</div>
      ) : isError ? (
        <div className="p-6 text-red-400">Failed to load assets: {String(error)}</div>
      ) : assets.length === 0 ? (
        <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-8 text-center text-neutral-500">
          No assets match the selected filters.
        </div>
      ) : (
        <>
          <table className="w-full text-left text-sm">
            <thead className="border-b border-neutral-800 text-neutral-400">
              <tr>
                <th className="py-2 pr-4">Path</th>
                <th className="py-2 pr-4">Camera Model</th>
                <th className="py-2 pr-4">Tier status</th>
                <th className="py-2 pr-4">Graph status</th>
                <th className="py-2 pr-4">Fast Hash</th>
              </tr>
            </thead>
            <tbody>
              {assets.map((a) => (
                <tr key={a.id} className="border-b border-neutral-900 hover:bg-neutral-900">
                  <td className="py-2 pr-4">
                    <Link to={`/assets/${a.id}`} className="text-sky-400 hover:underline font-mono text-xs">
                      {a.filePath}
                    </Link>
                  </td>
                  <td className="py-2 pr-4 text-neutral-400 text-xs">{a.cameraModel || "—"}</td>
                  <td className="py-2 pr-4 text-neutral-400 text-xs">{a.indexingStatus}</td>
                  <td className={`py-2 pr-4 text-xs ${statusColor[a.graphStatus] ?? ""}`}>{a.graphStatus}</td>
                  <td className="py-2 pr-4 font-mono text-xs text-neutral-500">{a.fastHash ?? "—"}</td>
                </tr>
              ))}
            </tbody>
          </table>

          {/* PAGINATION CONTROLS */}
          <div className="flex items-center justify-between pt-4 border-t border-neutral-800 text-xs text-neutral-400">
            <div>
              Showing {Math.min(offset + 1, total)} to {Math.min(offset + assets.length, total)} of {total} items
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
