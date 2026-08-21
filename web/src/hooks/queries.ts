import { type QueryClient, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";

// The query key namespaces backing a single asset's detail page (useAsset,
// useAssetGraph, useAssetLineage) -- "asset" (singular) is a DIFFERENT
// namespace from "assets" (plural, the list), so invalidating "assets" does
// NOT also cover these. Shared between invalidateEdgeReviewQueries below
// (the mutation-triggered path) and useEventStream's SSE-nudge path so the
// two invalidation lists can't drift apart the way they did before #153.
export const ASSET_DETAIL_QUERY_KEYS = ["asset", "asset-lineage", "asset-graph"] as const;

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

export function useAssetSyncStatus(id: number | undefined) {
  return useQuery({
    queryKey: ["asset-sync-status", id],
    queryFn: () => api.getAssetSyncStatus(id as number),
    enabled: id !== undefined,
    // Poll so the status stays fresh while the sync worker progresses
    // (useEventStream only nudges on SSE events, not on worker drain ticks).
    refetchInterval: 15_000,
  });
}

export function useRetrySync() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.retrySync(id),
    onSuccess: (_data, id) => {
      void queryClient.invalidateQueries({ queryKey: ["asset-sync-status", id] });
    },
  });
}

export function useInheritMetadata() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.inheritMetadata(id),
    onSuccess: (_data, id) => {
      // The endpoint rewrites the file in place and refreshes size/hash/
      // indexing_status on the node (see internal/httpapi's
      // refreshNodeAfterInPlaceWrite) -- re-fetch the asset so the Metadata
      // panel reflects the new file state, not the pre-write one.
      void queryClient.invalidateQueries({ queryKey: ["asset", id] });
    },
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
  // AssetDetailPage renders graphStatus directly from these queries, so
  // omitting them left a mounted detail page stale for the query's full
  // staleTime (main.tsx) after confirming/rejecting/creating an edge.
  for (const key of ASSET_DETAIL_QUERY_KEYS) {
    void queryClient.invalidateQueries({ queryKey: [key] });
  }
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
    mutationFn: (input: Parameters<typeof api.startScan>[0]) => api.startScan(input),
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

// usePruneCache backs both the Storage Health page's location-level purge
// control and AssetDetailPage's per-asset [Purge Cache] action -- same
// endpoint, the caller decides dry-run vs. execute and whether to narrow
// via nodeIds. Invalidated on success rather than relying on the SSE nudge
// alone: useEventStream doesn't invalidate "storage-health", so a caller
// that needs the fresh count right away shouldn't wait on that 10s poll.
export function usePruneCache() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: import("../api/types").PruneRequest) => api.pruneCache(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["storage-health"] });
      void queryClient.invalidateQueries({ queryKey: ["assets"] });
      void queryClient.invalidateQueries({ queryKey: ["asset"] });
    },
  });
}

export function useJobs(params: import("../api/types").JobsQueryParams = {}) {
  return useQuery({
    queryKey: ["jobs", params],
    queryFn: () => api.listJobs(params),
    refetchInterval: 15_000,
  });
}
