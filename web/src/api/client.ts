import type { Asset, AssetGraph, AuditEntry, Config, CreateEdgeInput, Edge, LineageResponse, Me, PathRewrite, ScanJob, StorageLocation } from "./types";

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

  listAssets: (params: { limit?: number; offset?: number } = {}) => {
    const qs = new URLSearchParams();
    if (params.limit) qs.set("limit", String(params.limit));
    if (params.offset) qs.set("offset", String(params.offset));
    return request<{ assets: Asset[] }>(`/api/v1/assets?${qs}`);
  },
  getAsset: (id: number) => request<Asset>(`/api/v1/assets/${id}`),
  getAssetGraph: (id: number) => request<AssetGraph>(`/api/v1/assets/${id}/graph`),
  getAssetLineage: (id: number | string, depth = 2) => request<LineageResponse>(`/api/v1/assets/${id}/lineage?depth=${depth}`),

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

  startScan: (storageLocationId: number) =>
    request<{ jobId: number }>("/api/v1/scan", {
      method: "POST",
      body: JSON.stringify({ storageLocationId }),
    }),
  listProgress: (limit = 10) => request<{ jobs: ScanJob[] }>(`/api/v1/progress?limit=${limit}`),
};
