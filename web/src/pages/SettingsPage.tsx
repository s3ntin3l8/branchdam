import { useContext, useMemo, useState } from "react";
import { ApiError } from "../api/client";
import type { SettingsField } from "../api/types";
import { DirtyFormContext, useDirtyFormProvider, useDirtyGuard } from "../hooks/useDirtyFormGuard";
import { FieldRow } from "../components/form/FieldRow";
import { NumberField } from "../components/form/NumberField";
import { SecretField } from "../components/form/SecretField";
import { SelectField } from "../components/form/SelectField";
import { TextField } from "../components/form/TextField";
import { ToggleField } from "../components/form/ToggleField";
import { RestartServerCard } from "../components/RestartServerButton";
import { useConfig, usePutSettings, useSettings } from "../hooks/queries";

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
  } else if (field.type === "stringList" || Array.isArray(field.value)) {
    const list = Array.isArray(field.value) ? field.value : [];
    if (list.length > 0) {
      display = list.join(", ");
    } else if (field.key === "authz.groups") {
      display = "(empty -- every authenticated user is admin)";
    } else {
      display = "(empty)";
    }
  } else if (field.value === undefined || field.value === null || field.value === "") {
    display = "(empty)";
  } else {
    display = String(field.value);
  }
  return <p className="rounded border border-neutral-800/60 bg-neutral-950/40 px-3 py-2 text-sm text-neutral-400">{display}</p>;
}

