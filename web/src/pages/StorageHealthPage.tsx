import { useState } from "react";
import { usePruneCache, usePutStorageLocation, useStorageHealth } from "../hooks/queries";
import type { PruneCandidate, StorageLocationHealth, StorageLocationSafeField } from "../api/types";
import { FieldRow } from "../components/form/FieldRow";
import { NumberField } from "../components/form/NumberField";
import { TextField } from "../components/form/TextField";
import { ToggleField } from "../components/form/ToggleField";

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB", "TB", "PB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return `${parseFloat((bytes / Math.pow(k, i)).toFixed(1))} ${sizes[i]}`;
}

// PruneControl is a two-step flow: "Purge Cache" runs a dry-run plan and
// shows the candidate count/bytes; a second, explicit click ("Confirm
// Purge") re-posts with execute:true. Mirrors the repo's existing feedback
// pattern (IngestPage/AuditQueuePage) -- isPending disables the button and
// swaps its label, errors render as inline text -- no toast library exists
// in this codebase, so this deliberately doesn't add one.
function PruneControl({ locationId }: { locationId: number }) {
  const [plan, setPlan] = useState<PruneCandidate[] | null>(null);
  const [result, setResult] = useState<PruneCandidate[] | null>(null);
  const pruneCache = usePruneCache();

  const runPlan = () => {
    setResult(null);
    pruneCache.mutate(
      { storageLocationId: locationId },
      { onSuccess: (res) => setPlan(res.candidates) },
    );
  };

  const runExecute = () => {
    pruneCache.mutate(
      { storageLocationId: locationId, execute: true },
      {
        onSuccess: (res) => {
          setPlan(null);
          setResult(res.candidates);
        },
      },
    );
  };

  const cancel = () => setPlan(null);

  if (result) {
    const purged = result.filter((c) => c.purged).length;
    const failed = result.length - purged;
    return (
      <div className="rounded bg-neutral-800/50 p-3 text-xs text-neutral-300 border border-neutral-700">
        <p>
          Purged {purged} file{purged === 1 ? "" : "s"}
          {failed > 0 ? `, ${failed} failed` : ""}.
        </p>
        {failed > 0 && (
          <ul className="mt-1 space-y-0.5 text-red-400">
            {result
              .filter((c) => !c.purged)
              .map((c) => (
                <li key={c.nodeId} className="font-mono break-all">
                  {c.filePath}: {c.error}
                </li>
              ))}
          </ul>
        )}
        <button
          type="button"
          onClick={() => setResult(null)}
          className="mt-2 rounded bg-neutral-700 px-2 py-1 text-xs text-neutral-200 hover:bg-neutral-600"
        >
          Dismiss
        </button>
      </div>
    );
  }

  if (plan) {
    const totalBytes = plan.reduce((sum, c) => sum + c.sizeBytes, 0);
    return (
      <div className="rounded bg-neutral-800/50 p-3 text-xs text-neutral-300 border border-neutral-700 space-y-2">
        <p>
          {plan.length === 0
            ? "No files are eligible for pruning right now."
            : `${plan.length} file${plan.length === 1 ? "" : "s"} eligible (${formatBytes(totalBytes)}).`}
        </p>
        {pruneCache.isError && (
          <p className="text-red-400">Purge failed: {String(pruneCache.error)}</p>
        )}
        <div className="flex gap-2">
          {plan.length > 0 && (
            <button
              type="button"
              onClick={runExecute}
              disabled={pruneCache.isPending}
              className="rounded bg-red-900 px-2 py-1 text-xs text-red-200 hover:bg-red-800 disabled:opacity-50"
            >
              {pruneCache.isPending ? "Purging…" : "Confirm Purge"}
            </button>
          )}
          <button
            type="button"
            onClick={cancel}
            className="rounded bg-neutral-700 px-2 py-1 text-xs text-neutral-200 hover:bg-neutral-600"
          >
            Cancel
          </button>
        </div>
      </div>
    );
  }

  return (
    <div>
      {pruneCache.isError && (
        <p className="mb-1 text-xs text-red-400">Plan failed: {String(pruneCache.error)}</p>
      )}
      <button
        type="button"
        onClick={runPlan}
        disabled={pruneCache.isPending}
        className="rounded bg-neutral-800 px-2 py-1 text-xs text-neutral-300 border border-neutral-700 hover:bg-neutral-700 disabled:opacity-50"
      >
        {pruneCache.isPending ? "Checking…" : "Purge Cache"}
      </button>
    </div>
  );
}

