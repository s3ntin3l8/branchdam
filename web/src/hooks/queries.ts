import { type QueryClient, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";

export function useMe() {
  return useQuery({ queryKey: ["me"], queryFn: api.me });
}

export function useConfig() {
  return useQuery({ queryKey: ["config"], queryFn: api.config });
}

export function usePathRewrites() {
  return useQuery({ queryKey: ["path-rewrites"], queryFn: api.listPathRewrites });
}

export function useStorageLocations() {
  return useQuery({ queryKey: ["storage-locations"], queryFn: api.listStorageLocations });
}

export function useAssets(params: import("../api/types").AssetQueryParams = {}) {
  return useQuery({
    queryKey: ["assets", params],
    queryFn: () => api.listAssets(params),
  });
}

export function useAssetFacets() {
  return useQuery({
    queryKey: ["asset-facets"],
    queryFn: api.getAssetFacets,
  });
}

export function useAsset(id: number | undefined) {
  return useQuery({
    queryKey: ["asset", id],
    queryFn: () => api.getAsset(id as number),
    enabled: id !== undefined,
  });
}

export function useAssetGraph(id: number | undefined) {
  return useQuery({
    queryKey: ["asset-graph", id],
    queryFn: () => api.getAssetGraph(id as number),
    enabled: id !== undefined,
  });
}

export function useAssetLineage(id: number | string | undefined, depth = 2) {
  return useQuery({
    queryKey: ["asset-lineage", id, depth],
    queryFn: () => api.getAssetLineage(id as number | string, depth),
    enabled: id !== undefined && id !== "",
  });
}

export function useUnlinkedCount() {
  return useQuery({
    queryKey: ["unlinked-count"],
    queryFn: async () => {
      const res = await api.listAssets({ limit: 500 });
      return res.assets.filter((a) => a.graphStatus === "UNLINKED").length;
    },
  });
}

export function useAuditQueue(params: { limit?: number; offset?: number } = {}) {
  return useQuery({
    queryKey: ["audit-queue", params],
    queryFn: () => api.listAuditQueue(params),
  });
}

// invalidateEdgeReviewQueries is shared by useConfirmEdge/useRejectEdge/
// useCreateEdge: any write that changes an edge's review_state also
// recomputes the target node's graph_status server-side (see
// internal/httpapi/routes.go's recomputeGraphStatus), so every query whose
// data depends on graph_status must be invalidated alongside the audit
// queue -- not just the queue itself, which is what confirm/reject were
// missing before this fix (they matched neither each other nor
// useCreateEdge, which already invalidated all of these).
function invalidateEdgeReviewQueries(queryClient: QueryClient) {
  void queryClient.invalidateQueries({ queryKey: ["audit-queue"] });
  void queryClient.invalidateQueries({ queryKey: ["assets"] });
  // "asset" (singular, useAsset's key for GET /api/v1/assets/{id}) is a
  // DIFFERENT query key namespace from "assets" (plural, the list) --
  // TanStack Query's prefix matching only covers keys that start with the
  // exact given key array, so invalidating "assets" does NOT also cover
  // "asset". AssetDetailPage renders graphStatus directly from this query,
  // so omitting it left a mounted detail page stale for the query's full
  // staleTime (main.tsx) after confirming/rejecting/creating an edge.
  void queryClient.invalidateQueries({ queryKey: ["asset"] });
  void queryClient.invalidateQueries({ queryKey: ["asset-lineage"] });
  void queryClient.invalidateQueries({ queryKey: ["asset-graph"] });
  void queryClient.invalidateQueries({ queryKey: ["unlinked-count"] });
}

export function useConfirmEdge() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.confirmEdge(id),
    onSuccess: () => invalidateEdgeReviewQueries(queryClient),
  });
}

export function useRejectEdge() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.rejectEdge(id),
    onSuccess: () => invalidateEdgeReviewQueries(queryClient),
  });
}

export function useCreateEdge() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: Parameters<typeof api.createEdge>[0]) => api.createEdge(input),
    onSuccess: () => invalidateEdgeReviewQueries(queryClient),
  });
}

export function useStartScan() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (storageLocationId: number) => api.startScan(storageLocationId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["progress"] });
    },
  });
}

export function useProgress(limit = 10) {
  return useQuery({
    queryKey: ["progress", limit],
    queryFn: () => api.listProgress(limit),
    // useEventStream (SSE) invalidates this query on every server-side
    // nudge, so polling is just a safety net for the case the connection
    // dropped without the browser noticing yet.
    refetchInterval: 15_000,
  });
}

export function useStorageHealth() {
  return useQuery({
    queryKey: ["storage-health"],
    queryFn: api.getStorageHealth,
    refetchInterval: 10_000,
  });
}

export function useJobs(params: import("../api/types").JobsQueryParams = {}) {
  return useQuery({
    queryKey: ["jobs", params],
    queryFn: () => api.listJobs(params),
    refetchInterval: 15_000,
  });
}
