import { useMemo, useState } from "react";
import { ApiError } from "../api/client";
import type { SettingsField } from "../api/types";
import { FieldRow } from "../components/form/FieldRow";
import { NumberField } from "../components/form/NumberField";
import { SecretField } from "../components/form/SecretField";
import { SelectField } from "../components/form/SelectField";
import { TextField } from "../components/form/TextField";
import { ToggleField } from "../components/form/ToggleField";
import { useConfig, usePathRewrites, usePutSettings, useSettings } from "../hooks/queries";

// The registry's Field.Validate enum choices (internal/settings/registry.go)
// aren't part of the wire DTO -- there's no generated client here (see
// CLAUDE.md), so this mirrors the server's two oneOf(...) validators by
// hand, same as api/types.ts itself. If a validator's allowed values ever
// change, this map needs updating alongside it; a mismatch only means a
// stale dropdown, since the server is still the one that enforces the rule.
const SELECT_OPTIONS: Record<string, string[]> = {
  logLevel: ["debug", "info", "warn", "error"],
  "workers.fullHashPolicy": ["always", "tier3_and_collision", "never"],
};

function ReadOnlyValue({ field }: { field: SettingsField }) {
  let display: string;
  if (field.secret) {
    display = field.hasValue ? "Set (hidden)" : "Not set";
  } else if (Array.isArray(field.value)) {
    display = field.value.length > 0 ? field.value.join(", ") : "(empty -- every authenticated user is admin)";
  } else if (field.value === undefined || field.value === null || field.value === "") {
    display = "(empty)";
  } else {
    display = String(field.value);
  }
  return <p className="rounded border border-neutral-800/60 bg-neutral-950/40 px-3 py-2 text-sm text-neutral-400">{display}</p>;
}

function renderInput(field: SettingsField, draft: unknown, onChange: (value: unknown) => void) {
  if (!field.editable || field.type === "stringList") {
    // No editable stringList field exists today -- authz.groups is the only
    // one and it's display-only -- so there's nothing to build an editor
    // for yet; render it the same as any other read-only field.
    return <ReadOnlyValue field={field} />;
  }
  if (field.secret) {
    return <SecretField hasValue={!!field.hasValue} value={draft as string} onChange={onChange} />;
  }
  const options = SELECT_OPTIONS[field.key];
  if (options) {
    return <SelectField value={draft as string} options={options} onChange={onChange} />;
  }
  switch (field.type) {
    case "bool":
      return <ToggleField checked={draft as boolean} onChange={onChange} />;
    case "int":
      return <NumberField value={draft as number} onChange={onChange} />;
    default:
      return <TextField value={draft as string} onChange={onChange} />;
  }
}

function SettingsFieldEditor({
  field,
  saving,
  onSave,
  onRevert,
}: {
  field: SettingsField;
  saving: boolean;
  onSave: (key: string, value: unknown) => void;
  onRevert: (key: string) => void;
}) {
  const baseline = field.secret ? "" : field.value;
  const [prevBaseline, setPrevBaseline] = useState(baseline);
  const [draft, setDraft] = useState<unknown>(baseline);
  const [dirty, setDirty] = useState(false);

  // A revert, another session's write, or the SSE-driven refetch can all
  // move field.value out from under an untouched draft. Adjusted during
  // render (React's documented alternative to an effect for "reset derived
  // state when a prop changes") rather than in a useEffect, which would
  // call setState after the initial render and trigger a second one. A
  // dirty (in-progress, unsaved) draft is deliberately left alone so a
  // concurrent nudge can't clobber what the operator is mid-typing.
  if (baseline !== prevBaseline) {
    setPrevBaseline(baseline);
    if (!dirty) setDraft(baseline);
  }

  const handleChange = (value: unknown) => {
    setDraft(value);
    setDirty(true);
  };

  const secretEmpty = field.secret && draft === "";
  const numberInvalid = field.type === "int" && typeof draft === "number" && Number.isNaN(draft);
  const canSave = field.editable && dirty && !secretEmpty && !numberInvalid;

  return (
    <FieldRow
      label={field.label}
      doc={field.doc}
      source={field.source}
      pendingRestart={field.pendingRestart}
      readOnlyReason={field.readOnlyReason}
    >
      <div className="flex items-start gap-2">
        <div className="flex-1">{renderInput(field, draft, handleChange)}</div>
        <div className="flex shrink-0 gap-1 pt-0.5">
          {canSave && (
            <button
              type="button"
              onClick={() => onSave(field.key, draft)}
              disabled={saving}
              className="rounded bg-indigo-600 px-2 py-1 text-xs font-medium text-white hover:bg-indigo-500 disabled:opacity-50"
            >
              Save
            </button>
          )}
          {field.source === "override" && field.editable && (
            <button
              type="button"
              onClick={() => onRevert(field.key)}
              disabled={saving}
              className="rounded border border-neutral-700 px-2 py-1 text-xs text-neutral-300 hover:border-neutral-500 disabled:opacity-50"
            >
              Revert to config
            </button>
          )}
        </div>
      </div>
    </FieldRow>
  );
}