// StorageLocationDraft holds the five non-enabled safe fields as one unit --
// unlike SettingsFieldEditor (one field, one Save), this form batches an
// edit across several fields into a single PUT, so "dirty" and the
// baseline-resync check operate on the whole object rather than per-field.
interface StorageLocationDraft {
  name: string;
  watch: boolean;
  sweep: boolean;
  sweepIntervalSecs: number;
  cacheTtlHours: number;
}

function draftFromLoc(loc: StorageLocationHealth): StorageLocationDraft {
  return {
    name: loc.name,
    watch: loc.watch,
    sweep: loc.sweep,
    sweepIntervalSecs: loc.sweepIntervalSecs,
    cacheTtlHours: loc.cacheTtlHours,
  };
}

function isOverridden(loc: StorageLocationHealth, field: StorageLocationSafeField): boolean {
  return loc.overriddenFields.includes(field);
}

// StorageLocationEditForm edits the six safe fields inline on
// LocationGaugeCard. useStorageHealth polls every 10s and useEventStream
// invalidates it on every SSE nudge, so the draft must survive a background
// refetch while the form is open -- same baseline-resync-during-render
// pattern as SettingsFieldEditor (SettingsPage.tsx), applied to the whole
// draft object at once via a JSON key rather than per field.
function StorageLocationEditForm({ loc, onClose }: { loc: StorageLocationHealth; onClose: () => void }) {
  const putLocation = usePutStorageLocation();
  const baseline = draftFromLoc(loc);
  const baselineKey = JSON.stringify(baseline);
  const [prevBaselineKey, setPrevBaselineKey] = useState(baselineKey);
  const [draft, setDraft] = useState<StorageLocationDraft>(baseline);
  const [dirty, setDirty] = useState(false);
  const [error, setError] = useState<string | null>(null);

  if (baselineKey !== prevBaselineKey) {
    setPrevBaselineKey(baselineKey);
    if (!dirty) {
      setDraft(baseline);
    }
  }

  const update = <K extends keyof StorageLocationDraft>(key: K, value: StorageLocationDraft[K]) => {
    setDraft((d) => ({ ...d, [key]: value }));
    setDirty(true);
    setError(null);
  };

  const nameInvalid = draft.name.trim() === "";
  const sweepSecsInvalid = Number.isNaN(draft.sweepIntervalSecs) || draft.sweepIntervalSecs < 0;
  const cacheTtlInvalid = Number.isNaN(draft.cacheTtlHours) || draft.cacheTtlHours < 0;
  const canSave = dirty && !nameInvalid && !sweepSecsInvalid && !cacheTtlInvalid;

  const handleSave = () => {
    const set: Partial<Record<StorageLocationSafeField, unknown>> = {};
    if (draft.name !== baseline.name) set.name = draft.name;
    if (draft.watch !== baseline.watch) set.watch = draft.watch;
    if (draft.sweep !== baseline.sweep) set.sweep = draft.sweep;
    if (draft.sweepIntervalSecs !== baseline.sweepIntervalSecs) set.sweepIntervalSecs = draft.sweepIntervalSecs;
    if (loc.prunable && draft.cacheTtlHours !== baseline.cacheTtlHours) set.cacheTtlHours = draft.cacheTtlHours;
    if (Object.keys(set).length === 0) {
      setDirty(false);
      return;
    }
    setError(null);
    putLocation.mutate(
      { id: loc.id, input: { set } },
      {
        onSuccess: () => setDirty(false),
        onError: (err: unknown) => setError((err as { message?: string }).message || "Failed to save"),
      }
    );
  };

  const handleResetToConfig = () => {
    setError(null);
    putLocation.mutate(
      { id: loc.id, input: { unset: loc.overriddenFields } },
      {
        onSuccess: () => setDirty(false),
        onError: (err: unknown) => setError((err as { message?: string }).message || "Failed to reset to config"),
      }
    );
  };

  const handleToggleEnabled = () => {
    setError(null);
    putLocation.mutate(
      { id: loc.id, input: { set: { enabled: loc.disabled } } },
      {
        onError: (err: unknown) =>
          setError((err as { message?: string }).message || "Failed to change enabled state"),
      }
    );
  };

  return (
    <div className="rounded border border-neutral-700 bg-neutral-950/40 p-3">
      {error && <p className="mb-2 text-xs text-red-400">{error}</p>}

      <FieldRow label="Name" source={isOverridden(loc, "name") ? "override" : "config"} pendingRestart>
        <TextField value={draft.name} onChange={(v) => update("name", v)} />
      </FieldRow>

      <FieldRow label="Watch for changes" source={isOverridden(loc, "watch") ? "override" : "config"} pendingRestart>
        <ToggleField checked={draft.watch} onChange={(v) => update("watch", v)} />
      </FieldRow>

      <FieldRow label="Periodic sweep" source={isOverridden(loc, "sweep") ? "override" : "config"} pendingRestart>
        <ToggleField checked={draft.sweep} onChange={(v) => update("sweep", v)} />
      </FieldRow>

      <FieldRow
        label="Sweep interval (seconds)"
        source={isOverridden(loc, "sweepIntervalSecs") ? "override" : "config"}
        pendingRestart
      >
        <NumberField value={draft.sweepIntervalSecs} onChange={(v) => update("sweepIntervalSecs", v)} />
      </FieldRow>

      {loc.prunable && (
        <FieldRow
          label="Cache TTL (hours)"
          source={isOverridden(loc, "cacheTtlHours") ? "override" : "config"}
          pendingRestart
        >
          <NumberField value={draft.cacheTtlHours} onChange={(v) => update("cacheTtlHours", v)} />
        </FieldRow>
      )}

      <div className="flex flex-wrap items-center justify-between gap-2 pt-2">
        <div className="flex flex-wrap gap-2">
          <button
            type="button"
            onClick={handleSave}
            disabled={!canSave || putLocation.isPending}
            className="rounded bg-indigo-600 px-2 py-1 text-xs font-medium text-white hover:bg-indigo-500 disabled:opacity-50"
          >
            {putLocation.isPending ? "Saving…" : "Save"}
          </button>
          <button
            type="button"
            onClick={onClose}
            className="rounded border border-neutral-700 px-2 py-1 text-xs text-neutral-300 hover:border-neutral-500"
          >
            Close
          </button>
          {loc.overriddenFields.length > 0 && (
            <button
              type="button"
              onClick={handleResetToConfig}
              disabled={putLocation.isPending}
              className="rounded border border-neutral-700 px-2 py-1 text-xs text-neutral-300 hover:border-neutral-500 disabled:opacity-50"
            >
              Reset to config
            </button>
          )}
        </div>
        {/* Enable/disable is deliberately its own immediate action, not part
            of the batched Save above -- the server rejects disabling the
            last enabled location with a 422. Disabled while dirty: it fires
            its own PUT with only {enabled}, which would silently drop any
            unsaved edits to the other five fields sitting in the form. */}
        <div className="flex flex-col items-end gap-1">
          <button
            type="button"
            onClick={handleToggleEnabled}
            disabled={putLocation.isPending || dirty}
            title={dirty ? "Save or discard your other edits first" : undefined}
            className={`rounded border px-2 py-1 text-xs disabled:opacity-50 ${
              loc.disabled
                ? "border-emerald-800 text-emerald-300 hover:border-emerald-600"
                : "border-red-900 text-red-300 hover:border-red-700"
            }`}
          >
            {loc.disabled ? "Enable location" : "Disable location"}
          </button>
          <p className="max-w-[16rem] text-right text-[10px] text-neutral-500">
            {dirty
              ? "Save or discard your other edits above first."
              : "Takes effect on next restart: no watch/sweep/manual-scan-target. Reads and Tier-3 prune authorization are unaffected either way."}
          </p>
        </div>
      </div>
    </div>
  );
}

