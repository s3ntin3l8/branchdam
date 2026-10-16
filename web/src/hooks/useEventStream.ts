import { useEffect, useState } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { ASSET_DETAIL_QUERY_KEYS } from "./queries";

const EVENTS_URL = "/api/v1/events";

// EventSource.readyState value for a stream the browser has given up on.
// Spelled out rather than read off the EventSource global so a test fake
// without the static constants still exercises the same branch.
const READY_STATE_CLOSED = 2;

// Reconnect backoff once the browser has given up on the stream: 1s, 2s,
// 4s, ... capped at 30s, reset to 1s by the next successful open.
export const SSE_RECONNECT_BASE_MS = 1_000;
export const SSE_RECONNECT_MAX_MS = 30_000;

export function sseReconnectDelay(attempt: number): number {
  return Math.min(SSE_RECONNECT_BASE_MS * 2 ** attempt, SSE_RECONNECT_MAX_MS);
}

/**
 * Subscribes to the SSE progress stream (GET /api/v1/events) and
 * invalidates the queries a scan affects on every nudge. Deliberately does
 * NOT parse the event's data payload -- the backend hub's design is "a
 * nudge means something changed, go re-fetch" (see internal/sse's package
 * doc), not "here is the new state." Re-fetching through TanStack Query
 * also gets retry/dedup/caching for free instead of hand-rolling it here.
 *
 * `enabled` is the caller's "this session is signed in" verdict. No stream
 * is opened while it is false: the server answers an unauthenticated
 * /api/v1/events with 403, which puts the EventSource into CLOSED. When it
 * flips to true (an in-SPA login, an MFA challenge answered) a fresh stream
 * opens immediately rather than waiting for a hard reload.
 *
 * The browser's EventSource only retries on its own after a dropped
 * connection (readyState CONNECTING). A non-200 answer -- 403 after a lost
 * session, 429/503 from the server's per-account and global stream limiters
 * -- leaves it CLOSED for good, so this hook recreates it with capped
 * exponential backoff (sseReconnectDelay) and asks /api/v1/me to re-check
 * the session, which lets Layout send a signed-out user to /login.
 */
export function useEventStream({ enabled }: { enabled: boolean }): { disconnected: boolean } {
  const queryClient = useQueryClient();
  const [disconnected, setDisconnected] = useState(false);

  useEffect(() => {
    if (!enabled) return;

    let source: EventSource | null = null;
    let debounceTimer: ReturnType<typeof setTimeout> | null = null;
    let reconnectTimer: ReturnType<typeof setTimeout> | null = null;
    let attempt = 0;

    const onOpen = () => {
      attempt = 0;
      setDisconnected(false);
    };

    const onProgress = () => {
      setDisconnected(false);
      if (debounceTimer) {
        clearTimeout(debounceTimer);
      }
      debounceTimer = setTimeout(() => {
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
        // The nav badge (unlinked-count) and the asset list's filter facets
        // are derived from the same rows a scan/agent ingest changes; they
        // were never refreshed here and stayed stale until a full reload.
        void queryClient.invalidateQueries({ queryKey: ["unlinked-count"] });
        void queryClient.invalidateQueries({ queryKey: ["asset-facets"] });
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
        // The same coarse nudge also covers admin actions taken in another
        // session (audit-log rows, pairing changes, user edits) and the
        // Immich sync worker's per-asset status, which the pages above
        // otherwise only saw on their own staleTime or poll interval.
        void queryClient.invalidateQueries({ queryKey: ["audit-entries"] });
        void queryClient.invalidateQueries({ queryKey: ["companion-pairings"] });
        void queryClient.invalidateQueries({ queryKey: ["users"] });
        void queryClient.invalidateQueries({ queryKey: ["asset-sync-status"] });
      }, 200);
    };

    const detach = (es: EventSource) => {
      es.removeEventListener("open", onOpen);
      es.removeEventListener("error", onError);
      es.removeEventListener("progress", onProgress);
      es.close();
    };

    function onError() {
      setDisconnected(true);
      if (!source || source.readyState !== READY_STATE_CLOSED || reconnectTimer) {
        // CONNECTING: the browser is already retrying a dropped connection.
        return;
      }
      detach(source);
      source = null;
      // A CLOSED stream is the only signal this hook gets of a 403 (session
      // lost). Re-checking /me lets Layout redirect a signed-out user; for a
      // 429/503 it is a cheap no-op.
      void queryClient.invalidateQueries({ queryKey: ["me"] });
      reconnectTimer = setTimeout(connect, sseReconnectDelay(attempt));
      attempt += 1;
    }

    function connect() {
      reconnectTimer = null;
      const es = new EventSource(EVENTS_URL);
      source = es;
      es.addEventListener("open", onOpen);
      es.addEventListener("error", onError);
      es.addEventListener("progress", onProgress);
    }

    connect();

    return () => {
      if (debounceTimer) {
        clearTimeout(debounceTimer);
      }
      if (reconnectTimer) {
        clearTimeout(reconnectTimer);
      }
      if (source) {
        detach(source);
      }
      setDisconnected(false);
    };
  }, [enabled, queryClient]);

  // A stream that isn't supposed to be open isn't "reconnecting".
  return { disconnected: enabled && disconnected };
}
