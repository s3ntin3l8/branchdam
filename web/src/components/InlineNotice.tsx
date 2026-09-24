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
  success: "text-emerald-400 hover:text-emerald-300",
  error: "text-red-400 hover:text-red-300",
} as const;

// InlineNotice is a transient, non-blocking page-level banner for success
// or error feedback that previously would have required a window.alert.
// Pattern mirrors DedupNotice: ref-guarded auto-dismiss timer so parent
// re-renders with new callback references do not reset the countdown.
//
// Timer reset contract: the countdown starts on mount and is governed only
// by autoDismissMs (which is tone-derived, so an in-place tone change
// restarts or cancels it as appropriate). Swapping message alone never
// restarts the timer -- callers that re-show a notice (identical message
// or not) MUST change the React key (e.g. a monotonic notice id) so the
// component remounts with a fresh window.
//
// Errors use role="alert" so screen readers announce them assertively;
// success stays role="status" (polite). The roles carry the live-region
// semantics, so no explicit aria-live is set. Error tone is sticky by
// default (autoDismissMs 0) so a failure cannot vanish unread; success
// auto-dismisses after 8s. Scrolls itself into view on appear (and on
// message/tone change) so feedback above a long table is not missed.
export default function InlineNotice({
  tone,
  message,
  onDismiss,
  autoDismissMs = tone === "error" ? 0 : 8000,
}: InlineNoticeProps) {
  const rootRef = useRef<HTMLDivElement>(null);
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
  }, [autoDismissMs]);

  useEffect(() => {
    // jsdom leaves scrollIntoView undefined; optional-call keeps tests happy.
    rootRef.current?.scrollIntoView?.({ behavior: "smooth", block: "nearest" });
  }, [message, tone]);

  const isError = tone === "error";

  return (
    <div
      ref={rootRef}
      role={isError ? "alert" : "status"}
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
