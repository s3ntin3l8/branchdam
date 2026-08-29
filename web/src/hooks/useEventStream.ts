import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ASSET_DETAIL_QUERY_KEYS } from "./queries";

export type ConnectionState = "connecting" | "open" | "disconnected";

/**
 * Subscribes to the SSE progress stream (GET /api/v1/events) and
 * invalidates the queries a scan affects on every nudge. Deliberately does
 * NOT parse the event's data payload -- the backend hub's design is "a
 * nudge means something changed, go re-fetch" (see internal/sse's package
 * doc), not "here is the new state." Re-fetching through TanStack Query
 * also gets retry/dedup/caching for free instead of hand-rolling it here.
 */
export function useEventStream(): { connectionState: ConnectionState } {
  const queryClient = useQueryClient();
  const [connectionState, setConnectionState] = useState<ConnectionState>("connecting");

  useEffect(() => {
    const source = new EventSource("/api/v1/events");

    source.onopen = () => setConnectionState("open");

    source.onerror = () => setConnectionState("disconnected");

    const onProgress = () => {
      setConnectionState("open");
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
      // A settings write from a second browser session broadcasts the same
      // coarse nudge as a scan progress tick (the hub has no per-topic
      // channel) -- this session's SettingsPage, if mounted, must still
      // pick up the change within the nudge's latency rather than waiting
      // out its own staleTime.
      void queryClient.invalidateQueries({ queryKey: ["settings"] });
      // A background scan or graph-resolver pass changing a node's
      // graph_status -- not a manual confirm/reject/create action -- must
      // still refresh a mounted AssetDetailPage (its detail query, lineage,
      // and graph) within the query's staleTime, matching what queries.ts's
      // invalidateEdgeReviewQueries already does for the mutation path. See
      // ASSET_DETAIL_QUERY_KEYS's doc comment for why "asset" is a separate
      // namespace from "assets" above.
      for (const key of ASSET_DETAIL_QUERY_KEYS) {
        void queryClient.invalidateQueries({ queryKey: [key] });
      }
    };

    source.addEventListener("progress", onProgress);

    return () => {
      source.removeEventListener("progress", onProgress);
      source.close();
    };
  }, [queryClient]);

  return { connectionState };
}