function LocationGaugeCard({ loc }: { loc: StorageLocationHealth }) {
  const percentUsed = loc.totalBytes > 0 ? Math.min(100, Math.round((loc.usedBytes / loc.totalBytes) * 100)) : 0;
  const [editing, setEditing] = useState(false);

  return (
    <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-5 shadow-sm space-y-4">
      <div className="flex items-start justify-between">
        <div>
          <div className="flex items-center space-x-2">
            <h3 className="text-base font-semibold text-neutral-100">{loc.name}</h3>
            <span className="rounded bg-neutral-800 px-2 py-0.5 text-xs font-mono text-neutral-300">{loc.tier}</span>
          </div>
          <p className="mt-1 text-xs font-mono text-neutral-400 break-all">{loc.rootPath}</p>
        </div>
        <div className="flex flex-wrap gap-1.5 justify-end">
          <button
            type="button"
            onClick={() => setEditing((e) => !e)}
            className="rounded bg-neutral-800 px-2 py-0.5 text-xs text-neutral-300 border border-neutral-700 hover:bg-neutral-700"
          >
            {editing ? "Close" : "Edit"}
          </button>
          {/* Disabled (an override the operator set) is deliberately a
              distinct badge from Inactive (the mount failed to resolve at
              startup, self-healing) -- conflating them would make "I turned
              this off" indistinguishable from "the NAS fell off the
              network". */}
          {loc.disabled && (
            <span className="rounded bg-red-950 px-2 py-0.5 text-xs font-medium text-red-300 border border-red-800">
              DISABLED
            </span>
          )}
          {/* watch/sweep/sweepIntervalSecs/cacheTtlHours/enabled overrides
              are all restart-required (main.go's seedStorageLocations only
              runs at boot), so the merged values above the fold can
              legitimately disagree with what's actually running right now
              -- this chip is the only always-visible signal of that, since
              the per-field "Applies on restart" badges are inside the
              collapsed Edit form. */}
          {loc.overriddenFields.length > 0 && (
            <span className="rounded bg-amber-950 px-2 py-0.5 text-xs font-medium text-amber-300 border border-amber-800">
              PENDING RESTART
            </span>
          )}
          {!loc.isActive && (
            <span className="rounded bg-neutral-800 px-2 py-0.5 text-xs font-medium text-neutral-400 border border-neutral-700">
              INACTIVE
            </span>
          )}
          {loc.isDegraded ? (
            <span className="rounded bg-red-950 px-2 py-0.5 text-xs font-medium text-red-300 border border-red-800">
              DEGRADED
            </span>
          ) : (
            <span className="rounded bg-emerald-950 px-2 py-0.5 text-xs font-medium text-emerald-300 border border-emerald-800">
              HEALTHY
            </span>
          )}
          {loc.readOnly && (
            <span className="rounded bg-amber-950 px-2 py-0.5 text-xs text-amber-300 border border-amber-800">
              READ-ONLY
            </span>
          )}
          {loc.prunable && (
            <span className="rounded bg-blue-950 px-2 py-0.5 text-xs text-blue-300 border border-blue-800">
              PRUNABLE
            </span>
          )}
        </div>
      </div>

      {editing && <StorageLocationEditForm loc={loc} onClose={() => setEditing(false)} />}

      {loc.isDegraded ? (
        <div className="rounded bg-red-950/50 p-3 text-xs text-red-300 border border-red-900/50">
          <p className="font-semibold">Location Access Failed</p>
          <p className="mt-0.5 font-mono text-neutral-400">{loc.degradedMessage || "Failed to query filesystem stats via statfs"}</p>
        </div>
      ) : (
        <>
          <div>
            <div className="flex justify-between text-xs text-neutral-400 mb-1.5">
              <span>Capacity Used: {percentUsed}%</span>
              <span>
                {formatBytes(loc.usedBytes)} / {formatBytes(loc.totalBytes)}
              </span>
            </div>
            <div className="h-2.5 w-full rounded-full bg-neutral-800 overflow-hidden">
              <div
                className={`h-full transition-all duration-300 ${
                  percentUsed > 90 ? "bg-red-500" : percentUsed > 75 ? "bg-amber-500" : "bg-indigo-500"
                }`}
                style={{ width: `${percentUsed}%` }}
              />
            </div>
          </div>

          <div className="grid grid-cols-3 gap-2 pt-2 border-t border-neutral-800 text-center text-xs">
            <div>
              <span className="block text-neutral-400">Indexed Nodes</span>
              <span className="font-semibold text-neutral-200">{loc.nodeCount.toLocaleString()}</span>
            </div>
            <div>
              <span className="block text-neutral-400">Used Space</span>
              <span className="font-semibold text-neutral-200">{formatBytes(loc.usedBytes)}</span>
            </div>
            <div>
              <span className="block text-neutral-400">Free Space</span>
              <span className="font-semibold text-neutral-200">{formatBytes(loc.freeBytes)}</span>
            </div>
          </div>
        </>
      )}

      {loc.prunable && (
        <div className="pt-2 border-t border-neutral-800">
          <PruneControl locationId={loc.id} />
        </div>
      )}
    </div>
  );
}

