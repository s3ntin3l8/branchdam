import { useState } from "react";
import { useParams, useSearchParams } from "react-router";
import {
  useAsset,
  useAssetLineage,
  useAssetSyncStatus,
  useInheritMetadata,
  usePruneCache,
  useRetrySync,
  useStorageLocations,
} from "../hooks/queries";
import AssetGraphCanvas from "../components/AssetGraphCanvas";
import Thumbnail from "../components/Thumbnail";
import type { Asset } from "../api/types";

// identityTags are the two tags Plan (internal/metadata) always emits
// regardless of whether there was anything to inherit -- excluded from the
// "N tag(s) inherited" count so re-running against an already-fully-tagged
// child (or one with no eligible parent field at all) doesn't read as
// having inherited something.
const identityTags = new Set(["XMP-dc:Identifier", "XMP-xmpMM:DerivedFrom"]);

// AssetInheritMetadataControl backs POST /api/v1/assets/{id}/inherit-metadata
// (#54) -- the client for this endpoint (api.inheritMetadata) has existed
// since #156 but had no caller anywhere in the SPA until now (#186).
// Attempt-then-report, same shape as AssetPruneControl below: the endpoint's
// eligibility rules (a resolved Tier-1/2 ancestry parent that isn't
// ARCHIVED/MISSING, a writable location, exiftool available) are involved
// enough that re-deriving them client-side to gate the button would drift
// from the server's actual logic -- the 409/422/503 response body's message
// is surfaced as-is instead.
function AssetInheritMetadataControl({ asset }: { asset: Asset }) {
  const inheritMetadata = useInheritMetadata();
  const [inheritedCount, setInheritedCount] = useState<number | null>(null);

  const handleClick = () => {
    setInheritedCount(null);
    inheritMetadata.mutate(asset.id, {
      onSuccess: (data) => {
        const count = Object.keys(data.inherited).filter((tag) => !identityTags.has(tag)).length;
        setInheritedCount(count);
      },
    });
  };

  return (
    <span className="flex items-center gap-2 text-xs">
      {inheritMetadata.isError && (
        <span className="text-red-400">Inherit failed: {String(inheritMetadata.error)}</span>
      )}
      {inheritedCount !== null && !inheritMetadata.isError && (
        <span className="text-neutral-400">
          {inheritedCount > 0 ? `Inherited ${inheritedCount} tag${inheritedCount === 1 ? "" : "s"}.` : "Nothing new to inherit."}
        </span>
      )}
      <button
        type="button"
        onClick={handleClick}
        disabled={inheritMetadata.isPending}
        className="rounded border border-neutral-700 bg-neutral-800 px-2 py-1 text-xs text-neutral-300 hover:bg-neutral-700 disabled:opacity-50"
      >
        {inheritMetadata.isPending ? "Inheriting…" : "Inherit Metadata"}
      </button>
    </span>
  );
}

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="grid grid-cols-3 gap-2 py-1 text-sm">
      <dt className="text-neutral-500">{label}</dt>
      <dd className="col-span-2 break-all font-mono text-neutral-200">{value ?? "—"}</dd>
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
  const location = locationsData?.locations.find((l) => l.id === asset.storageLocationId);

  const [checked, setChecked] = useState<"eligible" | "ineligible" | null>(null);
  // null = not attempted; otherwise the actual outcome for THIS asset's
  // candidate, not just "the request succeeded" -- Execute re-verifies
  // eligibility right before deleting, so a 200 response can still report
  // purged=false (e.g. the Tier-3 master went MISSING between the dry-run
  // and this click), which must not be shown as "Cache purged."
  const [purgeResult, setPurgeResult] = useState<{ purged: boolean; error?: string } | null>(null);
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
      { onSuccess: (res) => setChecked(res.candidates.length > 0 ? "eligible" : "ineligible") },
    );
  };

  const confirmPurge = () => {
    pruneCache.mutate(
      { storageLocationId: asset.storageLocationId, nodeIds: [asset.id], execute: true },
      {
        onSuccess: (res) => {
          setChecked(null);
          const outcome = res.candidates.find((c) => c.nodeId === asset.id);
          setPurgeResult(outcome ? { purged: outcome.purged, error: outcome.error } : { purged: false });
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
        <button type="button" onClick={() => setPurgeResult(null)} className="underline hover:text-red-300">
          Dismiss
        </button>
      </span>
    );
  }

  if (checked === "ineligible") {
    return (
      <span className="text-xs text-neutral-500">
        Not eligible for pruning.{" "}
        <button type="button" onClick={() => setChecked(null)} className="underline hover:text-neutral-300">
          Dismiss
        </button>
      </span>
    );
  }

  if (checked === "eligible") {
    return (
      <span className="flex items-center gap-2 text-xs">
        {pruneCache.isError && <span className="text-red-400">Purge failed: {String(pruneCache.error)}</span>}
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
      {pruneCache.isError && <span className="mr-2 text-xs text-red-400">Check failed: {String(pruneCache.error)}</span>}
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

export default function AssetDetailPage() {
  const { id } = useParams<{ id: string }>();
  const assetId = id ? Number(id) : undefined;
  const [searchParams, setSearchParams] = useSearchParams();

  const rawDepth = Number(searchParams.get("depth"));
  const depth = !Number.isNaN(rawDepth) && rawDepth >= 1 && rawDepth <= 5 ? rawDepth : 2;

  const { data: asset, isLoading, isError } = useAsset(assetId);
  const { data: lineage, isLoading: isLineageLoading } = useAssetLineage(assetId, depth);
  const { data: sync, isLoading: isSyncLoading } = useAssetSyncStatus(assetId);
  const retrySync = useRetrySync();

  if (!assetId || Number.isNaN(assetId)) return <div className="p-6 text-red-400">Invalid asset id.</div>;
  if (isLoading) return <div className="p-6 text-neutral-400">Loading…</div>;
  if (isError || !asset) return <div className="p-6 text-red-400">Asset not found.</div>;

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
            className="h-24 w-24 shrink-0 rounded-lg object-cover"
          />
          <div>
            <h1 className="text-xl font-semibold">{asset.fileName}</h1>
            <p className="text-sm text-neutral-500">{asset.filePath}</p>
          </div>
        </div>
        <AssetPruneControl asset={asset} />
      </div>

      <section>
        <div className="mb-2 flex items-center justify-between">
          <h2 className="text-sm font-medium text-neutral-400">Metadata</h2>
          {asset.lifecycleState !== "ARCHIVED" && <AssetInheritMetadataControl asset={asset} />}
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

      <section>
        <h2 className="mb-2 text-sm font-medium text-neutral-400">Sync status</h2>
        <div className="rounded border border-neutral-800 p-3">
          {isSyncLoading ? (
            <p className="text-sm text-neutral-500">Loading sync status…</p>
          ) : sync?.sync && sync.sync.length > 0 ? (
            <div className="space-y-3">
              {sync.sync.map((s) => (
                <div key={s.remote} className="flex items-start justify-between gap-4 text-sm">
                  <div className="min-w-0">
                    <div className="flex items-center gap-2">
                      <span className="font-medium text-neutral-200">{s.remote}</span>
                      <span className="rounded bg-neutral-800 px-2 py-0.5 font-mono text-xs text-neutral-300">
                        {s.syncStatus}
                      </span>
                    </div>
                    {s.lastError && <p className="mt-1 break-all text-xs text-red-400">Error: {s.lastError}</p>}
                    {s.lastAttemptAt !== undefined && (
                      <p className="mt-1 text-xs text-neutral-500">
                        Last attempt: {new Date(s.lastAttemptAt * 1000).toLocaleString()}
                      </p>
                    )}
                  </div>
                  {s.syncStatus === "PUSH_FAILED" && asset.lifecycleState !== "ARCHIVED" && (
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
            <p className="mt-3 text-xs text-red-400">Failed to retry sync: {String(retrySync.error)}</p>
          )}
        </div>
      </section>

      <section>
        <div className="mb-2 flex items-center justify-between">
          <h2 className="text-sm font-medium text-neutral-400">Multi-hop Lineage Graph</h2>
          <div className="flex items-center gap-2 text-sm text-neutral-400">
            <span>Traversal Depth:</span>
            <select
              value={depth}
              onChange={(e) => handleDepthChange(Number(e.target.value))}
              className="rounded border border-neutral-800 bg-neutral-900 px-2 py-1 text-sm text-white focus:outline-none focus:ring-1 focus:ring-indigo-500"
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
