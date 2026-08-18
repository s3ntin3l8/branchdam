import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { renderHook } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useEventStream } from "./useEventStream";

// jsdom doesn't implement EventSource -- a minimal fake is enough to drive
// the "progress" listener this hook registers, mirroring the ResizeObserver
// stub pattern in test/setup.ts.
class FakeEventSource {
  static instances: FakeEventSource[] = [];
  private listeners: Record<string, Array<() => void>> = {};
  url: string;

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
    FakeEventSource.instances = [];
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    globalThis.EventSource = FakeEventSource as any;
  });

  afterEach(() => {
    globalThis.EventSource = originalEventSource;
  });

  // L4: the ingest jobs page (#52) and storage-health page (#51) were both
  // added after this hook and never wired into its "progress" nudge --
  // they previously only updated on their own poll interval instead of
  // reacting live like assets/audit-queue/progress already did.
  it("invalidates jobs and storage-health on a progress nudge, alongside progress/assets/audit-queue", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const spy = vi.spyOn(queryClient, "invalidateQueries");

    renderHook(() => useEventStream(), { wrapper: wrapper(queryClient) });

    expect(FakeEventSource.instances).toHaveLength(1);
    FakeEventSource.instances[0].emit("progress");

    const keys = spy.mock.calls.map(([filters]) => filters?.queryKey);
    expect(keys).toEqual(
      expect.arrayContaining([
        ["progress"],
        ["assets"],
        ["audit-queue"],
        ["jobs"],
        ["storage-health"],
      ]),
    );
  });
});
