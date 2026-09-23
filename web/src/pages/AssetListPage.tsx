import { useEffect, useRef, useState } from "react";
import { useSearchParams, Link } from "react-router";
import { useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";
import { useAssetFacets, useAssets, useDeleteAsset, useMe, useRestoreAsset, useStorageLocations, useUsers } from "../hooks/queries";
import ConfirmDialog from "../components/ConfirmDialog";
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

  const queryClient = useQueryClient();
  const deleteAsset = useDeleteAsset();
  const restoreAsset = useRestoreAsset();

  // Per-row archive confirmation. Restore has no confirm step, matching
  // AssetDetailPage's AssetDeleteControl.
  //
  // deleteAsset/restoreAsset are each a single shared mutation instance
  // across every row (there is only one row's dialog/action in flight at a
  // time). Without an explicit .reset() at the open/close boundary, a
  // failed archive's error state -- or a stale isPending/isError from a
  // previous row's restore -- leaks into the next row's dialog or button
  // before that row has been touched. openArchiveDialog/closeArchiveDialog
  // and handleRestore below reset the relevant mutation and scope its
  // pending/error label/message display to the specific row via
  // archiveTarget / restoreTargetId. The Restore button's disabled state is
  // deliberately NOT scoped the same way: since there is only one shared
  // mutation, disabling only the clicked row would let a second row's click
  // reset() and re-target that same mutation mid-flight, silently dropping
  // the first row's in-flight result. Disabling every Restore button while
  // any restore is pending keeps the single mutation single-owner.
  const [archiveTarget, setArchiveTarget] = useState<Asset | null>(null);
  const [restoreTargetId, setRestoreTargetId] = useState<number | null>(null);

  // Batch archive selection. Scoped to the CURRENT PAGE only -- there is no
  // "select all matching this filter" endpoint to back a broader claim, so
  // the selection is cleared whenever the page or any filter changes rather
  // than silently carrying stale ids across a refetch.
  const [selectedIds, setSelectedIds] = useState<Set<number>>(new Set());
  const [batchDialogOpen, setBatchDialogOpen] = useState(false);
  const [batchPending, setBatchPending] = useState(false);
  const [batchProgress, setBatchProgress] = useState<{ done: number; total: number } | null>(null);
  const [batchError, setBatchError] = useState<{ count: number; message: string } | null>(null);

  // Reset selection synchronously during render when the page or any
  // filter changes, rather than in a useEffect -- this is React's
  // documented "adjusting state when a prop changes" pattern
  // (react.dev/learn/you-might-not-need-an-effect#adjusting-state-when-a-prop-changes),
  // which avoids the extra render-then-effect-then-render cascade a
  // useEffect version would cause here.
  const filterKey = searchParams.toString();
  const [prevFilterKey, setPrevFilterKey] = useState(filterKey);
  if (filterKey !== prevFilterKey) {
    setPrevFilterKey(filterKey);
    setSelectedIds(new Set());
    setBatchError(null);
    setRestoreTargetId(null);
  }

  const selectableIds = assets
    .filter((a) => a.lifecycleState !== "ARCHIVED" && a.lifecycleState !== "TRASHED")
    .map((a) => a.id);
  const allSelectableSelected = selectableIds.length > 0 && selectableIds.every((id) => selectedIds.has(id));
  const someSelected = selectedIds.size > 0;

  const selectAllRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (selectAllRef.current) {
      selectAllRef.current.indeterminate = someSelected && !allSelectableSelected;
    }
  }, [someSelected, allSelectableSelected]);

  const toggleRow = (id: number) => {
    setSelectedIds((prev) => {
      const next = new Set(prev);
      if (next.has(id)) next.delete(id);
      else next.add(id);
      return next;
    });
  };

  const toggleSelectAll = () => {
    setSelectedIds(allSelectableSelected ? new Set() : new Set(selectableIds));
  };

  const openArchiveDialog = (a: Asset) => {
    deleteAsset.reset();
    setArchiveTarget(a);
  };

  const closeArchiveDialog = () => {
    deleteAsset.reset();
    setArchiveTarget(null);
  };

  const handleRestore = (id: number) => {
    restoreAsset.reset();
    setRestoreTargetId(id);
    restoreAsset.mutate(id, {
      onSuccess: () => setRestoreTargetId(null),
    });
  };

  // Sequential loop over the existing single-asset DELETE endpoint rather
  // than a new batch route: the writer pool is single-connection
  // (SetMaxOpenConns(1)), so a batch route buys no concurrency, and this
  // way every archive still gets its own actor_audit row for free.
  // Deliberately calls api.deleteAsset directly instead of the
  // useDeleteAsset hook -- that hook invalidates ["assets"] in its own
  // onSuccess, which would refetch (and re-render the rows this loop is
  // iterating over) after every single archive in the batch. Invalidate
  // once, after the whole batch settles, instead.
  const runBatchArchive = async () => {
    const ids = Array.from(selectedIds);
    setBatchPending(true);
    setBatchError(null);
    setBatchProgress({ done: 0, total: ids.length });

    const failed: number[] = [];
    const succeeded: number[] = [];
    let firstErrorMessage: string | undefined;
    let completed = 0;
    for (const id of ids) {
      try {
        await api.deleteAsset(id);
        succeeded.push(id);
      } catch (err) {
        failed.push(id);
        if (firstErrorMessage === undefined) {
          firstErrorMessage = err instanceof Error ? err.message : String(err);
        }
      }
      completed += 1;
      setBatchProgress({ done: completed, total: ids.length });
    }

    // Mirror useDeleteAsset's per-id invalidation (["asset", id],
    // ["asset-metadata", id]) for every archived id, in addition to the
    // single ["assets"] list invalidation -- otherwise a detail page
    // cached earlier in the session for one of these ids would stay stale
    // after a batch archive. Only for succeeded ids: a failed archive
    // didn't change that asset's state.
    void queryClient.invalidateQueries({ queryKey: ["assets"] });
    for (const id of succeeded) {
      void queryClient.invalidateQueries({ queryKey: ["asset", id] });
      void queryClient.invalidateQueries({ queryKey: ["asset-metadata", id] });
    }
    setBatchPending(false);
    setBatchDialogOpen(false);
    setBatchProgress(null);
    if (failed.length > 0) {
      setSelectedIds(new Set(failed));
      setBatchError({ count: failed.length, message: firstErrorMessage ?? "unknown error" });
    } else {
      setSelectedIds(new Set());
    }
  };

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

  const hasActiveFilters = Boolean(cameraModel || graphStatus || storageLocationId || unlinkedOnly || effectiveMyUploads || lifecycleState);

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
              <option value="TRASHED">TRASHED</option>
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

        </div>
      </div>

      {someSelected && (
        <div className="sticky top-0 z-10 flex items-center justify-between gap-4 rounded-lg border border-indigo-800/60 bg-indigo-950/60 px-4 py-2 text-xs">
          <span className="text-indigo-200">
            {selectedIds.size} selected
          </span>
          <div className="flex items-center gap-3">
            {batchError && (
              <span className="text-red-400">
                {batchError.count} failed to trash: {batchError.message}
              </span>
            )}
            <button
              type="button"
              onClick={() => setSelectedIds(new Set())}
              className="rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1 text-neutral-300 hover:bg-neutral-700"
            >
              Clear
            </button>
            <button
              type="button"
              onClick={() => setBatchDialogOpen(true)}
              className="rounded border border-red-800/80 bg-red-950/60 px-2.5 py-1 font-medium text-red-300 hover:bg-red-900/60"
            >
              Trash selected
            </button>
          </div>
        </div>
      )}

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
                <th className="py-2 pr-4">
                  <input
                    ref={selectAllRef}
                    type="checkbox"
                    aria-label="Select all on this page"
                    checked={allSelectableSelected}
                    disabled={selectableIds.length === 0}
                    onChange={toggleSelectAll}
                    className="rounded border-neutral-700 bg-neutral-800 text-indigo-500 focus:ring-0 disabled:opacity-40"
                  />
                </th>
                <th className="py-2 pr-4"></th>
                <th className="py-2 pr-4">Path</th>
                <th className="py-2 pr-4">Lifecycle</th>
                <th className="py-2 pr-4">Camera Model</th>
                <th className="py-2 pr-4">Tier status</th>
                <th className="py-2 pr-4">Graph status</th>
                <th className="py-2 pr-4">Uploaded by</th>
                <th className="py-2 pr-4">Fast Hash</th>
                <th className="py-2 pr-4">Actions</th>
              </tr>
            </thead>
            <tbody>
              {assets.map((a) => (
                <tr key={a.id} className="border-b border-neutral-900 hover:bg-neutral-900">
                  <td className="py-2 pr-4">
                    <input
                      type="checkbox"
                      aria-label={`Select ${a.fileName}`}
                      checked={selectedIds.has(a.id)}
                      disabled={a.lifecycleState === "ARCHIVED"}
                      onChange={() => toggleRow(a.id)}
                      className="rounded border-neutral-700 bg-neutral-800 text-indigo-500 focus:ring-0 disabled:opacity-40"
                    />
                  </td>
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
                      a.lifecycleState === "TRASHED" ? "bg-orange-950 text-orange-300 border border-orange-800/60" :
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
                  <td className="py-2 pr-4 text-xs">
                    {a.lifecycleState === "ARCHIVED" || a.lifecycleState === "TRASHED" ? (
                      <div className="flex flex-col items-start gap-1">
                        <button
                          type="button"
                          onClick={() => handleRestore(a.id)}
                          disabled={restoreAsset.isPending}
                          className="rounded border border-emerald-800/80 bg-emerald-950/60 px-2 py-1 font-medium text-emerald-300 hover:bg-emerald-900/60 disabled:opacity-50"
                        >
                          {restoreAsset.isPending && restoreTargetId === a.id ? "Restoring…" : "Restore"}
                        </button>
                        {restoreAsset.isError && restoreTargetId === a.id && (
                          <span className="text-[10px] text-red-400">
                            Failed to restore: {String(restoreAsset.error)}
                          </span>
                        )}
                      </div>
                    ) : (
                      <button
                        type="button"
                        onClick={() => openArchiveDialog(a)}
                        className="rounded border border-red-800/80 bg-red-950/60 px-2 py-1 font-medium text-red-300 hover:bg-red-900/60"
                      >
                        Trash
                      </button>
                    )}
                  </td>
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

      {archiveTarget && (
        <ConfirmDialog
          titleId="archive-asset-title"
          title="Trash Asset"
          body={
            <>
              Move <strong className="text-amber-400">{archiveTarget.fileName}</strong> to{" "}
              <code className="text-amber-400">.trash/</code>? The media node and any linked Tier-2 export copies
              will be removed from their original locations, marked{" "}
              <code className="text-amber-400">TRASHED</code>, and auto-purged after 30 days unless restored.
            </>
          }
          confirmLabel="Confirm Trash"
          pendingLabel="Trashing…"
          isPending={deleteAsset.isPending}
          error={deleteAsset.isError ? deleteAsset.error : undefined}
          errorLabel="Failed to trash"
          onConfirm={() => {
            deleteAsset.mutate(archiveTarget.id, {
              onSuccess: () => setArchiveTarget(null),
            });
          }}
          onCancel={closeArchiveDialog}
        />
      )}

      {batchDialogOpen && (
        <ConfirmDialog
          titleId="batch-archive-title"
          title="Trash Selected Assets"
          body={
            batchPending && batchProgress
              ? `Trashing ${batchProgress.done}/${batchProgress.total}…`
              : `Are you sure you want to trash ${selectedIds.size} asset${selectedIds.size === 1 ? "" : "s"}? Each will be moved to .trash/, marked TRASHED, and auto-purged after 30 days unless restored.`
          }
          confirmLabel="Confirm Trash"
          pendingLabel="Trashing…"
          isPending={batchPending}
          onConfirm={() => {
            void runBatchArchive();
          }}
          onCancel={() => setBatchDialogOpen(false)}
        />
      )}
    </div>
  );
}
