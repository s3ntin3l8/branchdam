import { useEffect } from "react";
import { useQueryClient } from "@tanstack/react-query";

/**
 * Subscribes to the SSE progress stream (GET /api/v1/events) and
 * invalidates the queries a scan affects on every nudge. Deliberately does
 * NOT parse the event's data payload -- the backend hub's design is "a
 * nudge means something changed, go re-fetch" (see internal/sse's package
 * doc), not "here is the new state." Re-fetching through TanStack Query
 * also gets retry/dedup/caching for free instead of hand-rolling it here.
 */
export function useEventStream() {
  const queryClient = useQueryClient();

  useEffect(() => {
    const source = new EventSource("/api/v1/events");

    const onProgress = () => {
      void queryClient.invalidateQueries({ queryKey: ["progress"] });
      void queryClient.invalidateQueries({ queryKey: ["assets"] });
      void queryClient.invalidateQueries({ queryKey: ["audit-queue"] });
      // The ingest jobs page (#52) and storage-health page (#51) were both
      // added after this hook and never wired in -- they previously only
      // updated on their own poll interval (15s / 10s respectively,
      // hooks/queries.ts) instead of reacting to a nudge like everything
      // else here.
      void queryClient.invalidateQueries({ queryKey: ["jobs"] });
      void queryClient.invalidateQueries({ queryKey: ["storage-health"] });
      // "asset" (singular, useAsset's key for GET /api/v1/assets/{id}) is a
      // DIFFERENT query key namespace from "assets" (plural, the list) --
      // TanStack Query's prefix matching only covers keys that start with the
      // exact given key array, so invalidating "assets" does NOT also cover
      // "asset". A background scan or graph-resolver pass changing a node's
      // graph_status -- not a manual confirm/reject/create action -- must
      // still refresh a mounted AssetDetailPage (its detail query, lineage,
      // and graph) within the query's staleTime, matching what queries.ts's
      // invalidateEdgeReviewQueries already does for the mutation path.
      void queryClient.invalidateQueries({ queryKey: ["asset"] });
      void queryClient.invalidateQueries({ queryKey: ["asset-lineage"] });
      void queryClient.invalidateQueries({ queryKey: ["asset-graph"] });
    };

    source.addEventListener("progress", onProgress);

    return () => {
      source.removeEventListener("progress", onProgress);
      source.close();
    };
  }, [queryClient]);
}
