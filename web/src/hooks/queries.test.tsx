import { describe, expect, it, vi } from "vitest";
import { renderHook, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import type { ReactNode } from "react";
import { useConfirmEdge, useCreateEdge, useRejectEdge } from "./queries";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    confirmEdge: vi.fn(),
    rejectEdge: vi.fn(),
    createEdge: vi.fn(),
  },
}));

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
