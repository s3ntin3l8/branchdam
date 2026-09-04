import { useState } from "react";
import { api } from "../api/client";
import type { ThumbState } from "../api/types";

interface ThumbnailProps {
  assetId: number;
  thumbState: ThumbState;
  alt: string;
  className?: string;
}

// Thumbnail renders a node's cached JPEG (GET /api/v1/assets/{id}/thumbnail,
// internal/httpapi/thumbnail.go), or the state-appropriate placeholder when
// there isn't one yet. The three call sites (AssetListPage, AssetDetailPage,
// AuditQueuePage) share this so the PENDING/UNSUPPORTED/FAILED handling
// can't drift between them.
//
// The <img> only mounts when thumbState === "READY", which is what makes a
// thumbnail appear without a page reload once the background worker
// finishes: useEventStream's nudge invalidates the asset/audit-queue
// queries, thumbState flips to READY on refetch, and React mounts a brand
// new <img> element (rather than reusing one whose src never changed) --
// see internal/thumbs.Worker's nudge-once-per-batch behavior on the Go
// side for why that transition reliably fires shortly after generation.
//
// When a thumbnail image fails to load (broken/corrupted preview), onError
// gracefully falls back to the placeholder rather than displaying a broken img.
export default function Thumbnail({ assetId, thumbState, alt, className }: ThumbnailProps) {
  const [hasError, setHasError] = useState(false);
  const [prevAssetId, setPrevAssetId] = useState(assetId);
  const [prevThumbState, setPrevThumbState] = useState(thumbState);
  const base = className ?? "h-12 w-12 rounded object-cover";

  if (prevAssetId !== assetId || prevThumbState !== thumbState) {
    setPrevAssetId(assetId);
    setPrevThumbState(thumbState);
    setHasError(false);
  }

  if (thumbState === "READY" && !hasError) {
    return (
      <img
        src={api.thumbnailUrl(assetId)}
        alt={alt}
        loading="lazy"
        className={base}
        onError={() => setHasError(true)}
      />
    );
  }

  if (thumbState === "PENDING" && !hasError) {
    return (
      <div
        className={`${base} animate-pulse bg-neutral-800`}
        role="status"
        aria-label="Thumbnail generating"
      />
    );
  }

  // UNSUPPORTED (no embedded preview, expected for some formats), FAILED
  // (retries exhausted), and broken images (onError triggered) all render
  // the same static placeholder -- neither is worth distinguishing to a user
  // scanning a list of files.
  return (
    <div
      className={`${base} flex items-center justify-center bg-neutral-800 text-neutral-600`}
      role="img"
      aria-label="No thumbnail available"
    >
      <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" className="h-1/2 w-1/2">
        <rect x="3" y="3" width="18" height="18" rx="2" />
        <circle cx="9" cy="9" r="2" />
        <path d="M21 15l-5-5L5 21" />
      </svg>
    </div>
  );
}
