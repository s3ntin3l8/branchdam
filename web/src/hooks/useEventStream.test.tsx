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
  //
  // #153: a background scan or graph-resolver pass changing a node's
  // graph_status -- not a manual confirm/reject/create action -- must also
  // refresh a mounted AssetDetailPage. "asset" (singular) is a different key
  // namespace from "assets" (plural) and prefix matching doesn't bridge them
  // (see the same note in queries.ts's invalidateEdgeReviewQueries), so the
  // detail query, lineage, and graph need their own entries here.
  //
  // "settings" was added once the Settings page (SettingsPage.tsx) started
  // consuming useSettings() -- a second browser session's PUT broadcasts
  // this same coarse nudge, since the hub has no per-topic channel.
  it("invalidates the graph_status-dependent asset queries on a progress nudge, alongside every other key", () => {
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const spy = vi.spyOn(queryClient, "invalidateQueries");

    renderHook(() => useEventStream(), { wrapper: wrapper(queryClient) });

    expect(FakeEventSource.instances).toHaveLength(1);
    FakeEventSource.instances[0].emit("progress");

    // Exact match, not arrayContaining -- this hook invalidates a fixed,
    // known set of keys on every nudge, so a future omission (e.g. dropping
    // one of ASSET_DETAIL_QUERY_KEYS by accident) should fail this test
    // instead of passing silently.
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
});
