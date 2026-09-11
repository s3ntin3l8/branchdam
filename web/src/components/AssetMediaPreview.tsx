import { useEffect, useState } from "react";
import { api } from "../api/client";
import Thumbnail from "./Thumbnail";
import type { Asset, LineageResponse } from "../api/types";

interface AssetMediaPreviewProps {
  asset: Asset;
  lineage?: LineageResponse;
}

const VIDEO_EXTENSIONS = new Set(["mp4", "webm", "mov", "m4v", "ogv", "mkv"]);
const IMAGE_EXTENSIONS = new Set(["jpg", "jpeg", "png", "webp", "gif", "svg", "avif"]);
const AUDIO_EXTENSIONS = new Set(["mp3", "wav", "flac", "aac", "ogg", "m4a"]);

function getVideoMimeType(ext: string): string {
  switch (ext.toLowerCase()) {
    case "mp4":
    case "m4v":
      return "video/mp4";
    case "webm":
      return "video/webm";
    case "mov":
      return "video/quicktime";
    case "ogv":
      return "video/ogg";
    case "mkv":
      return "video/x-matroska";
    default:
      return "video/mp4";
  }
}

export default function AssetMediaPreview({ asset, lineage }: AssetMediaPreviewProps) {
  const [selectedAssetId, setSelectedAssetId] = useState<number | null>(null);
  const [isLightboxOpen, setIsLightboxOpen] = useState(false);

  // Close lightbox on Escape key
  useEffect(() => {
    if (!isLightboxOpen) return;
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") {
        setIsLightboxOpen(false);
      }
    };
    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [isLightboxOpen]);

  // Find linked proxy / original from lineage
  let originalNode: Asset | undefined;
  let proxyNode: Asset | undefined;

  if (lineage) {
    for (const edge of lineage.edges) {
      if (edge.relationshipType === "PROXY_OF") {
        if (edge.sourceNodeId === asset.id) {
          proxyNode = lineage.nodes.find((n) => n.id === edge.targetNodeId);
          originalNode = asset;
        } else if (edge.targetNodeId === asset.id) {
          originalNode = lineage.nodes.find((n) => n.id === edge.sourceNodeId);
          proxyNode = asset;
        }
      }
    }
  }

  const activeAsset =
    (selectedAssetId === originalNode?.id
      ? originalNode
      : selectedAssetId === proxyNode?.id
        ? proxyNode
        : null) ?? asset;

  const ext = activeAsset.fileExt.toLowerCase();
  const isVideo = VIDEO_EXTENSIONS.has(ext);
  const isImage = IMAGE_EXTENSIONS.has(ext);
  const isAudio = AUDIO_EXTENSIONS.has(ext);

  const hasProxyPair = Boolean(originalNode && proxyNode && originalNode.id !== proxyNode.id);

  return (
    <div className="space-y-3">
      {/* Proxy / Original Switcher Pill */}
      {hasProxyPair && originalNode && proxyNode && (
        <div className="flex items-center gap-2">
          <span className="text-xs font-medium text-neutral-400">Stream Source:</span>
          <div className="inline-flex rounded-md border border-neutral-800 bg-neutral-900 p-0.5">
            <button
              type="button"
              onClick={() => setSelectedAssetId(originalNode.id)}
              className={`rounded px-2.5 py-1 text-xs font-medium transition ${
                activeAsset.id === originalNode.id
                  ? "bg-neutral-700 text-neutral-100 shadow"
                  : "text-neutral-400 hover:text-neutral-200"
              }`}
            >
              Original ({originalNode.fileExt.toUpperCase()})
            </button>
            <button
              type="button"
              onClick={() => setSelectedAssetId(proxyNode.id)}
              className={`rounded px-2.5 py-1 text-xs font-medium transition ${
                activeAsset.id === proxyNode.id
                  ? "bg-neutral-700 text-neutral-100 shadow"
                  : "text-neutral-400 hover:text-neutral-200"
              }`}
            >
              Proxy ({proxyNode.fileExt.toUpperCase()})
            </button>
          </div>
          {activeAsset.id !== asset.id && (
            <span className="text-xs italic text-amber-400/90">
              (Viewing linked {activeAsset.id === proxyNode.id ? "proxy" : "original"}: {activeAsset.fileName})
            </span>
          )}
        </div>
      )}

      {/* Main Media Preview Area */}
      {isVideo ? (
        <div className="relative flex items-center justify-center overflow-hidden rounded-lg border border-neutral-800 bg-black">
          <video
            key={activeAsset.id}
            controls
            preload="metadata"
            poster={api.thumbnailUrl(activeAsset.id)}
            className="max-h-[500px] w-full object-contain"
            data-testid="asset-video-player"
          >
            <source src={api.streamUrl(activeAsset.id)} type={getVideoMimeType(ext)} />
            Your browser does not support HTML5 video playback.
          </video>
        </div>
      ) : isImage ? (
        <div className="group relative flex max-h-[500px] min-h-[240px] items-center justify-center overflow-hidden rounded-lg border border-neutral-800 bg-neutral-900 p-2">
          <img
            src={api.streamUrl(activeAsset.id)}
            alt={activeAsset.fileName}
            className="max-h-[480px] max-w-full cursor-pointer rounded object-contain transition hover:opacity-95"
            onClick={() => setIsLightboxOpen(true)}
            data-testid="asset-image-preview"
          />
          <button
            type="button"
            onClick={() => setIsLightboxOpen(true)}
            className="absolute bottom-4 right-4 rounded-md border border-neutral-700 bg-neutral-900/80 px-3 py-1.5 text-xs text-neutral-200 opacity-0 shadow backdrop-blur transition hover:bg-neutral-800 group-hover:opacity-100"
          >
            Enlarge
          </button>
        </div>
      ) : isAudio ? (
        <div className="flex flex-col items-center justify-center gap-4 rounded-lg border border-neutral-800 bg-neutral-900 p-6">
          <div className="text-sm font-medium text-neutral-300">{activeAsset.fileName}</div>
          <audio
            key={activeAsset.id}
            controls
            className="w-full max-w-md"
            data-testid="asset-audio-player"
            src={api.streamUrl(activeAsset.id)}
          >
            Your browser does not support audio playback.
          </audio>
        </div>
      ) : (
        /* RAW or other format preview fallback */
        <div className="rounded-lg border border-neutral-800 bg-neutral-900 p-4">
          <div className="flex min-h-[220px] items-center justify-center">
            <Thumbnail
              assetId={activeAsset.id}
              thumbState={activeAsset.thumbState}
              alt={activeAsset.fileName}
              className="max-h-[400px] max-w-full rounded object-contain"
            />
          </div>
          <div className="mt-3 flex items-center justify-between border-t border-neutral-800/80 pt-2 text-xs text-neutral-400">
            <span className="flex items-center gap-1.5">
              <span className="inline-block h-2 w-2 rounded-full bg-amber-500/80" />
              Preview (Generated Thumbnail)
            </span>
            <span className="font-mono uppercase text-neutral-500">.{ext} format</span>
          </div>
        </div>
      )}

      {/* Lightbox Modal for Full-Resolution Image Inspection */}
      {isLightboxOpen && isImage && (
        <div
          role="dialog"
          aria-modal="true"
          className="fixed inset-0 z-50 flex items-center justify-center bg-black/90 p-4"
          onClick={() => setIsLightboxOpen(false)}
        >
          <div
            className="relative flex max-h-[95vh] max-w-[95vw] flex-col items-center"
            onClick={(e) => e.stopPropagation()}
          >
            <div className="absolute -top-10 right-0 flex items-center gap-3">
              <span className="text-xs text-neutral-400">{activeAsset.fileName}</span>
              <button
                type="button"
                onClick={() => setIsLightboxOpen(false)}
                className="rounded bg-neutral-800 px-2 py-1 text-xs text-neutral-200 hover:bg-neutral-700"
                data-testid="lightbox-close-button"
              >
                Close (ESC)
              </button>
            </div>
            <img
              src={api.streamUrl(activeAsset.id)}
              alt={activeAsset.fileName}
              className="max-h-[90vh] max-w-[90vw] rounded object-contain shadow-2xl"
            />
          </div>
        </div>
      )}
    </div>
  );
}
