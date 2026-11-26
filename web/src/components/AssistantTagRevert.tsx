import { useState } from "react";
import ConfirmDialog from "./ConfirmDialog";
import { useMe, useRevertAssistantTags } from "../hooks/queries";

export default function AssistantTagRevert() {
  const { data: me } = useMe();
  const revert = useRevertAssistantTags();
  const [open, setOpen] = useState(false);
  const [done, setDone] = useState<string | null>(null);
  if (!me?.isAdmin) return null;

  return (
    <div className="mb-6 rounded-lg border border-neutral-800 bg-neutral-900/50 p-4">
      <h2 className="mb-1 text-sm font-semibold text-neutral-200">Assistant tags</h2>
      <p className="mb-3 text-xs text-neutral-400">
        Remove every tag and description an assistant wrote. Tags a person added stay.
      </p>
      {done && <p className="mb-2 text-xs text-neutral-300">{done}</p>}
      <button
        type="button"
        onClick={() => {
          setDone(null);
          setOpen(true);
        }}
        className="rounded border border-neutral-700 bg-neutral-800 px-2 py-1 text-xs text-neutral-200 hover:bg-neutral-700"
      >
        Revert assistant tags
      </button>
      {open && (
        <ConfirmDialog
          titleId="revert-assistant-tags-title"
          title="Revert assistant tags"
          body="This deletes every assistant tag and description. It cannot be undone."
          confirmLabel="Revert"
          pendingLabel="Reverting…"
          isPending={revert.isPending}
          error={revert.error}
          onCancel={() => {
            if (!revert.isPending) setOpen(false);
          }}
          onConfirm={() => {
            revert.mutate(undefined, {
              onSuccess: (result) => {
                setOpen(false);
                setDone(
                  `Removed ${result.tagsRemoved} tag${result.tagsRemoved === 1 ? "" : "s"} and ${result.annotationsRemoved} description${result.annotationsRemoved === 1 ? "" : "s"}.`,
                );
              },
            });
          }}
        />
      )}
    </div>
  );
}
