import { useEffect } from "react";
import { Link } from "react-router";

export interface DedupNoticeProps {
  fileName?: string;
  nodeUuid?: string;
  assetId?: number;
  onDismiss: () => void;
  autoDismissMs?: number;
}

export default function DedupNotice({
  fileName,
  nodeUuid,
  assetId,
  onDismiss,
  autoDismissMs = 10000,
}: DedupNoticeProps) {
  useEffect(() => {
    if (autoDismissMs <= 0) return;
    const timer = setTimeout(() => {
      onDismiss();
    }, autoDismissMs);
    return () => clearTimeout(timer);
  }, [onDismiss, autoDismissMs]);

  const targetLink = assetId ? `/assets/${assetId}` : nodeUuid ? `/assets/${nodeUuid}` : "/assets";

  return (
    <div
      role="status"
      aria-live="polite"
      className="flex items-center justify-between gap-3 rounded-lg border border-amber-800/80 bg-amber-950/70 px-4 py-3 text-xs text-amber-200 shadow-lg shadow-black/40 backdrop-blur-sm"
    >
      <div className="flex items-center gap-2.5 min-w-0">
        <svg
          className="h-4 w-4 shrink-0 text-amber-400"
          fill="none"
          viewBox="0 0 24 24"
          stroke="currentColor"
          strokeWidth={2}
          aria-hidden="true"
        >
          <path
            strokeLinecap="round"
            strokeLinejoin="round"
            d="M13 16h-1v-4h-1m1-4h.01M21 12a9 9 0 11-18 0 9 9 0 0118 0z"
          />
        </svg>
        <span className="truncate">
          {fileName ? (
            <>
              <strong className="font-semibold text-amber-100">{fileName}</strong> is already in your library.
            </>
          ) : (
            "This file is already in your library."
          )}
        </span>
        <span className="text-amber-400/80 font-medium">—</span>
        <Link
          to={targetLink}
          className="shrink-0 font-medium text-sky-400 underline hover:text-sky-300 focus:outline-none focus:ring-1 focus:ring-sky-400 rounded"
        >
          View existing node →
        </Link>
      </div>
      <button
        type="button"
        onClick={onDismiss}
        className="shrink-0 p-1 text-amber-400 hover:text-amber-100 focus:outline-none"
        aria-label="Dismiss notice"
      >
        ✕
      </button>
    </div>
  );
}
