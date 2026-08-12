import { useParams } from "react-router";
import { useAsset, useAssetGraph } from "../hooks/queries";
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

  const { data: asset, isLoading, isError } = useAsset(assetId);
  const { data: graph } = useAssetGraph(assetId);

  if (!assetId || Number.isNaN(assetId)) return <div className="p-6 text-red-400">Invalid asset id.</div>;
  if (isLoading) return <div className="p-6 text-neutral-400">Loading…</div>;
  if (isError || !asset) return <div className="p-6 text-red-400">Asset not found.</div>;

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
        <h2 className="mb-2 text-sm font-medium text-neutral-400">Lineage (direct parents/children)</h2>
        {graph ? <AssetGraphCanvas assetId={asset.id} graph={graph} /> : <p className="text-sm text-neutral-500">Loading graph…</p>}
      </section>
    </div>
  );
}
