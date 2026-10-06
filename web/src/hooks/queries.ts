import { type QueryClient, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { ApiError, api } from "../api/client";

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

// useUsers backs the asset list's "uploaded by" column lookup, the
// pairing UI's "Owned by" selector, and the Users administration page.
// /api/v1/users is admin-only and returns 503 in deployments without
// attribution wired; the SPA tolerates that as "empty cache".
export function useUsers(
  params: { limit?: number; offset?: number } = {},
  options: { enabled?: boolean } = {}
) {
  return useQuery({
    queryKey: ["users", params],
    queryFn: () => api.listUsers(params),
    // Non-admins get 403 from this admin-only endpoint, and every 401/403
    // raises the global "Access denied" banner -- callers that can't know
    // the user is an admin must gate on useMe().isAdmin.
    enabled: options.enabled ?? true,
    retry: (failureCount, error) => {
      // 503 = feature disabled, 401/403 = not allowed: retrying can't help
      // and only repeats the console noise / auth-error banner.
      if (error instanceof ApiError && [401, 403, 503].includes(error.status)) return false;
      return failureCount < 2;
    },
  });
}

export function useCreateUser() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: import("../api/types").CreateUserInput) => api.createUser(input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
    },
  });
}

export function useAdminResetPassword() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (userId: number) => api.adminResetPassword(userId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
    },
  });
}

export function useDisableUser() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (userId: number) => api.disableUser(userId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
    },
  });
}

export function useEnableUser() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (userId: number) => api.enableUser(userId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
    },
  });
}

export function useUpdateUser() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ userId, input }: { userId: number; input: import("../api/types").UpdateUserInput }) =>
      api.updateUser(userId, input),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
    },
  });
}

export function useRevokeUserSessions() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (userId: number) => api.revokeUserSessions(userId),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["users"] });
    },
  });
}

export function useAudit(params: import("../api/types").AuditQueryParams = {}) {
  return useQuery({
    queryKey: ["audit", params],
    queryFn: () => api.listAudit(params),
  });
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

export function useSettings() {
  return useQuery({ queryKey: ["settings"], queryFn: api.getSettings });
}

// A settings write can change almost anything a request handler reads
// (internal/httpapi's Server.cfg() resolves live), so a successful PUT
// invalidates every query whose data could depend on it -- mirroring
// invalidateEdgeReviewQueries's "the write's blast radius, not just the
// resource that was written" shape below.
export function invalidateSettingsQueries(queryClient: QueryClient) {
  void queryClient.invalidateQueries({ queryKey: ["settings"] });
  void queryClient.invalidateQueries({ queryKey: ["config"] });
  void queryClient.invalidateQueries({ queryKey: ["path-rewrites"] });
  void queryClient.invalidateQueries({ queryKey: ["storage-locations"] });
  void queryClient.invalidateQueries({ queryKey: ["storage-health"] });
}

export function usePutSettings() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: import("../api/types").PutSettingsRequest) => api.putSettings(input),
    onSuccess: () => invalidateSettingsQueries(queryClient),
  });
}

// useRestartServer backs both the Settings page's "Restart server" card and
// the Storage Health page's conditional "Restart to apply" button -- same
// mutation, same POST /api/v1/restart. The server is briefly unreachable
// immediately after this resolves (see internal/httpapi/restart.go's
// restartGraceDelay and cmd/branchdam's re-exec), so onSuccess deliberately
// just invalidates the same broad set usePutSettings does rather than
// polling: useStorageHealth's existing 10s refetchInterval and the default
// retry/refetch-on-focus behavior are what actually bring the pages back
// once the process is listening again.
export function useRestartServer() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: () => api.postRestart(),
    onSuccess: () => invalidateSettingsQueries(queryClient),
  });
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

export function useAssetMetadata(id: number | undefined) {
  return useQuery({
    queryKey: ["asset-metadata", id],
    queryFn: () => api.getAssetMetadata(id as number),
    enabled: id !== undefined && !Number.isNaN(id),
  });
}

