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
  // Present for a settings.Field-backed row (SettingsPage), whose registry
  // key is stable and unique; omitted for a StorageHealthPage row, which
  // has no such key and identifies itself by label alone. When present,
  // it becomes a `field-row-<fieldKey>` data-testid so tests can scope a
  // query to one row instead of relying on DOM-structure traversal (e.g.
  // `.closest("div")`) that breaks the moment this component's markup
  // shape changes.
  fieldKey?: string;
  label: string;
  doc?: string;
  source: SettingsFieldSource;
  pendingRestart?: boolean;
  readOnlyReason?: string;
  children: React.ReactNode;
}

export function FieldRow({ fieldKey, label, doc, source, pendingRestart, readOnlyReason, children }: FieldRowProps) {
  return (
    <div
      data-testid={fieldKey ? `field-row-${fieldKey}` : undefined}
      className="border-b border-neutral-800/60 py-3 last:border-b-0"
    >
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