function renderInput(field: SettingsField, draft: unknown, onChange: (value: unknown) => void, secretsAvailable: boolean) {
  if (!field.editable || field.type === "stringList") {
    // No editable stringList field exists today -- authz.groups is the only
    // one and it's display-only -- so there's nothing to build an editor
    // for yet; render it the same as any other read-only field.
    return <ReadOnlyValue field={field} />;
  }
  if (field.secret) {
    // A PUT of a secret field returns 422 when BRANCHDAM_SECRET_KEY isn't
    // set (Store.Apply's seal-failure branch) -- disable the input rather
    // than let the operator type a value into a save that's guaranteed to
    // fail, on top of the page-level banner already saying so.
    return <SecretField hasValue={!!field.hasValue} value={draft as string} onChange={onChange} disabled={!secretsAvailable} />;
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
  secretsAvailable,
  onSave,
  onRevert,
}: {
  field: SettingsField;
  saving: boolean;
  secretsAvailable: boolean;
  onSave: (key: string, value: unknown, onError: () => void) => void;
  onRevert: (key: string, onError: () => void) => void;
}) {
  const { register } = useContext(DirtyFormContext);
  const { markDirty, markClean } = register();
  const baseline = field.secret ? "" : field.value;
  const [prevBaseline, setPrevBaseline] = useState(baseline);
  const [draft, setDraft] = useState<unknown>(baseline);
  const [dirty, setDirty] = useState(false);

  // A revert, another session's write, or the SSE-driven refetch can all
  // move field.value out from under a draft. Adjusted during render
  // (React's documented alternative to an effect for "reset derived state
  // when a prop changes") rather than in a useEffect, which would call
  // setState after the initial render and trigger a second one.
  //
  // The Save/Revert button handlers below already clear `dirty` optimistically
  // on click (and restore it on failure), so this is the fallback for the
  // other ways a field's baseline can move: an untouched draft always
  // resyncs, and a draft the operator abandoned mid-edit resyncs too, once
  // the world catches up to whatever they'd typed. A dirty draft that still
  // disagrees with a freshly changed baseline (a concurrent, unrelated
  // external edit) is left alone, so a nudge can't clobber in-progress typing.
  if (baseline !== prevBaseline) {
    setPrevBaseline(baseline);
    if (!dirty || draft === baseline) {
      setDraft(baseline);
      setDirty(false);
    }
  }

  const handleChange = (value: unknown) => {
    setDraft(value);
    setDirty(true);
    markDirty();
  };

  const secretEmpty = field.secret && draft === "";
  const secretBlocked = field.secret && !secretsAvailable;
  const numberInvalid = field.type === "int" && typeof draft === "number" && Number.isNaN(draft);
  const canSave = field.editable && dirty && !secretEmpty && !secretBlocked && !numberInvalid;

  return (
    <FieldRow
      label={field.label}
      doc={field.doc}
      source={field.source}
      pendingRestart={field.pendingRestart}
      readOnlyReason={field.readOnlyReason}
    >
      <div className="flex items-start gap-2">
        <div className="flex-1">{renderInput(field, draft, handleChange, secretsAvailable)}</div>
        <div className="flex shrink-0 gap-1 pt-0.5">
          {canSave && (
            <button
              type="button"
              onClick={() => {
                const submitted = draft;
                setDirty(false);
                markClean();
                if (field.secret) setDraft("");
                onSave(field.key, submitted, () => {
                  setDirty(true);
                  markDirty();
                  if (field.secret) setDraft(submitted);
                });
              }}
              disabled={saving}
              className="rounded bg-indigo-600 px-2 py-1 text-xs font-medium text-white hover:bg-indigo-500 disabled:opacity-50"
            >
              Save
            </button>
          )}
          {field.source === "override" && field.editable && (
            <button
              type="button"
              onClick={() => {
                setDirty(false);
                markClean();
                onRevert(field.key, () => { setDirty(true); markDirty(); });
              }}
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

function PathRewritesEditor({
  field,
  saving,
  onSave,
  onRevert,
}: {
  field?: SettingsField;
  saving: boolean;
  onSave: (key: string, value: unknown, onError: () => void) => void;
  onRevert: (key: string, onError: () => void) => void;
}) {
  const { register } = useContext(DirtyFormContext);
  const { markDirty, markClean } = register();
  const currentRules = useMemo(() => {
    if (!field || !Array.isArray(field.value)) return [] as Array<{ from: string; to: string }>;
    return field.value as Array<{ from: string; to: string }>;
  }, [field]);

  const [rules, setRules] = useState<Array<{ from: string; to: string }>>(currentRules);
  const [prevRules, setPrevRules] = useState(currentRules);
  const [dirty, setDirty] = useState(false);
  const [newFrom, setNewFrom] = useState("");
  const [newTo, setNewTo] = useState("");

  if (JSON.stringify(currentRules) !== JSON.stringify(prevRules)) {
    setPrevRules(currentRules);
    if (!dirty) {
      setRules(currentRules);
    }
  }

  const handleAdd = () => {
    if (!newFrom.trim() || !newTo.trim()) return;
    setRules((prev) => [...prev, { from: newFrom.trim(), to: newTo.trim() }]);
    setNewFrom("");
    setNewTo("");
    setDirty(true);
  };

  const handleDelete = (index: number) => {
    setRules((prev) => prev.filter((_, i) => i !== index));
    setDirty(true);
  };

  const handleUpdate = (index: number, key: "from" | "to", val: string) => {
    setRules((prev) => {
      const copy = [...prev];
      copy[index] = { ...copy[index], [key]: val };
      return copy;
    });
    setDirty(true);
  };

  const canSave = field?.editable && dirty;

  return (
    <div className="rounded-lg border border-neutral-800 bg-neutral-900/50 p-4">
      <div className="flex items-center justify-between mb-2">
        <div className="flex items-center gap-2">
          <h2 className="text-sm font-semibold uppercase tracking-wider text-neutral-400">
            Operator Path Rewrites (Tier-1 Resolution)
          </h2>
          {field && (
            <span
              className={`rounded px-1.5 py-0.5 text-[10px] font-medium tracking-wide uppercase ${
                field.source === "override"
                  ? "bg-amber-950/80 text-amber-300 border border-amber-800/60"
                  : "bg-neutral-800/80 text-neutral-400 border border-neutral-700/60"
              }`}
            >
              {field.source}
            </span>
          )}
        </div>
        <div className="flex gap-2">
          {canSave && (
            <button
              type="button"
              onClick={() => {
                const submitted = rules;
                setDirty(false);
                markClean();
                onSave("pathRewrites", submitted, () => { setDirty(true); markDirty(); });
              }}
              disabled={saving}
              className="rounded bg-indigo-600 px-2.5 py-1 text-xs font-medium text-white hover:bg-indigo-500 disabled:opacity-50"
            >
              Save Rewrites
            </button>
          )}
          {field?.source === "override" && field.editable && (
            <button
              type="button"
              onClick={() => {
                setDirty(false);
                markClean();
                onRevert("pathRewrites", () => { setDirty(true); markDirty(); });
              }}
              disabled={saving}
              className="rounded border border-neutral-700 px-2.5 py-1 text-xs text-neutral-300 hover:border-neutral-500 disabled:opacity-50"
            >
              Revert to config
            </button>
          )}
        </div>
      </div>
      <p className="mb-4 text-xs text-neutral-500">
        Host-to-container path transformation rules used when resolving project-file references.
      </p>

      {rules.length === 0 ? (
        <p className="mb-4 text-sm text-neutral-500">No operator path rewrites configured.</p>
      ) : (
        <div className="overflow-x-auto mb-4">
          <table className="w-full text-left text-sm text-neutral-300">
            <thead className="border-b border-neutral-800 text-xs font-semibold text-neutral-400">
              <tr>
                <th className="pb-2">Original Host Prefix</th>
                <th className="pb-2">Target Container Prefix</th>
                <th className="pb-2 w-16 text-right">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800/60 font-mono text-xs">
              {rules.map((rw, i) => (
                <tr key={i} className="hover:bg-neutral-800/30">
                  <td className="py-2 pr-4 text-amber-300">
                    <input
                      type="text"
                      value={rw.from}
                      onChange={(e) => handleUpdate(i, "from", e.target.value)}
                      className="w-full rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-xs text-amber-300 focus:border-indigo-500 focus:outline-none"
                    />
                  </td>
                  <td className="py-2 pr-4 text-emerald-300">
                    <input
                      type="text"
                      value={rw.to}
                      onChange={(e) => handleUpdate(i, "to", e.target.value)}
                      className="w-full rounded border border-neutral-800 bg-neutral-950 px-2 py-1 text-xs text-emerald-300 focus:border-indigo-500 focus:outline-none"
                    />
                  </td>
                  <td className="py-2 text-right">
                    <button
                      type="button"
                      onClick={() => handleDelete(i)}
                      className="rounded px-2 py-1 text-xs text-red-400 hover:bg-red-950/40 hover:text-red-300"
                    >
                      Delete
                    </button>
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}

      <div className="flex flex-wrap items-center gap-2 border-t border-neutral-800 pt-3">
        <input
          type="text"
          placeholder="Original host prefix (e.g. D:\Footage\)"
          value={newFrom}
          onChange={(e) => setNewFrom(e.target.value)}
          className="flex-1 min-w-[200px] rounded border border-neutral-800 bg-neutral-950 px-2.5 py-1.5 font-mono text-xs text-neutral-200 placeholder:text-neutral-600 focus:border-indigo-500 focus:outline-none"
        />
        <input
          type="text"
          placeholder="Target container prefix (e.g. /storage/projects/Footage/)"
          value={newTo}
          onChange={(e) => setNewTo(e.target.value)}
          className="flex-1 min-w-[200px] rounded border border-neutral-800 bg-neutral-950 px-2.5 py-1.5 font-mono text-xs text-neutral-200 placeholder:text-neutral-600 focus:border-indigo-500 focus:outline-none"
        />
        <button
          type="button"
          onClick={handleAdd}
          disabled={!newFrom.trim() || !newTo.trim()}
          className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-xs font-medium text-neutral-200 hover:bg-neutral-700 disabled:opacity-50"
        >
          Add Rule
        </button>
      </div>
    </div>
  );
}

export default function SettingsPage() {
  const { data: config, isLoading: configLoading } = useConfig();
  const { data: settings, isLoading: settingsLoading, error: settingsError } = useSettings();
  const putSettings = usePutSettings();
  const [fieldError, setFieldError] = useState<string | null>(null);
  const { dirtyCount, register } = useDirtyFormProvider();
  const blocker = useDirtyGuard(dirtyCount);

  const pathRewritesField = useMemo(() => {
    return settings?.fields.find((f) => f.key === "pathRewrites");
  }, [settings]);

  const grouped = useMemo(() => {
    if (!settings) return [] as Array<[string, SettingsField[]]>;
    const byGroup = new Map<string, SettingsField[]>();
    for (const f of settings.fields) {
      if (f.key === "pathRewrites") continue; // Rendered in dedicated PathRewritesEditor
      const list = byGroup.get(f.group) ?? [];
      list.push(f);
      byGroup.set(f.group, list);
    }
    return Array.from(byGroup.entries());
  }, [settings]);

  const handleSave = (key: string, value: unknown, onError: () => void) => {
    setFieldError(null);
    putSettings.mutate(
      { set: { [key]: value } },
      {
        onError: (err: unknown) => {
          setFieldError((err as { message?: string }).message || `Failed to save ${key}`);
          onError();
        },
      }
    );
  };

  const handleRevert = (key: string, onError: () => void) => {
    setFieldError(null);
    putSettings.mutate(
      { unset: [key] },
      {
        onError: (err: unknown) => {
          setFieldError((err as { message?: string }).message || `Failed to revert ${key}`);
          onError();
        },
      }
    );
  };

  return (
    <DirtyFormContext.Provider value={{ dirtyCount, register }}>
    <div className="p-6">
      <h1 className="mb-6 text-2xl font-bold text-white">System Settings</h1>

      {settings && (settings.pendingRestart?.length ?? 0) > 0 && (
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

      <RestartServerCard className="mb-8" />

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
                <SettingsFieldEditor
                  key={f.key}
                  field={f}
                  saving={putSettings.isPending}
                  secretsAvailable={settings?.secretsAvailable ?? true}
                  onSave={handleSave}
                  onRevert={handleRevert}
                />
              ))}
            </div>
          </div>
        ))
      )}

      <PathRewritesEditor
        field={pathRewritesField}
        saving={putSettings.isPending}
        onSave={handleSave}
        onRevert={handleRevert}
      />
    </div>
    {blocker.state === "blocked" && (
      <div className="fixed inset-0 z-50 flex items-center justify-center bg-black/60">
        <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow-xl max-w-sm">
          <h2 className="text-lg font-semibold text-white mb-2">Unsaved Changes</h2>
          <p className="text-sm text-neutral-400 mb-4">
            You have unsaved settings changes. Leave without saving?
          </p>
          <div className="flex justify-end gap-2">
            <button type="button" onClick={() => blocker.reset()}
              className="rounded bg-neutral-800 px-4 py-2 text-sm text-neutral-300 hover:bg-neutral-700">Stay</button>
            <button type="button" onClick={() => blocker.proceed()}
              className="rounded bg-red-700 px-4 py-2 text-sm text-white hover:bg-red-600">Leave</button>
          </div>
        </div>
      </div>
    )}
    </DirtyFormContext.Provider>
  );
}