// A lifecycle change (trash/restore/delete) also changes the asset's
// neighbours' lineage and graph, the unlinked badge, the list facets and
// per-location health, none of which live under the "assets"/"asset" keys.
function invalidateAssetLifecycleQueries(queryClient: QueryClient, id: number) {
  void queryClient.invalidateQueries({ queryKey: ["assets"] });
  void queryClient.invalidateQueries({ queryKey: ["asset", id] });
  void queryClient.invalidateQueries({ queryKey: ["asset-metadata", id] });
  void queryClient.invalidateQueries({ queryKey: ["asset-lineage"] });
  void queryClient.invalidateQueries({ queryKey: ["asset-graph"] });
  void queryClient.invalidateQueries({ queryKey: ["unlinked-count"] });
  void queryClient.invalidateQueries({ queryKey: ["asset-facets"] });
  void queryClient.invalidateQueries({ queryKey: ["storage-health"] });
}

export function useDeleteAsset() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.deleteAsset(id),
    onSuccess: (_data, id) => {
      invalidateAssetLifecycleQueries(queryClient, id);
    },
  });
}

export function useTrashAsset() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, keepExports }: { id: number; keepExports: boolean }) =>
      api.trashAsset(id, { keepExports }),
    onSuccess: (_data, vars) => {
      invalidateAssetLifecycleQueries(queryClient, vars.id);
    },
  });
}

export function useRestoreAsset() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.restoreAsset(id),
    onSuccess: (_data, id) => {
      invalidateAssetLifecycleQueries(queryClient, id);
    },
  });
}

export function useAuditEntries(params: import("../api/types").AuditQueryParams = {}, enabled: boolean = true) {
  return useQuery({
    queryKey: ["audit-entries", params],
    queryFn: () => api.listAudit(params),
    enabled,
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
      void queryClient.invalidateQueries({ queryKey: ["asset-metadata", id] });
    },
  });
}

export function useUnlinkedCount() {
  return useQuery({
    queryKey: ["unlinked-count"],
    queryFn: async () => {
      const res = await api.listAssets({ unlinkedOnly: true, limit: 1 });
      return res.total;
    },
  });
}

export function useAuditQueue(params: { limit?: number; beforeId?: number } = {}) {
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

// useInvalidateEdgeReviewQueries lets a batch caller (the audit queue's
// bulk confirm/reject) run N edge writes and refresh once at the end,
// instead of paying the full invalidation fan-out after every edge.
export function useInvalidateEdgeReviewQueries() {
  const queryClient = useQueryClient();
  return () => invalidateEdgeReviewQueries(queryClient);
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

// A folder drop uploads hundreds of files back to back; refreshing the list,
// badge and health after EACH one refetches them N times. Coalesce into one
// trailing refresh once the burst goes quiet.
const UPLOAD_REFRESH_DELAY_MS = 750;
let uploadRefreshTimer: ReturnType<typeof setTimeout> | null = null;
function scheduleUploadRefresh(queryClient: QueryClient) {
  if (uploadRefreshTimer) clearTimeout(uploadRefreshTimer);
  uploadRefreshTimer = setTimeout(() => {
    uploadRefreshTimer = null;
    void queryClient.invalidateQueries({ queryKey: ["assets"] });
    void queryClient.invalidateQueries({ queryKey: ["unlinked-count"] });
    void queryClient.invalidateQueries({ queryKey: ["storage-health"] });
  }, UPLOAD_REFRESH_DELAY_MS);
}

export function useUploadFile() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({
      file,
      options,
      onProgress,
      signal,
    }: {
      file: File;
      options?: import("../api/types").UploadOptions;
      onProgress?: (event: import("../api/types").UploadProgressEvent) => void;
      signal?: AbortSignal;
    }) => api.uploadFile(file, options, onProgress, signal),
    onSuccess: () => scheduleUploadRefresh(queryClient),
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
// via nodeIds. Invalidated on success rather than waiting for the next SSE
// nudge (useEventStream also refreshes "storage-health", but only when the
// server broadcasts one), so a caller that needs the fresh count right away
// doesn't depend on that.
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

// usePutStorageLocation backs LocationGaugeCard's inline edit form. Success
// invalidates storage-health (the page reads it directly) and
// storage-locations (used elsewhere for the manual-scan target picker,
// which enabled:false ought to exclude a location from as soon as it's set).
export function usePutStorageLocation() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({ id, input }: { id: number; input: import("../api/types").PutStorageLocationRequest }) =>
      api.putStorageLocation(id, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["storage-health"] });
      void queryClient.invalidateQueries({ queryKey: ["storage-locations"] });
    },
  });
}

