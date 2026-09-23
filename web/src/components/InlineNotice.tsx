import { useEffect, useRef } from "react";

export interface InlineNoticeProps {
  tone: "success" | "error";
  message: string;
  onDismiss: () => void;
  autoDismissMs?: number;
}

const toneClasses = {
  success: "border-emerald-800/60 bg-emerald-950/40 text-emerald-300",
  error: "border-red-800/60 bg-red-950/40 text-red-300",
} as const;

const dismissClasses = {
  success: "text-emerald-400 hover:text-emerald-200",
  error: "text-red-400 hover:text-red-200",
} as const;

// InlineNotice is a transient, non-blocking page-level banner for success
// or error feedback that previously would have required a window.alert.
// Pattern mirrors DedupNotice: ref-guarded auto-dismiss timer so parent
// re-renders with new callback references do not reset the countdown, but a
// swapped-in message still gets a fresh full window. Errors use role="alert"
// (assertive) so screen readers announce failures; success stays polite.
export default function InlineNotice({
  tone,
  message,
  onDismiss,
  autoDismissMs = 8000,
}: InlineNoticeProps) {
  const onDismissRef = useRef(onDismiss);
  useEffect(() => {
    onDismissRef.current = onDismiss;
  }, [onDismiss]);

  useEffect(() => {
    if (autoDismissMs <= 0) return;
    const timer = setTimeout(() => {
      onDismissRef.current();
    }, autoDismissMs);
    return () => clearTimeout(timer);
  }, [autoDismissMs, message]);

  const isError = tone === "error";

  return (
    <div
      role={isError ? "alert" : "status"}
      aria-live={isError ? "assertive" : "polite"}
      className={`mb-4 flex items-start justify-between gap-3 rounded border p-4 text-sm ${toneClasses[tone]}`}
    >
      <span>{message}</span>
      <button
        type="button"
        onClick={onDismiss}
        className={`shrink-0 -m-1 rounded p-1 focus:outline-none focus:ring-1 focus:ring-current ${dismissClasses[tone]}`}
        aria-label="Dismiss notice"
      >
        ✕
      </button>
    </div>
  );
}
