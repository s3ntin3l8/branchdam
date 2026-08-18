import { useParams, useSearchParams } from "react-router";
import { useAsset, useAssetLineage, useAssetSyncStatus, useRetrySync } from "../hooks/queries";
import AssetGraphCanvas from "../components/AssetGraphCanvas";

function Field({ label, value }: { label: string; value: React.ReactNode }) {
  return (
    <div className="grid grid-cols-3 gap-2 py-1 text-sm">
      <dt className="text-neutral-500">{label}</dt>
      <dd className="col-span-2 break-all font-mono text-neutral-200">{value ?? "—"}</dd>
    </div>
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
      <div>
        <h1 className="text-xl font-semibold">{asset.fileName}</h1>
        <p className="text-sm text-neutral-500">{asset.filePath}</p>
      </div>

      <section>
        <h2 className="mb-2 text-sm font-medium text-neutral-400">Metadata</h2>
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