export function useDeleteAgentTelemetry() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (agentId: string) => api.deleteAgentTelemetry(agentId),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["storage-health"] });
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

export function useCancelJob() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.cancelJob(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["jobs"] });
      void queryClient.invalidateQueries({ queryKey: ["progress"] });
      void queryClient.invalidateQueries({ queryKey: ["storage-health"] });
    },
  });
}

// Companion pairing hooks. All four mutations invalidate the list query
// on success so the SPA's pairings table stays in sync without manual
// refetch. Detail-view reads aren't auto-invalidated -- those are only
// pulled on a direct /companion/pairings/{id} navigation.

export function usePairings() {
  return useQuery({
    queryKey: ["companion-pairings"],
    queryFn: api.listPairings,
  });
}

export function usePairing(id: number | undefined) {
  return useQuery({
    queryKey: ["companion-pairing", id],
    queryFn: () => api.getPairing(id as number),
    enabled: id !== undefined,
  });
}

export function useCreatePairing() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (input: import("../api/types").CreateCompanionPairingRequest) =>
      api.createPairing(input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["companion-pairings"] });
    },
  });
}

export function useRotatePairing() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      input,
    }: {
      id: number;
      input: import("../api/types").RotateCompanionPairingRequest;
    }) => api.rotatePairing(id, input),
    onSuccess: (_data, vars) => {
      void queryClient.invalidateQueries({ queryKey: ["companion-pairings"] });
      void queryClient.invalidateQueries({ queryKey: ["companion-pairing", vars.id] });
      void queryClient.invalidateQueries({ queryKey: ["companion-pairing-audit"] });
    },
  });
}

export function useRevokePairing() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.revokePairing(id),
    onSuccess: (_data, id) => {
      void queryClient.invalidateQueries({ queryKey: ["companion-pairings"] });
      void queryClient.invalidateQueries({ queryKey: ["companion-pairing", id] });
      void queryClient.invalidateQueries({ queryKey: ["companion-pairing-audit"] });
    },
  });
}

export function useDeletePairing() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: (id: number) => api.deletePairing(id),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["companion-pairings"] });
    },
  });
}

// useRenamePairing invalidates the list so the table label updates in
// place. Credentials are fetched imperatively (not via React Query) so
// each open/close of the show-credentials modal does a fresh audited
// reveal -- see CompanionPairingsPage's handleShowCredentials.
export function useRenamePairing() {
  const queryClient = useQueryClient();
  return useMutation({
    mutationFn: ({
      id,
      input,
    }: {
      id: number;
      input: import("../api/types").RenameCompanionPairingRequest;
    }) => api.renamePairing(id, input),
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["companion-pairing-audit"] });
      void queryClient.invalidateQueries({ queryKey: ["companion-pairings"] });
    },
  });
}

export function usePairingAudit(id: number | undefined, params: { limit?: number; offset?: number } = {}) {
  return useQuery({
    queryKey: ["companion-pairing-audit", id, params],
    queryFn: () => api.pairingAudit(id as number, params),
    enabled: id !== undefined,
  });
}
