import { useState } from "react";
import { useParams, useSearchParams, Link } from "react-router";
import {
  useAsset,
  useAssetLineage,
  useAssetMetadata,
  useAssetSyncStatus,
  useTrashAsset,
  useInheritMetadata,
  usePreviewInheritMetadata,
  usePruneCache,
  useRestoreAsset,
  useRetrySync,
  useStorageLocations,
} from "../hooks/queries";
import AssetGraphCanvas from "../components/AssetGraphCanvas";
import AssetMediaPreview from "../components/AssetMediaPreview";
import ConfirmDialog from "../components/ConfirmDialog";
import Thumbnail from "../components/Thumbnail";
import type { Asset, InheritMetadataResponse } from "../api/types";

// identityTags are the two tags Plan (internal/metadata) always emits
// regardless of whether there was anything to inherit -- excluded from the
// "N tag(s) inherited" count so re-running against an already-fully-tagged
// child (or one with no eligible parent field at all) doesn't read as
// having inherited something.
const identityTags = new Set(["XMP-dc:Identifier", "XMP-xmpMM:DerivedFrom"]);

// AssetInheritMetadataControl backs POST /api/v1/assets/{id}/inherit-metadata
// (#54). The endpoint rewrites the child's file in place, so the button no
// longer fires it directly: it first asks for a ?dryRun=1 plan (same
// server-side eligibility checks, nothing written), shows the operator the
// exact tags and the parent they come from, and only writes on Confirm.
// Eligibility failures (no resolved parent, read-only location, ...) come
// back from the dry run with the server's own 409/422 message, surfaced
// as-is rather than re-derived client-side.
function AssetInheritMetadataControl({ asset }: { asset: Asset }) {
  const previewInherit = usePreviewInheritMetadata();
  const inheritMetadata = useInheritMetadata();
  const [plan, setPlan] = useState<InheritMetadataResponse | null>(null);
  const [message, setMessage] = useState<string | null>(null);

  const handlePreview = () => {
    setMessage(null);
    inheritMetadata.reset();
    previewInherit.mutate(asset.id, {
      onSuccess: (data) => {
        if (Object.keys(data.inherited).length === 0) {
          setMessage("Nothing new to inherit.");
          return;
        }
        setPlan(data);
      },
    });
  };

  const handleConfirm = () => {
    inheritMetadata.mutate(asset.id, {
      onSuccess: (data) => {
        setPlan(null);
        const count = Object.keys(data.inherited).filter(
          (tag) => !identityTags.has(tag),
        ).length;
        setMessage(
          count > 0
            ? `Inherited ${count} tag${count === 1 ? "" : "s"}.`
            : "Nothing new to inherit.",
        );
      },
    });
  };

  const plannedTags = plan
    ? Object.entries(plan.inherited).sort(([a], [b]) => a.localeCompare(b))
    : [];

  return (
    <span className="flex items-center gap-2 text-xs">
      {previewInherit.isError && (
        <span className="text-red-400">
          Cannot inherit: {String(previewInherit.error)}
        </span>
      )}
      {message && <span className="text-neutral-400">{message}</span>}
      <button
        type="button"
        onClick={handlePreview}
        disabled={previewInherit.isPending || inheritMetadata.isPending}
        className="rounded border border-neutral-700 bg-neutral-800 px-2 py-1 text-xs text-neutral-300 hover:bg-neutral-700 disabled:opacity-50"
      >
        {previewInherit.isPending ? "Checking…" : "Inherit Metadata"}
      </button>
      {plan && (
        <ConfirmDialog
          titleId="inherit-metadata-title"
          title="Inherit Metadata"
          body={
            <div className="space-y-3">
              <p>
                This rewrites{" "}
                <strong className="text-amber-300">{asset.fileName}</strong> in
                place, writing these tags
                {plan.parentFileName ? (
                  <>
                    {" "}
                    from{" "}
                    <strong className="text-neutral-100">
                      {plan.parentFileName}
                    </strong>
                  </>
                ) : null}
                :
              </p>
              <table
                className="w-full text-left font-mono text-[11px]"
                aria-label="Tags to be written"
              >
                <thead className="text-neutral-500">
                  <tr>
                    <th className="pr-3 font-normal">Tag</th>
                    <th className="font-normal">Value</th>
                  </tr>
                </thead>
                <tbody>
                  {plannedTags.map(([tag, value]) => (
                    <tr key={tag}>
                      <td className="pr-3 align-top text-neutral-400">{tag}</td>
                      <td className="break-all text-neutral-200">{value}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
          }
          confirmLabel="Write Tags"
          pendingLabel="Writing…"
          isPending={inheritMetadata.isPending}
          error={inheritMetadata.isError ? inheritMetadata.error : undefined}
          errorLabel="Inherit failed"
          onConfirm={handleConfirm}
          onCancel={() => setPlan(null)}
        />
      )}
    </span>
  );
}

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="grid grid-cols-3 gap-2 py-1 text-sm">
      <dt className="text-neutral-500">{label}</dt>
      <dd className="col-span-2 break-all font-mono text-neutral-200">
        {value ?? "—"}
      </dd>
    </div>
  );
}

// AssetPruneControl is the per-asset counterpart to StorageHealthPage's
// location-level purge control: same dry-run-then-confirm flow, narrowed
// to this one node via nodeIds. A plan that comes back empty means the
// asset isn't eligible (no verified Tier-3 ancestor, or not yet past its
// location's TTL) -- reported as a message, not an error, since "not
// eligible" is an expected outcome, not a failure.
//
// Renders nothing at all for an asset on a non-prunable location (the
// common case: any Tier-2/3 asset), mirroring StorageHealthPage's own
// loc.prunable gate rather than showing a button that always resolves to
// "Not eligible" -- the Asset DTO carries no tier/prunable field of its
// own, so this cross-references the already-fetched storage-locations
// list instead of adding one.
function AssetPruneControl({ asset }: { asset: Asset }) {
  const { data: locationsData } = useStorageLocations();
  const location = locationsData?.locations.find(
    (l) => l.id === asset.storageLocationId,
  );

  const [checked, setChecked] = useState<"eligible" | "ineligible" | null>(
    null,
  );
  // null = not attempted; otherwise the actual outcome for THIS asset's
  // candidate, not just "the request succeeded" -- Execute re-verifies
  // eligibility right before deleting, so a 200 response can still report
  // purged=false (e.g. the Tier-3 master went MISSING between the dry-run
  // and this click), which must not be shown as "Cache purged."
  const [purgeResult, setPurgeResult] = useState<{
    purged: boolean;
    error?: string;
  } | null>(null);
  const pruneCache = usePruneCache();

  // Storage locations are always fetched at app startup elsewhere, so this
  // is normally already cached; while genuinely unresolved (location ===
  // undefined, not yet loaded) the control stays hidden rather than
  // flashing a button that would immediately turn out to be ineligible.
  if (!location?.prunable) {
    return null;
  }

  const checkEligibility = () => {
    setPurgeResult(null);
    pruneCache.mutate(
      { storageLocationId: asset.storageLocationId, nodeIds: [asset.id] },
      {
        onSuccess: (res) =>
          setChecked(res.candidates.length > 0 ? "eligible" : "ineligible"),
      },
    );
  };

  const confirmPurge = () => {
    pruneCache.mutate(
      {
        storageLocationId: asset.storageLocationId,
        nodeIds: [asset.id],
        execute: true,
      },
      {
        onSuccess: (res) => {
          setChecked(null);
          const outcome = res.candidates.find((c) => c.nodeId === asset.id);
          setPurgeResult(
            outcome
              ? { purged: outcome.purged, error: outcome.error }
              : { purged: false },
          );
        },
      },
    );
  };

  if (purgeResult) {
    return purgeResult.purged ? (
      <span className="text-xs text-neutral-400">Cache purged.</span>
    ) : (
      <span className="text-xs text-red-400">
        Purge failed{purgeResult.error ? `: ${purgeResult.error}` : ""}.{" "}
        <button
          type="button"
          onClick={() => setPurgeResult(null)}
          className="underline hover:text-red-300"
        >
          Dismiss
        </button>
      </span>
    );
  }

  if (checked === "ineligible") {
    return (
      <span className="text-xs text-neutral-500">
        Not eligible for pruning.{" "}
        <button
          type="button"
          onClick={() => setChecked(null)}
          className="underline hover:text-neutral-300"
        >
          Dismiss
        </button>
      </span>
    );
  }

  if (checked === "eligible") {
    return (
      <span className="flex items-center gap-2 text-xs">
        {pruneCache.isError && (
          <span className="text-red-400">
            Purge failed: {String(pruneCache.error)}
          </span>
        )}
        <button
          type="button"
          onClick={confirmPurge}
          disabled={pruneCache.isPending}
          className="rounded bg-red-900 px-2 py-1 text-red-200 hover:bg-red-800 disabled:opacity-50"
        >
          {pruneCache.isPending ? "Purging…" : "Confirm Purge"}
        </button>
        <button
          type="button"
          onClick={() => setChecked(null)}
          className="rounded bg-neutral-800 px-2 py-1 text-neutral-300 hover:bg-neutral-700"
        >
          Cancel
        </button>
      </span>
    );
  }

  return (
    <span>
      {pruneCache.isError && (
        <span className="mr-2 text-xs text-red-400">
          Check failed: {String(pruneCache.error)}
        </span>
      )}
      <button
        type="button"
        onClick={checkEligibility}
        disabled={pruneCache.isPending}
        className="rounded border border-neutral-700 bg-neutral-800 px-2 py-1 text-xs text-neutral-300 hover:bg-neutral-700 disabled:opacity-50"
      >
        {pruneCache.isPending ? "Checking…" : "Purge Cache"}
      </button>
    </span>
  );
}

function AssetDeleteControl({ asset }: { asset: Asset }) {
  const [trashOpen, setTrashOpen] = useState(false);
  const [freeUpOpen, setFreeUpOpen] = useState(false);
  const trashAsset = useTrashAsset();
  const restoreAsset = useRestoreAsset();

  if (
    asset.lifecycleState === "ARCHIVED" ||
    asset.lifecycleState === "TRASHED"
  ) {
    if (asset.supersededBy) {
      return (
        <div className="flex items-center gap-2">
          <span className="rounded border border-neutral-700 bg-neutral-800/80 px-2.5 py-1 text-xs text-neutral-400">
            {asset.lifecycleState === "ARCHIVED"
              ? "Archived (Superseded)"
              : "Trashed (Superseded)"}
          </span>
          <Link
            to={`/assets/${asset.supersededBy}`}
            className="rounded border border-neutral-700 bg-neutral-800/80 px-2.5 py-1 text-xs font-medium text-neutral-300 hover:bg-neutral-700"
          >
            View Successor (Asset #{asset.supersededBy})
          </Link>
        </div>
      );
    }
    const label = asset.lifecycleState === "ARCHIVED" ? "Archived" : "Trashed";
    const spanClass =
      asset.lifecycleState === "ARCHIVED"
        ? "rounded border border-neutral-700 bg-neutral-800/80 px-2.5 py-1 text-xs text-neutral-400"
        : "rounded border border-orange-800/80 bg-orange-950/60 px-2.5 py-1 text-xs text-orange-300";
    return (
      <div className="flex items-center gap-2">
        <span className={spanClass}>{label}</span>
        <button
          type="button"
          onClick={() => restoreAsset.mutate(asset.id)}
          disabled={restoreAsset.isPending}
          className="rounded border border-emerald-800/80 bg-emerald-950/60 px-2.5 py-1 text-xs font-medium text-emerald-300 hover:bg-emerald-900/60 disabled:opacity-50"
        >
          {restoreAsset.isPending ? "Restoring…" : "Restore Asset"}
        </button>
        {restoreAsset.isError && (
          <span className="text-xs text-red-400">
            Failed to restore:{" "}
            {restoreAsset.error instanceof Error
              ? restoreAsset.error.message
              : String(restoreAsset.error)}
          </span>
        )}
      </div>
    );
  }

  // A MISSING asset's file is already gone from its recorded path (the
  // scanner couldn't find it), so there is nothing for Trash or Free Up
  // Space to move into .trash/ -- offering them would only produce a
  // server error, or worse, read as "this will free space". The row is
  // kept (invariant 1) and recovers on its own if the file reappears.
  if (asset.lifecycleState === "MISSING") {
    return (
      <span
        className="rounded border border-amber-800/80 bg-amber-950/60 px-2.5 py-1 text-xs text-amber-300"
        title="The file was not found at its recorded path on the last scan. It is kept in the catalog and relinked if it reappears."
      >
        Missing on disk
      </span>
    );
  }

  return (
    <div className="flex flex-wrap items-center gap-2">
      <button
        type="button"
        onClick={() => setTrashOpen(true)}
        disabled={trashAsset.isPending}
        className="rounded border border-red-800/80 bg-red-950/60 px-2.5 py-1 text-xs font-medium text-red-300 hover:bg-red-900/60 disabled:opacity-50"
      >
        Trash
      </button>
      <button
        type="button"
        onClick={() => setFreeUpOpen(true)}
        disabled={trashAsset.isPending}
        className="rounded border border-amber-800/80 bg-amber-950/60 px-2.5 py-1 text-xs font-medium text-amber-300 hover:bg-amber-900/60 disabled:opacity-50"
        title="Trash the master file but keep Tier-2 export copies on disk."
      >
        Free Up Space
      </button>

      {trashOpen && (
        <ConfirmDialog
          titleId="trash-asset-title"
          title="Trash Asset"
          body={
            <>
              Move <strong className="text-amber-300">{asset.fileName}</strong>{" "}
              to <code className="text-amber-300">.trash/</code>? The master
              file and any linked Tier-2 export copies will be removed from
              their original locations. The media node is marked{" "}
              <code className="text-amber-300">TRASHED</code> and the bytes are
              auto-purged after 30 days unless restored.
            </>
          }
          confirmLabel="Confirm Trash"
          pendingLabel="Trashing…"
          isPending={trashAsset.isPending}
          error={trashAsset.isError ? trashAsset.error : undefined}
          errorLabel="Failed to trash"
          onConfirm={() => {
            trashAsset.mutate(
              { id: asset.id, keepExports: false },
              { onSuccess: () => setTrashOpen(false) },
            );
          }}
          onCancel={() => setTrashOpen(false)}
        />
      )}

      {freeUpOpen && (
        <ConfirmDialog
          titleId="free-up-space-title"
          title="Free Up Space"
          body={
            <>
              Move <strong className="text-amber-300">{asset.fileName}</strong>{" "}
              to <code className="text-amber-300">.trash/</code> but keep any
              Tier-2 export copies on disk? The master file is removed from the
              archive; cloud galleries are left untouched. Auto-purged after 30
              days unless restored.
            </>
          }
          confirmLabel="Confirm Free Up Space"
          pendingLabel="Trashing…"
          isPending={trashAsset.isPending}
          error={trashAsset.isError ? trashAsset.error : undefined}
          errorLabel="Failed to trash"
          onConfirm={() => {
            trashAsset.mutate(
              { id: asset.id, keepExports: true },
              { onSuccess: () => setFreeUpOpen(false) },
            );
          }}
          onCancel={() => setFreeUpOpen(false)}
        />
      )}
    </div>
  );
}

function AssetMetadataInspector({ assetId }: { assetId: number }) {
  const { data, isLoading, isError } = useAssetMetadata(assetId);
  const [filter, setFilter] = useState("");
  const metadata = data?.metadata ?? [];

  const filtered = filter.trim()
    ? metadata.filter(
        (m) =>
          m.key.toLowerCase().includes(filter.toLowerCase()) ||
          m.value.toLowerCase().includes(filter.toLowerCase()) ||
          m.source.toLowerCase().includes(filter.toLowerCase()),
      )
    : metadata;

  return (
    <section>
      <div className="mb-2 flex items-center justify-between">
        <h2 className="text-sm font-medium text-neutral-400">
          Extracted Tags & EXIF ({metadata.length})
        </h2>
        {metadata.length > 0 && (
          <input
            type="text"
            placeholder="Filter tags..."
            value={filter}
            onChange={(e) => setFilter(e.target.value)}
            className="rounded border border-neutral-800 bg-neutral-900 px-2 py-1 text-xs text-neutral-200 placeholder:text-neutral-500 focus:outline-none focus:border-neutral-600"
          />
        )}
      </div>

      <div className="rounded border border-neutral-800 p-3 max-h-96 overflow-y-auto">
        {isLoading ? (
          <p className="text-xs text-neutral-500">
            Loading extracted metadata…
          </p>
        ) : isError ? (
          <p className="text-xs text-red-400">Failed to load metadata tags.</p>
        ) : metadata.length === 0 ? (
          <p className="text-xs text-neutral-500">
            No extracted metadata rows found for this asset.
          </p>
        ) : filtered.length === 0 ? (
          <p className="text-xs text-neutral-500">
            No tags match &quot;{filter}&quot;.
          </p>
        ) : (
          <table className="w-full text-left text-xs font-mono">
            <thead className="border-b border-neutral-800 text-neutral-500">
              <tr>
                <th className="py-1 pr-3">Source</th>
                <th className="py-1 pr-3">Key</th>
                <th className="py-1">Value</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-900">
              {filtered.map((m, idx) => (
                <tr
                  key={`${m.source}-${m.key}-${idx}`}
                  className="hover:bg-neutral-900/50"
                >
                  <td className="py-1 pr-3 text-indigo-400">{m.source}</td>
                  <td className="py-1 pr-3 text-neutral-300 font-semibold">
                    {m.key}
                  </td>
                  <td className="py-1 text-neutral-200 break-all">{m.value}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
    </section>
  );
}

export default function AssetDetailPage() {
  const { id } = useParams<{ id: string }>();
  const assetId = id ? Number(id) : undefined;
  const [searchParams, setSearchParams] = useSearchParams();

  const rawDepth = Number(searchParams.get("depth"));
  const depth =
    !Number.isNaN(rawDepth) && rawDepth >= 1 && rawDepth <= 5 ? rawDepth : 2;

  const { data: asset, isLoading, isError } = useAsset(assetId);
  const { data: lineage, isLoading: isLineageLoading } = useAssetLineage(
    assetId,
    depth,
  );
  const { data: sync, isLoading: isSyncLoading } = useAssetSyncStatus(assetId);
  const retrySync = useRetrySync();

  if (!assetId || Number.isNaN(assetId))
    return <div className="p-6 text-red-400">Invalid asset id.</div>;
  if (isLoading) return <div className="p-6 text-neutral-400">Loading…</div>;
  if (isError || !asset)
    return <div className="p-6 text-red-400">Asset not found.</div>;

  const handleDepthChange = (newDepth: number) => {
    setSearchParams((prev) => {
      prev.set("depth", String(newDepth));
      return prev;
    });
  };

  return (
    <div className="space-y-6 p-6">
      <div className="flex items-start justify-between gap-4">
        <div className="flex items-start gap-4">
          <Thumbnail
            assetId={asset.id}
            thumbState={asset.thumbState}
            alt={asset.fileName}
            version={asset.fullHash ?? asset.fastHash}
            className="h-24 w-24 shrink-0 rounded-lg object-cover"
          />
          <div>
            <h1 className="text-xl font-semibold text-neutral-100">
              {asset.fileName}
            </h1>
            <p className="text-sm text-neutral-500">{asset.filePath}</p>
          </div>
        </div>
        <div className="flex items-center gap-2">
          <AssetPruneControl asset={asset} />
          <AssetDeleteControl asset={asset} />
        </div>
      </div>

      <section>
        <h2 className="mb-2 text-sm font-medium text-neutral-400">
          Media Preview
        </h2>
        <AssetMediaPreview asset={asset} lineage={lineage} />
      </section>

      <section>
        <div className="mb-2 flex items-center justify-between">
          <h2 className="text-sm font-medium text-neutral-400">Metadata</h2>
          {/* MISSING and TRASHED files aren't at their recorded path, so
              there is nothing to write inherited tags into (Restore first). */}
          {asset.lifecycleState !== "ARCHIVED" &&
            asset.lifecycleState !== "MISSING" &&
            asset.lifecycleState !== "TRASHED" && (
              <AssetInheritMetadataControl asset={asset} />
            )}
        </div>
        <dl className="rounded border border-neutral-800 p-3">
          <Field label="Node UUID" value={asset.nodeUuid} />
          <Field label="Size" value={`${asset.sizeBytes} bytes`} />
          <Field label="Indexing status" value={asset.indexingStatus} />
          <Field label="Graph status" value={asset.graphStatus} />
          <Field label="Lifecycle" value={asset.lifecycleState} />
          <Field label="Camera model" value={asset.cameraModel} />
          <Field label="Fast hash (xxHash64)" value={asset.fastHash} />
          <Field label="Full hash (BLAKE3-256)" value={asset.fullHash} />
        </dl>
      </section>

      <AssetMetadataInspector assetId={asset.id} />

      <section>
        <h2 className="mb-2 text-sm font-medium text-neutral-400">
          Sync status
        </h2>
        <div className="rounded border border-neutral-800 p-3">
          {isSyncLoading ? (
            <p className="text-sm text-neutral-500">Loading sync status…</p>
          ) : sync?.sync && sync.sync.length > 0 ? (
            <div className="space-y-3">
              {sync.sync.map((s) => (
                <div
                  key={s.remote}
                  className="flex items-start justify-between gap-4 text-sm"
                >
                  <div className="min-w-0">
                    <div className="flex flex-wrap items-center gap-2">
                      <span className="font-medium text-neutral-200">
                        {s.remote}
                      </span>
                      <span className="rounded bg-neutral-800 px-2 py-0.5 font-mono text-xs text-neutral-300">
                        {s.syncStatus}
                      </span>
                      {s.exhausted && (
                        <span className="rounded border border-red-800 bg-red-950 px-2 py-0.5 text-xs font-medium text-red-300">
                          Exhausted (max retries reached)
                        </span>
                      )}
                    </div>
                    {s.retryCount !== undefined && s.retryCount > 0 && (
                      <p className="mt-1 text-xs text-neutral-400">
                        Retries: {s.retryCount}
                      </p>
                    )}
                    {s.lastError && (
                      <p className="mt-1 break-all text-xs text-red-400">
                        Error: {s.lastError}
                      </p>
                    )}
                    {s.lastAttemptAt !== undefined && (
                      <p className="mt-1 text-xs text-neutral-500">
                        Last attempt:{" "}
                        {new Date(s.lastAttemptAt * 1000).toLocaleString()}
                      </p>
                    )}
                  </div>
                  {(s.syncStatus === "PUSH_FAILED" || s.exhausted) &&
                    asset.lifecycleState !== "ARCHIVED" && (
                      <button
                        onClick={() => retrySync.mutate(asset.id)}
                        disabled={retrySync.isPending}
                        className="shrink-0 rounded border border-neutral-700 bg-neutral-900 px-3 py-1 text-xs text-neutral-200 transition hover:bg-neutral-800 disabled:cursor-not-allowed disabled:opacity-50"
                      >
                        {retrySync.isPending ? "Retrying…" : "Retry"}
                      </button>
                    )}
                </div>
              ))}
            </div>
          ) : (
            <p className="text-sm text-neutral-500">No sync state yet.</p>
          )}
          {retrySync.isError && (
            <p className="mt-3 text-xs text-red-400">
              Failed to retry sync: {String(retrySync.error)}
            </p>
          )}
        </div>
      </section>

      <section>
        <div className="mb-2 flex items-center justify-between">
          <h2 className="text-sm font-medium text-neutral-400">
            Multi-hop Lineage Graph
          </h2>
          <div className="flex items-center gap-2 text-sm text-neutral-400">
            <span>Traversal Depth:</span>
            <select
              value={depth}
              onChange={(e) => handleDepthChange(Number(e.target.value))}
              className="rounded border border-neutral-800 bg-neutral-900 px-2 py-1 text-sm text-neutral-100 focus:outline-none focus:ring-1 focus:ring-indigo-500"
            >
              <option value={1}>1 hop</option>
              <option value={2}>2 hops</option>
              <option value={3}>3 hops</option>
              <option value={4}>4 hops</option>
              <option value={5}>5 hops</option>
            </select>
          </div>
        </div>
        {isLineageLoading ? (
          <p className="text-sm text-neutral-500">Loading lineage graph…</p>
        ) : lineage ? (
          <AssetGraphCanvas assetId={asset.id} lineage={lineage} />
        ) : (
          <p className="text-sm text-neutral-500">No known lineage data.</p>
        )}
      </section>
    </div>
  );
}
