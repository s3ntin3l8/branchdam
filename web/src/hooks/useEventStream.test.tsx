import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { SSE_RECONNECT_MAX_MS, sseReconnectDelay, useEventStream } from "./useEventStream";

// jsdom doesn't implement EventSource -- a minimal fake is enough to drive
// the "progress", "error", and "open" listeners this hook registers.
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  private listeners: Record<string, Array<() => void>> = {};
  url: string;
  // 0 CONNECTING, 1 OPEN, 2 CLOSED -- tests set 2 to simulate a non-200
  // answer (403/429/503) after which the browser stops retrying.
  readyState = 0;
  closed = false;
  onerror: (() => void) | null = null;
  onopen: (() => void) | null = null;

  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }

  addEventListener(type: string, cb: () => void) {
    (this.listeners[type] ??= []).push(cb);
  }

  removeEventListener(type: string, cb: () => void) {
    this.listeners[type] = (this.listeners[type] ?? []).filter((l) => l !== cb);
  }

  close() {
    this.closed = true;
    this.readyState = 2;
  }

  emit(type: string) {
    for (const cb of this.listeners[type] ?? []) cb();
    if (type === "error" && this.onerror) this.onerror();
    if (type === "open" && this.onopen) this.onopen();
  }
}

function wrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  };
}

