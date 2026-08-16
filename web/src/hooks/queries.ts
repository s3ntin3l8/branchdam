import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { api } from "../api/client";

export function useMe() {
  return useQuery({ queryKey: ["me"], queryFn: api.me });
}

export function useConfig() {
  return useQuery({ queryKey: ["config"], queryFn: api.config });
}

export function useStorageLocations() {
  return useQuery({ queryKey: ["storage-locations"], queryFn: api.listStorageLocations });
}

export function useAssets(params: { limit?: number; offset?: number } = {}) {
  return useQuery({
    queryKey: ["assets", params],
    queryFn: () => api.listAssets(params),
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

export function useAuditQueue(params: { limit?: number; offset?: number } = {}) {
  return useQuery({
    queryKey: ["audit-queue", params],
    queryFn: () => api.listAuditQueue(params),
  });
}

export function useConfirmEdge() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.confirmEdge(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["audit-queue"] });
    },
  });
}

export function useRejectEdge() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.rejectEdge(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["audit-queue"] });
    },
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
