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
    };

    source.addEventListener("progress", onProgress);

    return () => {
      source.removeEventListener("progress", onProgress);
      source.close();
    };
  }, [queryClient]);
}
