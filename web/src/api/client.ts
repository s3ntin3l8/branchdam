import type { Asset, AssetGraph, AssetQueryParams, AssetSyncStatus, AuditEntry, Config, CreateEdgeInput, Edge, JobsQueryParams, LineageResponse, Me, PathRewrite, PostRestartResponse, PruneRequest, PruneResponse, PutSettingsRequest, PutStorageLocationRequest, ScanJob, SettingsResponse, StartScanRequest, StorageHealth, StorageLocation, UploadOptions, UploadProgressEvent, WebUploadResponse } from "./types";

export class ApiError extends Error {
  status: number;
  constructor(status: number, message: string) {
    super(message);
    this.status = status;
    this.name = "ApiError";
  }
}

async function request<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(path, {
    ...init,
    headers: { "Content-Type": "application/json", ...init?.headers },
  });
  if (!res.ok) {
    let detail = res.statusText;
    try {
      const body = (await res.json()) as { detail?: string };
      if (body.detail) detail = body.detail;
    } catch {
      // response body wasn't JSON -- fall back to statusText
    }
    throw new ApiError(res.status, detail);
  }
  if (res.status === 204) return undefined as T;
  return (await res.json()) as T;
}

export const api = {
  me: () => request<Me>("/api/v1/me"),
  config: () => request<Config>("/api/v1/config"),
  listPathRewrites: () => request<PathRewrite[]>("/api/v1/config/path-rewrites"),

  listStorageLocations: () => request<{ locations: StorageLocation[] }>("/api/v1/storage-locations"),

  listAssets: (params: AssetQueryParams = {}) => {
    const qs = new URLSearchParams();
    if (params.limit) qs.set("limit", String(params.limit));
    if (params.offset) qs.set("offset", String(params.offset));
    if (params.cameraModel) qs.set("cameraModel", params.cameraModel);
    if (params.graphStatus) qs.set("graphStatus", params.graphStatus);
    if (params.storageLocationId) qs.set("storageLocationId", String(params.storageLocationId));
    if (params.lifecycleState) qs.set("lifecycleState", params.lifecycleState);
    if (params.unlinkedOnly) qs.set("unlinkedOnly", "true");
    return request<{ assets: Asset[]; total: number }>(`/api/v1/assets?${qs.toString()}`);
  },
  getAssetFacets: () => request<{ cameraModels: string[] }>("/api/v1/assets/facets"),
  getAsset: (id: number) => request<Asset>(`/api/v1/assets/${id}`),
  getAssetGraph: (id: number) => request<AssetGraph>(`/api/v1/assets/${id}/graph`),
  getAssetLineage: (id: number | string, depth = 2) => request<LineageResponse>(`/api/v1/assets/${id}/lineage?depth=${depth}`),
  getAssetSyncStatus: (id: number) => request<AssetSyncStatus>(`/api/v1/assets/${id}/sync-status`),
  retrySync: (id: number) => request<{ requeued: number }>(`/api/v1/assets/${id}/sync/retry`, { method: "POST" }),

  // Not a request<T> call -- this is a plain URL for an <img src>, served
  // directly off the mux (see internal/httpapi/thumbnail.go), not through
  // Huma's JSON response path. The browser's own session cookie carries
  // auth, same as any other same-origin image request.
  thumbnailUrl: (id: number) => `/api/v1/assets/${id}/thumbnail`,

  listAuditQueue: (params: { limit?: number; offset?: number } = {}) => {
    const qs = new URLSearchParams();
    if (params.limit) qs.set("limit", String(params.limit));
    if (params.offset) qs.set("offset", String(params.offset));
    return request<{ entries: AuditEntry[] }>(`/api/v1/edges/audit?${qs}`);
  },
  confirmEdge: (id: number) => request<{ ok: boolean }>(`/api/v1/edges/${id}/confirm`, { method: "POST" }),
  rejectEdge: (id: number) => request<{ ok: boolean }>(`/api/v1/edges/${id}/reject`, { method: "POST" }),
  createEdge: (input: CreateEdgeInput) =>
    request<Edge>("/api/v1/edges", {
      method: "POST",
      body: JSON.stringify(input),
    }),

  inheritMetadata: (id: number) =>
    request<{ inherited: Record<string, string> }>(`/api/v1/assets/${id}/inherit-metadata`, { method: "POST" }),

  startScan: (input: StartScanRequest) =>
    request<{ jobId: number }>("/api/v1/scan", {
      method: "POST",
      body: JSON.stringify(input),
    }),
  listProgress: (limit = 10) => request<{ jobs: ScanJob[] }>(`/api/v1/progress?limit=${limit}`),
  listJobs: (params: JobsQueryParams = {}) => {
    const qs = new URLSearchParams();
    if (params.limit) qs.set("limit", String(params.limit));
    if (params.offset) qs.set("offset", String(params.offset));
    if (params.kind) qs.set("kind", params.kind);
    if (params.state) qs.set("state", params.state);
    return request<{ jobs: ScanJob[]; total: number }>(`/api/v1/jobs?${qs.toString()}`);
  },

  getStorageHealth: () => request<StorageHealth>("/api/v1/storage-health"),
  deleteAgentTelemetry: (agentId: string) =>
    request<{ ok: boolean }>(`/api/v1/storage-health/agents/${encodeURIComponent(agentId)}`, {
      method: "DELETE",
    }),
  putStorageLocation: (id: number, input: PutStorageLocationRequest) =>
    request<{ ok: boolean }>(`/api/v1/storage-locations/${id}`, {
      method: "PUT",
      body: JSON.stringify(input),
    }),

  pruneCache: (input: PruneRequest) =>
    request<PruneResponse>("/api/v1/prune", {
      method: "POST",
      body: JSON.stringify(input),
    }),

  getSettings: () => request<SettingsResponse>("/api/v1/settings"),
  putSettings: (input: PutSettingsRequest) =>
    request<SettingsResponse>("/api/v1/settings", {
      method: "PUT",
      body: JSON.stringify(input),
    }),
  postRestart: () => request<PostRestartResponse>("/api/v1/restart", { method: "POST" }),

  uploadFile: (
    file: File,
    options: UploadOptions = {},
    onProgress?: (event: UploadProgressEvent) => void,
    signal?: AbortSignal
  ): Promise<WebUploadResponse> => {
    return new Promise((resolve, reject) => {
      const xhr = new XMLHttpRequest();
      xhr.open("POST", "/api/v1/upload");

      if (signal) {
        signal.addEventListener("abort", () => {
          xhr.abort();
          reject(new DOMException("Upload aborted", "AbortError"));
        });
      }

      if (onProgress && xhr.upload) {
        xhr.upload.onprogress = (e) => {
          if (e.lengthComputable) {
            const percent = Math.round((e.loaded / e.total) * 100);
            onProgress({ loaded: e.loaded, total: e.total, percent });
          }
        };
      }

      xhr.onload = () => {
        if (xhr.status >= 200 && xhr.status < 300) {
          try {
            const data = JSON.parse(xhr.responseText) as WebUploadResponse;
            resolve(data);
          } catch {
            reject(new ApiError(xhr.status, "Invalid JSON response"));
          }
        } else {
          let detail = xhr.statusText;
          try {
            const errBody = JSON.parse(xhr.responseText) as { error?: string; detail?: string };
            detail = errBody.error || errBody.detail || detail;
          } catch {
            // non-json response
          }
          reject(new ApiError(xhr.status, detail));
        }
      };

      xhr.onerror = () => {
        reject(new ApiError(xhr.status || 0, "Network error during upload"));
      };

      const formData = new FormData();
      formData.append("file", file, file.name);
      if (options.storageLocationId) {
        formData.append("storageLocationId", String(options.storageLocationId));
      }
      if (options.relativePath) {
        formData.append("relativePath", options.relativePath);
      }
      if (options.applyNamingTemplate !== undefined) {
        formData.append("applyNamingTemplate", String(options.applyNamingTemplate));
      }
      if (options.overrideCameraModel) {
        formData.append("overrideCameraModel", options.overrideCameraModel);
      }
      if (options.overrideCapturedAt) {
        formData.append("overrideCapturedAt", String(options.overrideCapturedAt));
      }

      xhr.send(formData);
    });
  },
};
