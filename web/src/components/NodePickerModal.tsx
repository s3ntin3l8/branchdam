import { useRef, useState } from "react";
import Dialog from "./Dialog";
import { useAssets } from "../hooks/queries";
import { useDebouncedValue } from "../hooks/useDebouncedValue";
import Thumbnail from "./Thumbnail";
import type { Asset } from "../api/types";

export interface NodePickerModalProps {
  isOpen: boolean;
  onClose: () => void;
  onSelect: (asset: Asset) => void;
  selectedAssetId?: number | string;
  title?: string;
}

// One page of candidates. The search runs server-side (?q= on
// /api/v1/assets, a filename substring), so the picker reaches every asset
// in the library -- not just whatever the first page happened to hold.
export const NODE_PICKER_PAGE_SIZE = 25;

// NodePickerModal mounts its dialog only while open, so each opening starts
// with an empty search on the first page and the Dialog primitive handles
// focus (into the search box on open, back to the opener on close), the Tab
// trap and Escape.
export default function NodePickerModal(props: NodePickerModalProps) {
  if (!props.isOpen) return null;
  return <NodePickerDialog {...props} />;
}

function NodePickerDialog({
  onClose,
  onSelect,
  selectedAssetId,
  title = "Select Asset Node",
}: NodePickerModalProps) {
  const [searchQuery, setSearchQuery] = useState("");
  const [page, setPage] = useState(0);
  const searchInputRef = useRef<HTMLInputElement>(null);

  const debouncedQuery = useDebouncedValue(searchQuery.trim(), 300);
  // A new query starts from the first page ("adjust state when a prop
  // changes" -- no effect round-trip).
  const [prevQuery, setPrevQuery] = useState(debouncedQuery);
  if (debouncedQuery !== prevQuery) {
    setPrevQuery(debouncedQuery);
    setPage(0);
  }
  const { data, isLoading, isError, error, isFetching } = useAssets(
    {
      limit: NODE_PICKER_PAGE_SIZE,
      offset: page * NODE_PICKER_PAGE_SIZE,
      q: debouncedQuery || undefined,
    },
    { keepPreviousData: true }
  );
  const assets = data?.assets ?? [];
  const total = data?.total ?? 0;
  const totalPages = Math.max(1, Math.ceil(total / NODE_PICKER_PAGE_SIZE));

  return (
    <Dialog
      title={title}
      onClose={onClose}
      initialFocusRef={searchInputRef}
      overlayClassName="z-60 bg-black/70 backdrop-blur-xs"
      panelClassName="flex max-h-[85vh] w-full max-w-2xl flex-col rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow-2xl"
      headerClassName="mb-4 flex items-center justify-between border-b border-neutral-800 pb-3"
      titleClassName="text-lg font-semibold text-neutral-100"
      headerAction={
        <button
          type="button"
          onClick={onClose}
          aria-label="Close dialog"
          className="rounded p-1 text-neutral-400 hover:bg-neutral-800 hover:text-neutral-100"
        >
          ✕
        </button>
      }
    >
        {/* Search Bar */}
        <div className="mb-4">
          <div className="relative">
            <input
              ref={searchInputRef}
              type="text"
              value={searchQuery}
              onChange={(e) => setSearchQuery(e.target.value)}
              placeholder="Search by filename…"
              aria-label="Search assets"
              className="w-full rounded border border-neutral-800 bg-neutral-950 px-3 py-2 pr-8 text-sm text-neutral-100 focus:border-indigo-500 focus:outline-none focus:ring-1 focus:ring-indigo-500"
            />
            {searchQuery && (
              <button
                type="button"
                onClick={() => {
                  setSearchQuery("");
                  searchInputRef.current?.focus();
                }}
                aria-label="Clear search"
                className="absolute right-2.5 top-1/2 -translate-y-1/2 text-xs text-neutral-400 hover:text-neutral-100"
              >
                ✕
              </button>
            )}
          </div>
        </div>

        {/* Content list */}
        <div className="flex-1 overflow-y-auto space-y-2 pr-1 min-h-[220px]">
          {isLoading && (
            <div className="flex h-48 items-center justify-center text-sm text-neutral-400">
              Loading assets…
            </div>
          )}

          {isError && (
            <div className="flex h-48 items-center justify-center text-sm text-red-400">
              Failed to load assets: {String(error)}
            </div>
          )}

          {!isLoading && !isError && assets.length === 0 && !debouncedQuery && (
            <div className="flex h-48 items-center justify-center text-sm text-neutral-500">
              No assets found in library.
            </div>
          )}

          {!isLoading && !isError && assets.length === 0 && debouncedQuery && (
            <div className="flex h-48 items-center justify-center text-sm text-neutral-500">
              No assets matching &ldquo;{debouncedQuery}&rdquo;
            </div>
          )}

          {!isLoading &&
            !isError &&
            assets.map((asset) => {
              const isSelected =
                selectedAssetId !== undefined &&
                (String(asset.id) === String(selectedAssetId) || asset.nodeUuid === String(selectedAssetId));

              return (
                <button
                  type="button"
                  key={asset.id}
                  onClick={() => {
                    onSelect(asset);
                    onClose();
                  }}
                  className={`group flex w-full items-center gap-3 rounded-lg border p-3 text-left transition-colors ${
                    isSelected
                      ? "border-indigo-500 bg-indigo-950/40 text-neutral-100"
                      : "border-neutral-800 bg-neutral-950/60 text-neutral-300 hover:border-neutral-700 hover:bg-neutral-800/60"
                  }`}
                >
                  <Thumbnail
                    assetId={asset.id}
                    thumbState={asset.thumbState}
                    alt={asset.fileName}
                    className="h-12 w-12 shrink-0 rounded object-cover border border-neutral-800"
                  />
                  <div className="min-w-0 flex-1">
                    <div className="flex items-center justify-between gap-2">
                      <span
                        className="truncate text-sm font-medium text-neutral-100 group-hover:text-neutral-50"
                        title={asset.fileName}
                      >
                        {asset.fileName}
                      </span>
                      <span className="shrink-0 rounded bg-neutral-800 px-2 py-0.5 font-mono text-xs text-neutral-400">
                        Node #{asset.id}
                      </span>
                    </div>
                    <div className="truncate text-xs text-neutral-500" title={asset.filePath}>
                      {asset.filePath}
                    </div>
                    <div className="mt-1 flex flex-wrap items-center gap-2 text-xs text-neutral-400">
                      {asset.cameraModel && (
                        <span className="rounded bg-neutral-800/80 px-1.5 py-0.5 text-[11px] text-neutral-300">
                          {asset.cameraModel}
                        </span>
                      )}
                      <span className="font-mono text-[11px] text-neutral-500" title={asset.nodeUuid}>
                        UUID: {asset.nodeUuid}
                      </span>
                    </div>
                  </div>
                  <div className="shrink-0">
                    <span
                      className={`rounded border px-2.5 py-1 text-xs font-medium transition-colors ${
                        isSelected
                          ? "border-indigo-500 bg-indigo-600 text-white"
                          : "border-indigo-600/50 bg-indigo-600/20 text-indigo-300 group-hover:bg-indigo-600 group-hover:text-white"
                      }`}
                    >
                      {isSelected ? "Selected" : "Select"}
                    </span>
                  </div>
                </button>
              );
            })}
        </div>

        {/* Footer */}
        <div className="mt-4 flex items-center justify-between border-t border-neutral-800 pt-3 text-xs text-neutral-400">
          <div aria-live="polite">
            {!isLoading && !isError && total > 0 && (
              <span>
                Showing {page * NODE_PICKER_PAGE_SIZE + 1}–{page * NODE_PICKER_PAGE_SIZE + assets.length} of {total}{" "}
                {total === 1 ? "asset" : "assets"}
                {isFetching ? " (updating…)" : ""}
              </span>
            )}
          </div>
          <div className="flex items-center gap-2">
            <button
              type="button"
              aria-label="Previous page"
              disabled={page === 0 || isLoading}
              onClick={() => setPage((p) => Math.max(0, p - 1))}
              className="rounded border border-neutral-700 bg-neutral-800 px-3 py-2 text-neutral-300 hover:bg-neutral-700 disabled:opacity-40"
            >
              Previous
            </button>
            <span className="font-mono">
              {page + 1} / {totalPages}
            </span>
            <button
              type="button"
              aria-label="Next page"
              disabled={page + 1 >= totalPages || isLoading}
              onClick={() => setPage((p) => p + 1)}
              className="rounded border border-neutral-700 bg-neutral-800 px-3 py-2 text-neutral-300 hover:bg-neutral-700 disabled:opacity-40"
            >
              Next
            </button>
            <button
              type="button"
              onClick={onClose}
              className="rounded bg-neutral-800 px-4 py-2 text-sm text-neutral-300 hover:bg-neutral-700"
            >
              Cancel
            </button>
          </div>
        </div>
    </Dialog>
  );
}