export default function SettingsPage() {
  const { data: config, isLoading: configLoading } = useConfig();
  const { data: pathRewrites, isLoading: rewritesLoading } = usePathRewrites();
  const { data: settings, isLoading: settingsLoading, error: settingsError } = useSettings();
  const putSettings = usePutSettings();
  const [fieldError, setFieldError] = useState<string | null>(null);

  const grouped = useMemo(() => {
    if (!settings) return [] as Array<[string, SettingsField[]]>;
    const byGroup = new Map<string, SettingsField[]>();
    for (const f of settings.fields) {
      const list = byGroup.get(f.group) ?? [];
      list.push(f);
      byGroup.set(f.group, list);
    }
    return Array.from(byGroup.entries());
  }, [settings]);

  const handleSave = (key: string, value: unknown) => {
    setFieldError(null);
    putSettings.mutate(
      { set: { [key]: value } },
      { onError: (err: unknown) => setFieldError((err as { message?: string }).message || `Failed to save ${key}`) }
    );
  };

  const handleRevert = (key: string) => {
    setFieldError(null);
    putSettings.mutate(
      { unset: [key] },
      { onError: (err: unknown) => setFieldError((err as { message?: string }).message || `Failed to revert ${key}`) }
    );
  };

  return (
    <div className="p-6">
      <h1 className="mb-6 text-2xl font-bold text-white">System Settings</h1>

      {settings && settings.pendingRestart.length > 0 && (
        <div className="mb-6 rounded-lg border border-amber-800/60 bg-amber-950/30 p-4 text-sm text-amber-300">
          <span className="font-semibold">Restart required</span> to apply: {settings.pendingRestart.join(", ")}
        </div>
      )}

      {settings && !settings.secretsAvailable && (
        <div className="mb-6 rounded-lg border border-red-800/60 bg-red-950/30 p-4 text-sm text-red-300">
          Secret storage is unavailable (<span className="font-mono">BRANCHDAM_SECRET_KEY</span> is not set) -- secret
          fields cannot be changed until it is configured.
        </div>
      )}

      {fieldError && (
        <div className="mb-6 rounded-lg border border-red-800/60 bg-red-950/30 p-4 text-sm text-red-300">{fieldError}</div>
      )}

      <div className="mb-8 rounded-lg border border-neutral-800 bg-neutral-900/50 p-4">
        <h2 className="mb-2 text-sm font-semibold uppercase tracking-wider text-neutral-400">Server Info</h2>
        <p className="text-sm text-neutral-200">
          Version: <span className="font-mono text-emerald-400">{configLoading ? "Loading…" : config?.version || "unknown"}</span>
        </p>
      </div>

      {settingsLoading ? (
        <p className="mb-8 text-sm text-neutral-400">Loading settings…</p>
      ) : settingsError ? (
        <p className="mb-8 text-sm text-red-400">
          {settingsError instanceof ApiError && settingsError.status === 403
            ? "Admin access is required to view settings."
            : "Failed to load settings."}
        </p>
      ) : (
        grouped.map(([group, fields]) => (
          <div key={group} className="mb-6 rounded-lg border border-neutral-800 bg-neutral-900/50 p-4">
            <h2 className="mb-2 text-sm font-semibold uppercase tracking-wider text-neutral-400">{group}</h2>
            <div>
              {fields.map((f) => (
                <SettingsFieldEditor key={f.key} field={f} saving={putSettings.isPending} onSave={handleSave} onRevert={handleRevert} />
              ))}
            </div>
          </div>
        ))
      )}

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
