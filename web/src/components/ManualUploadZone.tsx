import React, { useCallback, useRef, useState } from "react";
import { Link } from "react-router";
import { useStorageLocations, useUploadFile } from "../hooks/queries";
import type { StorageLocation, UploadProgressEvent, WebUploadResponse } from "../api/types";

export interface QueueItem {
  id: string;
  file: File;
  relativePath: string;
  status: "queued" | "uploading" | "complete" | "error";
  progress: number;
  error?: string;
  response?: WebUploadResponse;
  abortController?: AbortController;
}

function formatBytes(bytes: number): string {
  if (bytes === 0) return "0 B";
  const k = 1024;
  const sizes = ["B", "KB", "MB", "GB", "TB"];
  const i = Math.floor(Math.log(bytes) / Math.log(k));
  return `${(bytes / Math.pow(k, i)).toFixed(1)} ${sizes[i]}`;
}

export default function ManualUploadZone() {
  const { data: locationsData, isLoading: loadingLocations } = useStorageLocations();
  const uploadFile = useUploadFile();

  const locations = locationsData?.locations ?? [];
  const writableLocations = locations.filter((l) => !l.readOnly);

  const [selectedLocationId, setSelectedLocationId] = useState<number | "">("");
  const [applyNamingTemplate, setApplyNamingTemplate] = useState<boolean>(true);
  const [customSubdir, setCustomSubdir] = useState<string>("");
  const [queue, setQueue] = useState<QueueItem[]>([]);
  const [isDragging, setIsDragging] = useState<boolean>(false);
  const [isProcessingQueue, setIsProcessingQueue] = useState<boolean>(false);

  const fileInputRef = useRef<HTMLInputElement>(null);
  const folderInputRef = useRef<HTMLInputElement>(null);

  // Set default writable location once loaded
  React.useEffect(() => {
    if (selectedLocationId === "" && writableLocations.length > 0) {
      const defaultLoc = writableLocations.find((l) => l.tier === "TIER3_MASTER_ARCHIVE") || writableLocations[0];
      setSelectedLocationId(defaultLoc.id);
    }
  }, [writableLocations, selectedLocationId]);

  const addFilesToQueue = useCallback((files: { file: File; relativePath?: string }[]) => {
    const newItems: QueueItem[] = files.map(({ file, relativePath }) => ({
      id: `${file.name}-${file.size}-${file.lastModified}-${Math.random().toString(36).slice(2, 7)}`,
      file,
      relativePath: relativePath || file.name,
      status: "queued",
      progress: 0,
    }));
    setQueue((prev) => [...prev, ...newItems]);
  }, []);

  const handleFileSelect = (e: React.ChangeEvent<HTMLInputElement>) => {
    if (!e.target.files) return;
    const fileList = Array.from(e.target.files);
    addFilesToQueue(fileList.map((file) => ({ file })));
    e.target.value = "";
  };

  const handleFolderSelect = (e: React.ChangeEvent<HTMLInputElement>) => {
    if (!e.target.files) return;
    const fileList = Array.from(e.target.files);
    addFilesToQueue(
      fileList.map((file) => ({
        file,
        relativePath: (file as unknown as { webkitRelativePath?: string }).webkitRelativePath || file.name,
      }))
    );
    e.target.value = "";
  };

  const readAllEntries = async (dirReader: FileSystemDirectoryEntry["createReader"] extends () => infer R ? R : never): Promise<FileSystemEntry[]> => {
    const entries: FileSystemEntry[] = [];
    while (true) {
      const batch = await new Promise<FileSystemEntry[]>((resolve, reject) => {
        dirReader.readEntries(resolve, reject);
      });
      if (!batch || batch.length === 0) {
        break;
      }
      entries.push(...batch);
    }
    return entries;
  };

  const traverseFileTree = async (item: FileSystemEntry, path = ""): Promise<{ file: File; relativePath: string }[]> => {
    if (item.isFile) {
      return new Promise((resolve) => {
        (item as FileSystemFileEntry).file((file) => {
          resolve([{ file, relativePath: path ? `${path}/${file.name}` : file.name }]);
        });
      });
    } else if (item.isDirectory) {
      const dirReader = (item as FileSystemDirectoryEntry).createReader();
      try {
        const entries = await readAllEntries(dirReader);
        const results: { file: File; relativePath: string }[] = [];
        for (const entry of entries) {
          const nested = await traverseFileTree(entry, path ? `${path}/${item.name}` : item.name);
          results.push(...nested);
        }
        return results;
      } catch {
        return [];
      }
    }
    return [];
  };

  const handleDrop = async (e: React.DragEvent<HTMLDivElement>) => {
    e.preventDefault();
    setIsDragging(false);

    const items = e.dataTransfer.items;
    if (items && items.length > 0) {
      const filePromises: Promise<{ file: File; relativePath: string }[]>[] = [];
      for (let i = 0; i < items.length; i++) {
        const entry = items[i].webkitGetAsEntry?.();
        if (entry) {
          filePromises.push(traverseFileTree(entry));
        } else {
          const file = items[i].getAsFile();
          if (file) filePromises.push(Promise.resolve([{ file, relativePath: file.name }]));
        }
      }
      const fileArrays = await Promise.all(filePromises);
      addFilesToQueue(fileArrays.flat());
    } else if (e.dataTransfer.files) {
      addFilesToQueue(Array.from(e.dataTransfer.files).map((file) => ({ file })));
    }
  };

  const removeItem = (id: string) => {
    setQueue((prev) => {
      const item = prev.find((i) => i.id === id);
      if (item?.status === "uploading" && item.abortController) {
        item.abortController.abort();
      }
      return prev.filter((i) => i.id !== id);
    });
  };

  const clearCompleted = () => {
    setQueue((prev) => prev.filter((i) => i.status !== "complete"));
  };

  const startUploads = async () => {
    if (selectedLocationId === "" || isProcessingQueue) return;
    setIsProcessingQueue(true);

    const queuedItems = queue.filter((item) => item.status === "queued" || item.status === "error");

    for (const item of queuedItems) {
      const abortController = new AbortController();

      setQueue((prev) =>
        prev.map((i) =>
          i.id === item.id ? { ...i, status: "uploading", progress: 0, error: undefined, abortController } : i
        )
      );

      try {
        let finalRelativePath = item.relativePath;
        if (!applyNamingTemplate && customSubdir.trim() !== "") {
          finalRelativePath = `${customSubdir.trim()}/${item.relativePath}`;
        }

        const res = await uploadFile.mutateAsync({
          file: item.file,
          options: {
            storageLocationId: Number(selectedLocationId),
            applyNamingTemplate,
            relativePath: !applyNamingTemplate ? finalRelativePath : undefined,
          },
          onProgress: (p: UploadProgressEvent) => {
            setQueue((prev) =>
              prev.map((i) => (i.id === item.id ? { ...i, progress: p.percent } : i))
            );
          },
          signal: abortController.signal,
        });

        setQueue((prev) =>
          prev.map((i) =>
            i.id === item.id
              ? {
                  ...i,
                  status: "complete",
                  progress: 100,
                  response: res,
                  abortController: undefined,
                }
              : i
          )
        );
      } catch (err: unknown) {
        if ((err as Error)?.name === "AbortError") {
          setQueue((prev) =>
            prev.map((i) =>
              i.id === item.id ? { ...i, status: "queued", progress: 0, abortController: undefined } : i
            )
          );
        } else {
          setQueue((prev) =>
            prev.map((i) =>
              i.id === item.id
                ? {
                    ...i,
                    status: "error",
                    error: (err as Error)?.message || "Upload failed",
                    abortController: undefined,
                  }
                : i
            )
          );
        }
      }
    }

    setIsProcessingQueue(false);
  };

  const totalBytes = queue.reduce((sum, item) => sum + item.file.size, 0);
  const completedItems = queue.filter((i) => i.status === "complete");
  const completedBytes = queue.reduce(
    (sum, item) => sum + (item.status === "complete" ? item.file.size : (item.file.size * item.progress) / 100),
    0
  );
  const overallProgress = totalBytes > 0 ? Math.round((completedBytes / totalBytes) * 100) : 0;

  return (
    <div className="space-y-6">
      {/* Upload Configuration Bar */}
      <div className="rounded-lg border border-neutral-800 bg-neutral-900/50 p-4 space-y-4">
        <div className="grid grid-cols-1 gap-4 md:grid-cols-2">
          <div>
            <label className="mb-1 block text-xs font-medium text-neutral-300">Target Storage Location</label>
            <select
              value={selectedLocationId}
              disabled={loadingLocations || isProcessingQueue}
              onChange={(e) => setSelectedLocationId(e.target.value ? Number(e.target.value) : "")}
              className="w-full rounded bg-neutral-900 border border-neutral-700 px-3 py-2 text-sm text-neutral-200 focus:border-brand focus:outline-none"
            >
              {loadingLocations ? (
                <option value="">Loading storage locations…</option>
              ) : writableLocations.length === 0 ? (
                <option value="">No writable storage locations found</option>
              ) : (
                writableLocations.map((loc: StorageLocation) => (
                  <option key={loc.id} value={loc.id}>
                    {loc.name} ({loc.tier})
                  </option>
                ))
              )}
            </select>
            <p className="mt-1 text-xs text-neutral-500">
              Only writable storage locations are available for upload.
            </p>
          </div>

          <div>
            <label className="mb-1 block text-xs font-medium text-neutral-300">Organization & Naming</label>
            <div className="space-y-2">
              <label className="flex items-center gap-2 text-xs text-neutral-300">
                <input
                  type="radio"
                  name="organizationMode"
                  checked={applyNamingTemplate}
                  disabled={isProcessingQueue}
                  onChange={() => setApplyNamingTemplate(true)}
                  className="text-brand focus:ring-brand"
                />
                <span>Organize with Ingest Naming Template (from Settings)</span>
              </label>

              <label className="flex items-center gap-2 text-xs text-neutral-300">
                <input
                  type="radio"
                  name="organizationMode"
                  checked={!applyNamingTemplate}
                  disabled={isProcessingQueue}
                  onChange={() => setApplyNamingTemplate(false)}
                  className="text-brand focus:ring-brand"
                />
                <span>Preserve Folder Hierarchy / Custom Subfolder</span>
              </label>

              {!applyNamingTemplate && (
                <input
                  type="text"
                  placeholder="Optional subfolder (e.g. 2026/OldFootage)"
                  value={customSubdir}
                  disabled={isProcessingQueue}
                  onChange={(e) => setCustomSubdir(e.target.value)}
                  className="mt-1 w-full rounded bg-neutral-900 border border-neutral-700 px-3 py-1.5 text-xs text-neutral-200 placeholder:text-neutral-600 focus:border-brand focus:outline-none"
                />
              )}
            </div>
          </div>
        </div>
      </div>

      {/* Drag and Drop Zone */}
      <div
        onDragOver={(e) => {
          e.preventDefault();
          setIsDragging(true);
        }}
        onDragLeave={() => setIsDragging(false)}
        onDrop={handleDrop}
        className={`flex flex-col items-center justify-center rounded-lg border-2 border-dashed p-8 text-center transition-colors ${
          isDragging ? "border-brand bg-brand/10 text-brand" : "border-neutral-800 bg-neutral-900/30 text-neutral-400 hover:border-neutral-700"
        }`}
      >
        <svg
          className="mb-3 h-10 w-10 text-neutral-500"
          fill="none"
          stroke="currentColor"
          viewBox="0 0 24 24"
          xmlns="http://www.w3.org/2000/svg"
        >
          <path
            strokeLinecap="round"
            strokeLinejoin="round"
            strokeWidth={1.5}
            d="M7 16a4 4 0 01-.88-7.903A5 5 0 1115.9 6L16 6a5 5 0 011 9.9M15 13l-3-3m0 0l-3 3m3-3v12"
          />
        </svg>
        <p className="mb-1 text-sm font-medium text-neutral-200">
          Drag and drop media files or folders here
        </p>
        <p className="mb-4 text-xs text-neutral-500">
          Supports RAW camera photos, video clips, sidecar files (.xmp, .edl, .drp), and exports
        </p>

        <div className="flex flex-wrap gap-2">
          <button
            type="button"
            onClick={() => fileInputRef.current?.click()}
            disabled={isProcessingQueue || writableLocations.length === 0}
            className="rounded bg-neutral-800 px-3 py-1.5 text-xs font-medium text-neutral-200 hover:bg-neutral-700 disabled:opacity-50"
          >
            Choose Files
          </button>
          <button
            type="button"
            onClick={() => folderInputRef.current?.click()}
            disabled={isProcessingQueue || writableLocations.length === 0}
            className="rounded bg-neutral-800 px-3 py-1.5 text-xs font-medium text-neutral-200 hover:bg-neutral-700 disabled:opacity-50"
          >
            Choose Folder
          </button>
        </div>

        <input
          ref={fileInputRef}
          type="file"
          multiple
          onChange={handleFileSelect}
          className="hidden"
        />
        <input
          ref={folderInputRef}
          type="file"
          multiple
          // @ts-expect-error webkitdirectory is standard for folder pickers
          webkitdirectory="true"
          onChange={handleFolderSelect}
          className="hidden"
        />
      </div>

      {/* Upload Queue Section */}
      {queue.length > 0 && (
        <div className="space-y-4 rounded-lg border border-neutral-800 bg-neutral-900/40 p-4">
          <div className="flex flex-wrap items-center justify-between gap-4">
            <div>
              <h3 className="text-sm font-medium text-neutral-200">Upload Queue ({queue.length} files)</h3>
              <p className="text-xs text-neutral-400">
                {completedItems.length} of {queue.length} completed ({formatBytes(completedBytes)} of {formatBytes(totalBytes)})
              </p>
            </div>
            <div className="flex items-center gap-2">
              {completedItems.length > 0 && (
                <button
                  type="button"
                  onClick={clearCompleted}
                  disabled={isProcessingQueue}
                  className="rounded bg-neutral-800 px-3 py-1.5 text-xs font-medium text-neutral-300 hover:bg-neutral-700 disabled:opacity-50"
                >
                  Clear Completed
                </button>
              )}
              <button
                type="button"
                onClick={startUploads}
                disabled={isProcessingQueue || selectedLocationId === "" || queue.every((i) => i.status === "complete")}
                className="rounded bg-sky-700 px-4 py-1.5 text-xs font-medium text-white hover:bg-sky-600 disabled:opacity-50"
              >
                {isProcessingQueue ? "Uploading…" : "Start Upload"}
              </button>
            </div>
          </div>

          {/* Overall Progress Bar */}
          {queue.length > 0 && (
            <div className="h-2 w-full overflow-hidden rounded-full bg-neutral-800">
              <div
                className="h-full bg-sky-600 transition-all duration-300"
                style={{ width: `${overallProgress}%` }}
              />
            </div>
          )}

          {/* Queue Items List */}
          <div className="max-h-80 overflow-y-auto space-y-2 pr-1">
            {queue.map((item) => (
              <div
                key={item.id}
                className="flex items-center justify-between rounded border border-neutral-800/80 bg-neutral-900/80 p-2.5 text-xs text-neutral-300"
              >
                <div className="min-w-0 flex-1 pr-4">
                  <div className="flex items-center gap-2">
                    <span className="font-medium text-neutral-200 truncate">{item.file.name}</span>
                    <span className="text-neutral-500">({formatBytes(item.file.size)})</span>
                  </div>
                  {item.relativePath !== item.file.name && (
                    <div className="text-[11px] text-neutral-500 truncate">{item.relativePath}</div>
                  )}
                  {item.status === "uploading" && (
                    <div className="mt-1.5 h-1.5 w-full overflow-hidden rounded-full bg-neutral-800">
                      <div
                        className="h-full bg-sky-500 transition-all duration-150"
                        style={{ width: `${item.progress}%` }}
                      />
                    </div>
                  )}
                  {item.error && <div className="mt-1 text-[11px] text-red-400">{item.error}</div>}
                </div>

                <div className="flex items-center gap-2 shrink-0">
                  {item.status === "queued" && (
                    <span className="rounded bg-neutral-800 px-2 py-0.5 text-[11px] text-neutral-400">Queued</span>
                  )}
                  {item.status === "uploading" && (
                    <span className="rounded bg-sky-950 px-2 py-0.5 text-[11px] text-sky-400 font-mono">
                      {item.progress}%
                    </span>
                  )}
                  {item.status === "complete" && (
                    <div className="flex items-center gap-2">
                      <span className="rounded bg-emerald-950 px-2 py-0.5 text-[11px] text-emerald-400">Ready</span>
                      {item.response?.asset?.id && (
                        <Link
                          to={`/assets/${item.response.asset.id}`}
                          className="text-sky-400 underline hover:text-sky-300"
                        >
                          View Asset
                        </Link>
                      )}
                    </div>
                  )}
                  {item.status === "error" && (
                    <span className="rounded bg-red-950 px-2 py-0.5 text-[11px] text-red-400">Failed</span>
                  )}
                  {item.status !== "uploading" && (
                    <button
                      type="button"
                      onClick={() => removeItem(item.id)}
                      className="text-neutral-500 hover:text-red-400 p-1"
                      title="Remove from queue"
                      aria-label={`Remove ${item.file.name} from upload queue`}
                    >
                      ✕
                    </button>
                  )}
                </div>
              </div>
            ))}
          </div>
        </div>
      )}
    </div>
  );
}
