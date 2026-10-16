import type { ReactNode } from "react";
import Dialog from "./Dialog";

// ConfirmDialog is the shared Cancel/Confirm modal for destructive actions,
// built on the Dialog primitive (focus trap, Escape, labelled title). Focus
// lands on Cancel when it opens -- the first focusable element -- so a
// stray Enter never confirms. While the action is pending the dialog can't
// be dismissed (Escape, backdrop and Cancel are all disabled), so the
// result of an in-flight request is never lost.
export interface ConfirmDialogProps {
  titleId?: string;
  title: string;
  body: ReactNode;
  confirmLabel: string;
  pendingLabel: string;
  isPending: boolean;
  error?: unknown;
  errorLabel?: string;
  onConfirm: () => void;
  onCancel: () => void;
}

export default function ConfirmDialog({
  titleId,
  title,
  body,
  confirmLabel,
  pendingLabel,
  isPending,
  error,
  errorLabel = "Failed",
  onConfirm,
  onCancel,
}: ConfirmDialogProps) {
  return (
    <Dialog title={title} titleId={titleId} onClose={onCancel} dismissible={!isPending}>
      <div className="space-y-4">
        <div className="text-xs text-neutral-300 leading-relaxed">{body}</div>
        {error !== undefined && error !== null && (
          <p className="text-xs text-red-400">
            {errorLabel}: {String(error)}
          </p>
        )}
        <div className="flex justify-end gap-2 pt-2">
          <button
            type="button"
            onClick={onCancel}
            disabled={isPending}
            className="rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-xs text-neutral-300 hover:bg-neutral-700"
          >
            Cancel
          </button>
          <button
            type="button"
            onClick={onConfirm}
            disabled={isPending}
            className="rounded bg-red-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-red-500 disabled:opacity-50"
          >
            {isPending ? pendingLabel : confirmLabel}
          </button>
        </div>
      </div>
    </Dialog>
  );
}
