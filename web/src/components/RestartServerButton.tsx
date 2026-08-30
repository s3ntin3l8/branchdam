import { useState } from "react";
import { useRestartServer } from "../hooks/queries";

// Shared by SettingsPage's "Restart server" card and StorageHealthPage's
// conditional "Restart to apply" button -- same POST /api/v1/restart
// mutation (useRestartServer), same two-step confirm flow. The repo has no
// dialog/modal primitive and browser modals (window.confirm) are avoided
// throughout (see claude-in-chrome's own guidance and the rest of this
// codebase), so "confirm" here is an inline second button state rather than
// a native dialog.
function useRestartConfirm() {
  const restart = useRestartServer();
  const [confirming, setConfirming] = useState(false);
  const [justRestarted, setJustRestarted] = useState(false);

  const handleConfirm = () => {
    setConfirming(false);
    restart.mutate(undefined, {
      onSuccess: () => {
        setJustRestarted(true);
        // The server is unreachable for a few seconds while it re-execs
        // (see internal/httpapi/restart.go's restartGraceDelay plus the
        // process replacing itself) -- this is purely a local UI note, not
        // a poll: useStorageHealth's existing 10s refetchInterval and
        // default query retry are what actually detect the server coming
        // back, per useRestartServer's own doc comment.
        setTimeout(() => setJustRestarted(false), 15_000);
      },
    });
  };

  return { restart, confirming, setConfirming, justRestarted, handleConfirm };
}

const confirmButtonClass =
  "rounded bg-amber-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-amber-500 disabled:opacity-50";
const cancelButtonClass =
  "rounded border border-neutral-700 px-3 py-1.5 text-xs text-neutral-300 hover:border-neutral-500 disabled:opacity-50";

// RestartServerCard is SettingsPage's always-visible restart affordance --
// unlike the pendingRestart banner it sits next to, it is not gated on
// Store.PendingRestart() being non-empty, since that diff never covers
// storage-location overrides (see StorageHealthPage's own restart button
// and CLAUDE.md's storageLocation.<rootPath>.* invariant).
export function RestartServerCard({ className = "" }: { className?: string }) {
  const { restart, confirming, setConfirming, justRestarted, handleConfirm } = useRestartConfirm();

  return (
    <div className={`rounded-lg border border-neutral-800 bg-neutral-900/50 p-4 ${className}`}>
      <h2 className="mb-2 text-sm font-semibold uppercase tracking-wider text-neutral-400">Restart Server</h2>
      <p className="mb-3 text-xs text-neutral-500">
        Applies every pending "restart required" setting and re-reads <span className="font-mono">config.yaml</span>.
        Does <span className="font-semibold text-neutral-400">not</span> pick up <span className="font-mono">.env</span>{" "}
        changes or a new image -- use <span className="font-mono">docker compose up -d</span> for those. Any running
        scan is stopped and resumes on its next pass.
      </p>

      {justRestarted && (
        <p className="mb-3 text-sm text-amber-300">Restarting… this page will reconnect automatically.</p>
      )}
      {restart.isError && (
        <p className="mb-3 text-sm text-red-400">Failed to restart: {String(restart.error)}</p>
      )}

      {confirming ? (
        <div className="flex items-center gap-2">
          <span className="text-sm text-neutral-300">Restart now?</span>
          <button type="button" onClick={handleConfirm} disabled={restart.isPending} className={confirmButtonClass}>
            Restart
          </button>
          <button
            type="button"
            onClick={() => setConfirming(false)}
            disabled={restart.isPending}
            className={cancelButtonClass}
          >
            Cancel
          </button>
        </div>
      ) : (
        <button
          type="button"
          onClick={() => setConfirming(true)}
          disabled={restart.isPending}
          className={confirmButtonClass}
        >
          Restart server
        </button>
      )}
    </div>
  );
}

// RestartToApplyButton is StorageHealthPage's compact equivalent, rendered
// only by the caller when at least one location has a pending override --
// see StorageHealthPage.tsx for that gating.
export function RestartToApplyButton() {
  const { restart, confirming, setConfirming, justRestarted, handleConfirm } = useRestartConfirm();

  if (justRestarted) {
    return <span className="text-xs text-amber-300">Restarting…</span>;
  }

  if (confirming) {
    return (
      <div className="flex items-center gap-2">
        <span className="text-xs text-neutral-300">Restart now?</span>
        <button type="button" onClick={handleConfirm} disabled={restart.isPending} className={confirmButtonClass}>
          Restart
        </button>
        <button
          type="button"
          onClick={() => setConfirming(false)}
          disabled={restart.isPending}
          className={cancelButtonClass}
        >
          Cancel
        </button>
      </div>
    );
  }

  return (
    <button
      type="button"
      onClick={() => setConfirming(true)}
      disabled={restart.isPending}
      title="Applies pending storage-location changes. Does not pick up .env changes or a new image."
      className={confirmButtonClass}
    >
      Restart to apply
    </button>
  );
}