describe("useEventStream", () => {
  const originalEventSource = globalThis.EventSource;

  beforeEach(() => {
    vi.useFakeTimers();
    FakeEventSource.instances = [];
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    globalThis.EventSource = FakeEventSource as any;
  });

  afterEach(() => {
    vi.useRealTimers();
    globalThis.EventSource = originalEventSource;
  });

  it("invalidates the graph_status-dependent asset queries on a progress nudge after 200ms debounce", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const spy = vi.spyOn(queryClient, "invalidateQueries");

    renderHook(() => useEventStream({ enabled: true }), { wrapper: wrapper(queryClient) });

    expect(FakeEventSource.instances).toHaveLength(1);
    act(() => {
      FakeEventSource.instances[0].emit("progress");
    });

    // Before debounce fires
    expect(spy).not.toHaveBeenCalled();

    // Advance 200ms
    act(() => {
      vi.advanceTimersByTime(200);
    });

    const keys = spy.mock.calls.map(([filters]) => filters?.queryKey);
    expect(keys).toEqual([
      ["progress"],
      ["assets"],
      ["audit-queue"],
      ["jobs"],
      ["storage-health"],
      ["unlinked-count"],
      ["asset-facets"],
      ["settings"],
      ["asset"],
      ["asset-lineage"],
      ["asset-graph"],
      ["audit-entries"],
      ["companion-pairings"],
      ["users"],
      ["asset-sync-status"],
    ]);
  });

  it("debounces rapid-fire progress nudges to avoid refetch churn (#357)", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const spy = vi.spyOn(queryClient, "invalidateQueries");

    renderHook(() => useEventStream({ enabled: true }), { wrapper: wrapper(queryClient) });

    const es = FakeEventSource.instances[0];

    // Emit 3 progress events in quick succession
    act(() => {
      es.emit("progress");
    });
    act(() => {
      vi.advanceTimersByTime(50);
      es.emit("progress");
    });
    act(() => {
      vi.advanceTimersByTime(50);
      es.emit("progress");
    });

    // 100ms passed total, debounce timer was reset twice, no invalidation yet
    expect(spy).not.toHaveBeenCalled();

    // Wait full 200ms from last event
    act(() => {
      vi.advanceTimersByTime(200);
    });

    // Only 15 invalidation calls (one batch for the 15 query keys)
    expect(spy).toHaveBeenCalledTimes(15);
    const keys = spy.mock.calls.map((c) => JSON.stringify(c[0]?.queryKey));
    expect(keys).toContain(JSON.stringify(["unlinked-count"]));
    expect(keys).toContain(JSON.stringify(["asset-facets"]));
  });

  it("tracks disconnected state on error and clears it on open or progress (#349)", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    const { result } = renderHook(() => useEventStream({ enabled: true }), { wrapper: wrapper(queryClient) });

    expect(result.current.disconnected).toBe(false);

    const es = FakeEventSource.instances[0];

    // Server drops connection / triggers error
    act(() => {
      es.emit("error");
    });
    expect(result.current.disconnected).toBe(true);

    // Browser reconnects and fires open
    act(() => {
      es.emit("open");
    });
    expect(result.current.disconnected).toBe(false);

    // Error again
    act(() => {
      es.emit("error");
    });
    expect(result.current.disconnected).toBe(true);

    // Nudge arrives
    act(() => {
      es.emit("progress");
    });
    expect(result.current.disconnected).toBe(false);
  });

  it("opens no stream while signed out", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { result } = renderHook(() => useEventStream({ enabled: false }), { wrapper: wrapper(queryClient) });

    act(() => {
      vi.advanceTimersByTime(60_000);
    });
    expect(FakeEventSource.instances).toHaveLength(0);
    expect(result.current.disconnected).toBe(false);
  });

  it("opens a stream as soon as the session becomes signed in", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { rerender } = renderHook(({ enabled }) => useEventStream({ enabled }), {
      wrapper: wrapper(queryClient),
      initialProps: { enabled: false },
    });
    expect(FakeEventSource.instances).toHaveLength(0);

    rerender({ enabled: true });
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(FakeEventSource.instances[0].url).toBe("/api/v1/events");

    // Signing out closes it again.
    rerender({ enabled: false });
    expect(FakeEventSource.instances[0].closed).toBe(true);
  });

  it("recreates the stream with capped exponential backoff once it is CLOSED", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const spy = vi.spyOn(queryClient, "invalidateQueries");
    const { result } = renderHook(() => useEventStream({ enabled: true }), { wrapper: wrapper(queryClient) });

    const closeLatest = () => {
      const es = FakeEventSource.instances[FakeEventSource.instances.length - 1];
      es.readyState = 2;
      act(() => es.emit("error"));
      return es;
    };

    // 1st failure: 1s backoff.
    const first = closeLatest();
    expect(first.closed).toBe(true);
    expect(result.current.disconnected).toBe(true);
    // A CLOSED stream re-checks the session so Layout can redirect.
    expect(spy.mock.calls.map(([f]) => f?.queryKey)).toContainEqual(["me"]);
    act(() => vi.advanceTimersByTime(999));
    expect(FakeEventSource.instances).toHaveLength(1);
    act(() => vi.advanceTimersByTime(1));
    expect(FakeEventSource.instances).toHaveLength(2);

    // 2nd failure: 2s backoff.
    closeLatest();
    act(() => vi.advanceTimersByTime(1_999));
    expect(FakeEventSource.instances).toHaveLength(2);
    act(() => vi.advanceTimersByTime(1));
    expect(FakeEventSource.instances).toHaveLength(3);

    // A successful open resets the backoff to 1s and clears the flag.
    act(() => FakeEventSource.instances[2].emit("open"));
    expect(result.current.disconnected).toBe(false);
    closeLatest();
    act(() => vi.advanceTimersByTime(1_000));
    expect(FakeEventSource.instances).toHaveLength(4);
  });

  it("leaves a CONNECTING stream to the browser's own retry", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderHook(() => useEventStream({ enabled: true }), { wrapper: wrapper(queryClient) });

    const es = FakeEventSource.instances[0];
    es.readyState = 0;
    act(() => es.emit("error"));
    act(() => vi.advanceTimersByTime(60_000));
    expect(FakeEventSource.instances).toHaveLength(1);
    expect(es.closed).toBe(false);
  });

  it("caps the backoff at 30s", () => {
    expect(sseReconnectDelay(0)).toBe(1_000);
    expect(sseReconnectDelay(1)).toBe(2_000);
    expect(sseReconnectDelay(4)).toBe(16_000);
    expect(sseReconnectDelay(5)).toBe(SSE_RECONNECT_MAX_MS);
    expect(sseReconnectDelay(50)).toBe(SSE_RECONNECT_MAX_MS);
  });

  it("cancels a pending reconnect on unmount", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const { unmount } = renderHook(() => useEventStream({ enabled: true }), { wrapper: wrapper(queryClient) });

    const es = FakeEventSource.instances[0];
    es.readyState = 2;
    act(() => es.emit("error"));
    unmount();
    act(() => vi.advanceTimersByTime(60_000));
    expect(FakeEventSource.instances).toHaveLength(1);
  });
});
