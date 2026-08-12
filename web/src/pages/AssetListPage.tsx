import { Link } from "react-router";
import { useAssets } from "../hooks/queries";

const statusColor: Record<string, string> = {
  LINKED: "text-emerald-400",
  UNLINKED: "text-neutral-500",
  NEEDS_REVIEW: "text-amber-400",
  ROOT: "text-sky-400",
};

export default function AssetListPage() {
  const { data, isLoading, isError, error } = useAssets({ limit: 100 });

  if (isLoading) return <div className="p-6 text-neutral-400">Loading assets…</div>;
  if (isError) return <div className="p-6 text-red-400">Failed to load assets: {String(error)}</div>;

  const assets = data?.assets ?? [];

  return (
    <div className="p-6">
      <h1 className="mb-4 text-xl font-semibold">Assets</h1>
      {assets.length === 0 ? (
        <p className="text-neutral-500">No assets indexed yet. Trigger a scan to get started.</p>
      ) : (
        <table className="w-full text-left text-sm">
          <thead className="border-b border-neutral-800 text-neutral-400">
            <tr>
              <th className="py-2 pr-4">Path</th>
              <th className="py-2 pr-4">Tier status</th>
              <th className="py-2 pr-4">Graph status</th>
              <th className="py-2 pr-4">Hash</th>
            </tr>
          </thead>
          <tbody>
            {assets.map((a) => (
              <tr key={a.id} className="border-b border-neutral-900 hover:bg-neutral-900">
                <td className="py-2 pr-4">
                  <Link to={`/assets/${a.id}`} className="text-sky-400 hover:underline">
                    {a.filePath}
                  </Link>
                </td>
                <td className="py-2 pr-4 text-neutral-400">{a.indexingStatus}</td>
                <td className={`py-2 pr-4 ${statusColor[a.graphStatus] ?? ""}`}>{a.graphStatus}</td>
                <td className="py-2 pr-4 font-mono text-xs text-neutral-500">{a.fastHash ?? "—"}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}