export default function StorageHealthPage() {
  const { data: health, isLoading, isError } = useStorageHealth();

  if (isLoading) {
    return <div className="p-6 text-neutral-400">Loading storage health metrics…</div>;
  }

  if (isError || !health) {
    return <div className="p-6 text-red-400">Failed to load storage health metrics.</div>;
  }

  const { locations, queues } = health;

  return (
    <div className="p-6 space-y-8 max-w-6xl">
      <div>
        <h1 className="text-2xl font-bold text-neutral-100">Storage Health</h1>
        <p className="mt-1 text-sm text-neutral-400">
          Real-time storage capacity across Tiers 1–3 and background processing queue depths.
        </p>
      </div>

      <section className="space-y-4">
        <h2 className="text-lg font-semibold text-neutral-200">Storage Tier Capacity</h2>
        {locations.length === 0 ? (
          <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-6 text-center text-neutral-400">
            No storage locations registered.
          </div>
        ) : (
          <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
            {locations.map((loc) => (
              <LocationGaugeCard key={loc.id} loc={loc} />
            ))}
          </div>
        )}
      </section>

      <section className="space-y-4">
        <h2 className="text-lg font-semibold text-neutral-200">Queue & Worker Pool Status</h2>
        <div className="grid grid-cols-1 sm:grid-cols-2 lg:grid-cols-4 gap-4">
          <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
            <span className="text-xs text-neutral-400 font-medium uppercase tracking-wider block">
              In-Flight Worker Jobs
            </span>
            <span className="mt-2 text-2xl font-bold text-indigo-400 block">
              {queues.workerPoolInFlight}
            </span>
            <span className="text-xs text-neutral-400 mt-1 block">Active background hashing</span>
          </div>

          <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
            <span className="text-xs text-neutral-400 font-medium uppercase tracking-wider block">
              Queued Worker Jobs
            </span>
            <span className="mt-2 text-2xl font-bold text-amber-400 block">
              {queues.workerPoolQueued} / {queues.workerPoolCapacity || "—"}
            </span>
            <span className="text-xs text-neutral-400 mt-1 block">Pending hash queue depth</span>
          </div>

          <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
            <span className="text-xs text-neutral-400 font-medium uppercase tracking-wider block">
              Worker Threads
            </span>
            <span className="mt-2 text-2xl font-bold text-emerald-400 block">
              {queues.workerCount || "—"}
            </span>
            <span className="text-xs text-neutral-400 mt-1 block">Configured worker pool size</span>
          </div>

          <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
            <span className="text-xs text-neutral-400 font-medium uppercase tracking-wider block">
              Running Scan Jobs
            </span>
            <span className="mt-2 text-2xl font-bold text-blue-400 block">
              {queues.runningScanJobs}
            </span>
            <span className="text-xs text-neutral-400 mt-1 block">Active pipeline scans</span>
          </div>
        </div>
      </section>
    </div>
  );
}
