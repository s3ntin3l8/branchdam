import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useEventStream } from "./useEventStream";

// jsdom doesn't implement EventSource -- a minimal fake is enough to drive
// the "progress", "error", and "open" listeners this hook registers.
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  private listeners: Record<string, Array<() => void>> = {};
  url: string;
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

  close() {}

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

    renderHook(() => useEventStream(), { wrapper: wrapper(queryClient) });

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
      ["settings"],
      ["asset"],
      ["asset-lineage"],
      ["asset-graph"],
    ]);
  });

  it("debounces rapid-fire progress nudges to avoid refetch churn (#357)", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const spy = vi.spyOn(queryClient, "invalidateQueries");

    renderHook(() => useEventStream(), { wrapper: wrapper(queryClient) });

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

    // Only 9 invalidation calls (one batch for the 9 query keys)
    expect(spy).toHaveBeenCalledTimes(9);
  });

  it("tracks disconnected state on error and clears it on open or progress (#349)", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    const { result } = renderHook(() => useEventStream(), { wrapper: wrapper(queryClient) });

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
});
