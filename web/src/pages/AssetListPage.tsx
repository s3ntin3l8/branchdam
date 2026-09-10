import { useSearchParams, Link } from "react-router";
import { useAssetFacets, useAssets, useMe, useStorageLocations, useUsers } from "../hooks/queries";
import Thumbnail from "../components/Thumbnail";
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
  const lifecycleState = (searchParams.get("lifecycleState") as Asset["lifecycleState"]) || "";
  const searchQuery = searchParams.get("q") || "";
  const unlinkedOnly = searchParams.get("unlinkedOnly") === "true";
  const myUploads = searchParams.get("myUploads") === "true";
  const page = Math.max(1, Number(searchParams.get("page") || "1"));

  const offset = (page - 1) * PAGE_SIZE;

  const { data: facetsData } = useAssetFacets();
  const { data: locsData } = useStorageLocations();
  const { data: meData } = useMe();
  const myUserID = meData?.attributionUserId ?? 0;
  // "My uploads" requires both the toggle and a resolved attribution
  // user id on the request Principal. Without a user id the toggle
  // would silently show an empty list, which is misleading -- treat
  // the toggle as off in that case.
  const effectiveMyUploads = myUploads && myUserID > 0;
  // Users cache (admin-only /api/v1/users). Used to render the
  // "Uploaded by" column -- a row-level lookup against the cache is
  // cheaper than embedding the full user in every asset row, and
  // matches the SPA's existing pattern for resolving location /
  // camera labels. Returns 503 in deployments without attribution
  // wired; the SPA tolerates that as "empty users cache".
  const { data: usersData } = useUsers();
  const usersByID = new Map<number, string>();
  for (const u of usersData?.users ?? []) {
    usersByID.set(u.id, u.username);
  }
  const formatUploadedBy = (id: number | undefined): string => {
    if (id === undefined || id === 0) return "—";
    return usersByID.get(id) ?? `#${id}`;
  };
  const { data, isLoading, isError, error } = useAssets({
    limit: PAGE_SIZE,
    offset,
    cameraModel: cameraModel || undefined,
    graphStatus: graphStatus || undefined,
    storageLocationId,
    lifecycleState: lifecycleState || undefined,
    unlinkedOnly: unlinkedOnly || undefined,
    uploadedByUserId: effectiveMyUploads ? myUserID : undefined,
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

  const hasActiveFilters = Boolean(cameraModel || graphStatus || storageLocationId || unlinkedOnly || effectiveMyUploads || lifecycleState || searchQuery);

  const displayedAssets = searchQuery.trim()
    ? assets.filter((a) =>
        a.fileName.toLowerCase().includes(searchQuery.toLowerCase()) ||
        a.filePath.toLowerCase().includes(searchQuery.toLowerCase())
      )
    : assets;

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

          {/* Lifecycle State */}
          <div>
            <label htmlFor="lifecycle-state-filter" className="block text-neutral-400 mb-1">Lifecycle State</label>
            <select
              id="lifecycle-state-filter"
              value={lifecycleState}
              onChange={(e) => updateFilters({ lifecycleState: e.target.value })}
              className="w-full rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 focus:outline-none focus:border-neutral-500"
            >
              <option value="">All States</option>
              <option value="ACTIVE">ACTIVE</option>
              <option value="MISSING">MISSING</option>
              <option value="ARCHIVED">ARCHIVED</option>
              <option value="HIDDEN">HIDDEN</option>
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

          {/* My Uploads Checkbox. Disabled when the request has no
              resolved attribution user id (machine/anonymous
              principal, or a deployment that hasn't wired attribution)
              -- the filter would be a no-op in that case. */}
          <div className="flex items-center pb-2 space-x-2">
            <input
              type="checkbox"
              id="my-uploads-checkbox"
              checked={effectiveMyUploads}
              disabled={myUserID === 0}
              onChange={(e) =>
                updateFilters({
                  myUploads: e.target.checked ? "true" : null,
                })
              }
              className="rounded border-neutral-700 bg-neutral-800 text-indigo-500 focus:ring-0 disabled:opacity-40"
            />
            <label
              htmlFor="my-uploads-checkbox"
              className={`font-medium select-none cursor-pointer ${myUserID === 0 ? "text-neutral-500" : "text-neutral-300"}`}
              title={myUserID === 0 ? "Attribution isn't wired for this request" : undefined}
            >
              My Uploads
            </label>
          </div>

          {/* Search Query */}
          <div className="sm:col-span-2">
            <label htmlFor="search-filter" className="block text-neutral-400 mb-1">Search Path / Filename</label>
            <input
              id="search-filter"
              type="text"
              placeholder="Search filename or path..."
              value={searchQuery}
              onChange={(e) => updateFilters({ q: e.target.value })}
              className="w-full rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1.5 text-neutral-200 placeholder:text-neutral-500 focus:outline-none focus:border-neutral-500"
            />
          </div>
        </div>
      </div>

      {isLoading ? (
        <div className="p-6 text-neutral-400">Loading assets…</div>
      ) : isError ? (
        <div className="p-6 text-red-400">Failed to load assets: {String(error)}</div>
      ) : displayedAssets.length === 0 ? (
        <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-8 text-center text-neutral-500">
          No assets match the selected filters.
        </div>
      ) : (
        <>
          <table className="w-full text-left text-sm">
            <thead className="border-b border-neutral-800 text-neutral-400">
              <tr>
                <th className="py-2 pr-4"></th>
                <th className="py-2 pr-4">Path</th>
                <th className="py-2 pr-4">Lifecycle</th>
                <th className="py-2 pr-4">Camera Model</th>
                <th className="py-2 pr-4">Tier status</th>
                <th className="py-2 pr-4">Graph status</th>
                <th className="py-2 pr-4">Uploaded by</th>
                <th className="py-2 pr-4">Fast Hash</th>
              </tr>
            </thead>
            <tbody>
              {displayedAssets.map((a) => (
                <tr key={a.id} className="border-b border-neutral-900 hover:bg-neutral-900">
                  <td className="py-2 pr-4">
                    <Thumbnail assetId={a.id} thumbState={a.thumbState} alt={a.fileName} />
                  </td>
                  <td className="py-2 pr-4">
                    <Link to={`/assets/${a.id}`} className="text-sky-400 hover:underline font-mono text-xs">
                      {a.filePath}
                    </Link>
                  </td>
                  <td className="py-2 pr-4 text-xs">
                    <span className={`rounded px-1.5 py-0.5 text-[10px] font-medium ${
                      a.lifecycleState === "ACTIVE" ? "bg-emerald-950 text-emerald-300 border border-emerald-800/60" :
                      a.lifecycleState === "MISSING" ? "bg-red-950 text-red-300 border border-red-800/60" :
                      a.lifecycleState === "ARCHIVED" ? "bg-neutral-800 text-neutral-400 border border-neutral-700" :
                      "bg-amber-950 text-amber-300 border border-amber-800/60"
                    }`}>
                      {a.lifecycleState}
                    </span>
                  </td>
                  <td className="py-2 pr-4 text-neutral-400 text-xs">{a.cameraModel || "—"}</td>
                  <td className="py-2 pr-4 text-neutral-400 text-xs">{a.indexingStatus}</td>
                  <td className={`py-2 pr-4 text-xs ${statusColor[a.graphStatus] ?? ""}`}>{a.graphStatus}</td>
                  <td className="py-2 pr-4 text-neutral-500 text-xs">{formatUploadedBy(a.uploadedByUserId)}</td>
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
                aria-label="Previous page"
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
                aria-label="Next page"
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
