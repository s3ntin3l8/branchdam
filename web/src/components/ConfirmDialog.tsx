import { useEffect, type ReactNode } from "react";

// ConfirmDialog is the shared confirmation modal, extracted from
// AssetDetailPage's original inline "Archive Asset" dialog (the only
// role="dialog" implementation in the codebase that actually gated a
// destructive action behind Cancel/Confirm). The repo has no dialog
// primitive library -- see the three other hand-rolled patterns in
// CompanionPairingsPage (QrModal), NodePickerModal, and
// RestartServerButton's inline two-step confirm -- so this exists to give
// future destructive actions one to reuse instead of hand-rolling a fourth.
//
// Intentionally NOT migrating those other call sites in this change; this
// component's contract mirrors the one usage it was extracted from.
export interface ConfirmDialogProps {
  titleId: string;
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
  useEffect(() => {
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape" && !isPending) {
        onCancel();
      }
    };
    window.addEventListener("keydown", onKeyDown);
    return () => window.removeEventListener("keydown", onKeyDown);
  }, [isPending, onCancel]);

  return (
    <div
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/70 p-4"
      role="dialog"
      aria-modal="true"
      aria-labelledby={titleId}
    >
      <div className="max-w-md w-full rounded-lg border border-neutral-800 bg-neutral-900 p-6 space-y-4">
        <h3 id={titleId} className="text-base font-semibold text-neutral-100">
          {title}
        </h3>
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
    </div>
  );
}
