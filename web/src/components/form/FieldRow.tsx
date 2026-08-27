import type { SettingsFieldSource } from "../../api/types";

function ProvenanceChip({ source }: { source: SettingsFieldSource }) {
  if (source === "override") {
    return (
      <span className="rounded-full border border-indigo-700/60 bg-indigo-950/40 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-indigo-300">
        UI override
      </span>
    );
  }
  return (
    <span className="rounded-full border border-neutral-700 bg-neutral-900 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-neutral-500">
      from config.yaml / .env
    </span>
  );
}

interface FieldRowProps {
  label: string;
  doc?: string;
  source: SettingsFieldSource;
  pendingRestart?: boolean;
  readOnlyReason?: string;
  children: React.ReactNode;
}

export function FieldRow({ label, doc, source, pendingRestart, readOnlyReason, children }: FieldRowProps) {
  return (
    <div className="border-b border-neutral-800/60 py-3 last:border-b-0">
      <div className="mb-1 flex flex-wrap items-center gap-2">
        <span className="text-sm font-medium text-neutral-200">{label}</span>
        <ProvenanceChip source={source} />
        {pendingRestart && (
          <span className="rounded-full border border-amber-700/60 bg-amber-950/40 px-2 py-0.5 text-[10px] font-semibold uppercase tracking-wide text-amber-400">
            Applies on restart
          </span>
        )}
      </div>
      {doc && <p className="mb-2 text-xs text-neutral-500">{doc}</p>}
      {children}
      {readOnlyReason && <p className="mt-1 text-xs text-neutral-600">{readOnlyReason}</p>}
    </div>
  );
}
