import { useConfig, usePathRewrites } from "../hooks/queries";

export default function SettingsPage() {
  const { data: config, isLoading: configLoading } = useConfig();
  const { data: pathRewrites, isLoading: rewritesLoading } = usePathRewrites();

  return (
    <div className="p-6">
      <h1 className="mb-6 text-2xl font-bold text-white">System Settings</h1>

      <div className="mb-8 rounded-lg border border-neutral-800 bg-neutral-900/50 p-4">
        <h2 className="mb-2 text-sm font-semibold uppercase tracking-wider text-neutral-400">Server Info</h2>
        <p className="text-sm text-neutral-200">
          Version: <span className="font-mono text-emerald-400">{configLoading ? "Loading…" : config?.version || "unknown"}</span>
        </p>
      </div>

      <div className="rounded-lg border border-neutral-800 bg-neutral-900/50 p-4">
        <h2 className="mb-2 text-sm font-semibold uppercase tracking-wider text-neutral-400">
          Operator Path Rewrites (Tier-1 Resolution)
        </h2>
        <p className="mb-4 text-xs text-neutral-500">
          Host-to-container path transformation rules used when resolving project-file references.
        </p>

        {rewritesLoading ? (
          <p className="text-sm text-neutral-400">Loading path rewrites…</p>
        ) : !pathRewrites || pathRewrites.length === 0 ? (
          <p className="text-sm text-neutral-500">No operator path rewrites configured.</p>
        ) : (
          <div className="overflow-x-auto">
            <table className="w-full text-left text-sm text-neutral-300">
              <thead className="border-b border-neutral-800 text-xs font-semibold text-neutral-400">
                <tr>
                  <th className="pb-2">Original Host Prefix</th>
                  <th className="pb-2">Target Container Prefix</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-neutral-800/60 font-mono text-xs">
                {pathRewrites.map((rw, i) => (
                  <tr key={i} className="hover:bg-neutral-800/30">
                    <td className="py-2.5 pr-4 text-amber-300">{rw.from}</td>
                    <td className="py-2.5 text-emerald-300">{rw.to}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </div>
    </div>
  );
}
