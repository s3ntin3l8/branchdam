import { describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import {
  useConfirmEdge,
  useCreateEdge,
  useRejectEdge,
  useTrashAsset,
  useUnlinkedCount,
  useUploadFile,
  useUsers,
} from "./queries";
import { ApiError, api } from "../api/client";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ApiError: actual.ApiError,
    api: {
      confirmEdge: vi.fn(),
      rejectEdge: vi.fn(),
      createEdge: vi.fn(),
      listAssets: vi.fn(),
      listUsers: vi.fn(),
      trashAsset: vi.fn(),
      uploadFile: vi.fn(),
    },
  };
});

// L3: confirm/reject/manual-create all change an edge's review_state, which
// the backend recomputes graph_status from (internal/httpapi/routes.go's
// recomputeGraphStatus) -- every query whose data depends on graph_status
// must be invalidated on success, not just the audit queue. Asserted
// directly against invalidateQueries' calls rather than re-fetch behavior,
// so a future edit that silently drops one of these keys fails loudly here
// instead of only showing up as a stale UI a user has to notice by eye.
const EXPECTED_EDGE_REVIEW_KEYS = [
  ["audit-queue"],
  ["assets"],
  ["asset"],
  ["asset-lineage"],
  ["asset-graph"],
  ["unlinked-count"],
];

function wrapper(queryClient: QueryClient) {
  return function Wrapper({ children }: { children: ReactNode }) {
    return <QueryClientProvider client={queryClient}>{children}</QueryClientProvider>;
  };
}

function invalidatedKeys(queryClient: QueryClient) {
  return vi
    .mocked(queryClient.invalidateQueries)
    .mock.calls.map(([filters]) => filters?.queryKey);
}

describe("edge review mutations invalidate every graph_status-dependent query", () => {
  it("useConfirmEdge", async () => {
    vi.mocked(api.confirmEdge).mockResolvedValue({ ok: true });
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.spyOn(queryClient, "invalidateQueries");

    const { result } = renderHook(() => useConfirmEdge(), { wrapper: wrapper(queryClient) });
    result.current.mutate(1);

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(invalidatedKeys(queryClient)).toEqual(expect.arrayContaining(EXPECTED_EDGE_REVIEW_KEYS));
  });

  it("useRejectEdge", async () => {
    vi.mocked(api.rejectEdge).mockResolvedValue({ ok: true });
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.spyOn(queryClient, "invalidateQueries");

    const { result } = renderHook(() => useRejectEdge(), { wrapper: wrapper(queryClient) });
    result.current.mutate(1);

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(invalidatedKeys(queryClient)).toEqual(expect.arrayContaining(EXPECTED_EDGE_REVIEW_KEYS));
  });

  it("useCreateEdge", async () => {
    vi.mocked(api.createEdge).mockResolvedValue({
      id: 1,
      sourceNodeId: 1,
      targetNodeId: 2,
      relationshipType: "DERIVED_FROM",
      confidence: 1,
      reviewState: "CONFIRMED",
      resolver: "manual",
    });
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.spyOn(queryClient, "invalidateQueries");

    const { result } = renderHook(() => useCreateEdge(), { wrapper: wrapper(queryClient) });
    result.current.mutate({ sourceNodeId: 1, targetNodeId: 2, relationshipType: "DERIVED_FROM" });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(invalidatedKeys(queryClient)).toEqual(expect.arrayContaining(EXPECTED_EDGE_REVIEW_KEYS));
  });
});

describe("useUnlinkedCount", () => {
  it("fetches server-side unlinked count via listAssets with unlinkedOnly: true and limit: 1", async () => {
    vi.mocked(api.listAssets).mockResolvedValue({ assets: [], total: 42 });
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    const { result } = renderHook(() => useUnlinkedCount(), { wrapper: wrapper(queryClient) });

    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    expect(result.current.data).toBe(42);
    expect(api.listAssets).toHaveBeenCalledWith({ unlinkedOnly: true, limit: 1 });
  });
});

describe("useUsers", () => {
  it("does not fetch the admin-only /users endpoint when disabled", async () => {
    vi.mocked(api.listUsers).mockClear();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    renderHook(() => useUsers({}, { enabled: false }), { wrapper: wrapper(queryClient) });
    await Promise.resolve();
    expect(api.listUsers).not.toHaveBeenCalled();
  });

  it("does not retry a 403 (retry is decided by status, not message text)", async () => {
    vi.mocked(api.listUsers).mockReset();
    vi.mocked(api.listUsers).mockRejectedValue(new ApiError(403, "admin authorization required"));
    // Use the hook's own retry policy: only the delay is overridden.
    const queryClient = new QueryClient({ defaultOptions: { queries: { retryDelay: 1 } } });
    const { result } = renderHook(() => useUsers(), { wrapper: wrapper(queryClient) });
    await waitFor(() => expect(result.current.isError).toBe(true));
    expect(api.listUsers).toHaveBeenCalledTimes(1);
  });
});

describe("trashing an asset refreshes everything a lifecycle change touches", () => {
  it("invalidates lineage, graph, unlinked badge, facets and storage health", async () => {
    vi.mocked(api.trashAsset).mockResolvedValue({ ok: true } as never);
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    vi.spyOn(queryClient, "invalidateQueries");
    const { result } = renderHook(() => useTrashAsset(), { wrapper: wrapper(queryClient) });
    result.current.mutate({ id: 7, keepExports: false });
    await waitFor(() => expect(result.current.isSuccess).toBe(true));
    const keys = invalidatedKeys(queryClient).map((k) => JSON.stringify(k));
    for (const k of [["assets"], ["asset", 7], ["asset-lineage"], ["asset-graph"], ["unlinked-count"], ["asset-facets"], ["storage-health"]]) {
      expect(keys).toContain(JSON.stringify(k));
    }
  });
});

describe("useUploadFile", () => {
  it("coalesces the post-upload refresh for a burst of files into one", async () => {
    vi.useFakeTimers();
    try {
      vi.mocked(api.uploadFile).mockResolvedValue({ status: "UPLOADED" } as never);
      const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
      vi.spyOn(queryClient, "invalidateQueries");
      const { result } = renderHook(() => useUploadFile(), { wrapper: wrapper(queryClient) });
      for (let i = 0; i < 5; i++) {
        await result.current.mutateAsync({ file: new File(["x"], `f${i}.jpg`) });
      }
      expect(queryClient.invalidateQueries).not.toHaveBeenCalled();
      await vi.advanceTimersByTimeAsync(1000);
      expect(invalidatedKeys(queryClient)).toEqual([["assets"], ["unlinked-count"], ["storage-health"]]);
    } finally {
      vi.useRealTimers();
    }
  });
});
