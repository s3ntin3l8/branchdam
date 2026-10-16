import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryRouter, RouterProvider } from "react-router";
import AuditQueuePage from "./AuditQueuePage";
import { api } from "../api/client";
import type { AuditQueueParams, EdgeAuditEntry } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    me: vi.fn().mockResolvedValue({ kind: "user", name: "admin", isAdmin: true }),
    listAuditQueue: vi.fn(),
    confirmEdge: vi.fn(),
    rejectEdge: vi.fn(),
    resetEdgeReview: vi.fn(),
    createEdge: vi.fn(),
    listAssets: vi.fn().mockResolvedValue({ assets: [], total: 0 }),
    thumbnailUrl: vi.fn((id: number) => `/api/v1/assets/${id}/thumbnail`),
  },
}));

function renderPage(path = "/audit") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const router = createMemoryRouter(
    [
      { path: "/audit", element: <AuditQueuePage /> },
      { path: "/assets/:id", element: <div>asset page</div> },
    ],
    { initialEntries: [path] }
  );
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
  return router;
}

function entry(id: number, overrides: Partial<EdgeAuditEntry> = {}): EdgeAuditEntry {
  return {
    id,
    sourceNodeId: id * 10,
    targetNodeId: id * 10 + 1,
    relationshipType: "DERIVED_FROM",
    confidence: 0.7,
    tier: 2,
    resolver: "filename_stem",
    evidenceJson: "{}",
    parentAlive: true,
    parentMissing: false,
    sourceNode: { id: id * 10, nodeUuid: `u-${id}-s`, fileName: `src-${id}.arw`, filePath: `/s/src-${id}.arw`, thumbState: "READY" },
    targetNode: { id: id * 10 + 1, nodeUuid: `u-${id}-t`, fileName: `tgt-${id}.jpg`, filePath: `/t/tgt-${id}.jpg`, thumbState: "READY" },
    ...overrides,
  };
}

function lastQueueParams(): AuditQueueParams {
  const calls = vi.mocked(api.listAuditQueue).mock.calls;
  return calls[calls.length - 1][0] as AuditQueueParams;
}

beforeEach(() => {
  vi.mocked(api.listAuditQueue).mockReset();
  vi.mocked(api.rejectEdge).mockReset();
  vi.mocked(api.confirmEdge).mockReset();
  vi.mocked(api.resetEdgeReview).mockReset();
});

describe("AuditQueuePage reversible review", () => {
  it("does not reject until the confirm step, and Cancel leaves the edge alone", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [entry(43)], total: 1 });
    vi.mocked(api.rejectEdge).mockResolvedValue({ ok: true });
    renderPage();

    await user.click(await screen.findByRole("button", { name: /^reject$/i }));
    const dialog = await screen.findByRole("dialog", { name: /reject edge #43/i });
    expect(api.rejectEdge).not.toHaveBeenCalled();

    await user.click(within(dialog).getByRole("button", { name: "Cancel" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.rejectEdge).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /^reject$/i }));
    await user.click(await screen.findByRole("button", { name: "Reject Edge" }));
    await waitFor(() => expect(api.rejectEdge).toHaveBeenCalledWith(43));
  });

  it("offers Undo after a reject, which resets the edge's review", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [entry(43)], total: 1 });
    vi.mocked(api.rejectEdge).mockResolvedValue({ ok: true });
    vi.mocked(api.resetEdgeReview).mockResolvedValue({ ok: true, previousState: "REJECTED" });
    renderPage();

    await user.click(await screen.findByRole("button", { name: /^reject$/i }));
    await user.click(await screen.findByRole("button", { name: "Reject Edge" }));

    const recent = await screen.findByRole("region", { name: "Recently reviewed" });
    expect(within(recent).getByText(/rejected/i)).toBeInTheDocument();
    await user.click(within(recent).getByRole("button", { name: "Undo review of edge #43" }));

    await waitFor(() => expect(api.resetEdgeReview).toHaveBeenCalledWith(43));
    await waitFor(() => expect(screen.queryByRole("region", { name: "Recently reviewed" })).not.toBeInTheDocument());
  });

  it("offers no Undo to a reviewer who is not an admin", async () => {
    const user = userEvent.setup();
    vi.mocked(api.me).mockResolvedValueOnce({ kind: "user", name: "bob", isAdmin: false } as Awaited<ReturnType<typeof api.me>>);
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [entry(43)], total: 1 });
    vi.mocked(api.rejectEdge).mockResolvedValue({ ok: true });
    renderPage();

    await user.click(await screen.findByRole("button", { name: /^reject$/i }));
    await user.click(await screen.findByRole("button", { name: "Reject Edge" }));
    await waitFor(() => expect(api.rejectEdge).toHaveBeenCalledWith(43));

    expect(screen.queryByRole("region", { name: "Recently reviewed" })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /undo/i })).not.toBeInTheDocument();
  });

  it("offers Undo after a confirm too", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [entry(7)], total: 1 });
    vi.mocked(api.confirmEdge).mockResolvedValue({ ok: true });
    vi.mocked(api.resetEdgeReview).mockResolvedValue({ ok: true, previousState: "CONFIRMED" });
    renderPage();

    await user.click(await screen.findByRole("button", { name: /^confirm$/i }));
    const recent = await screen.findByRole("region", { name: "Recently reviewed" });
    await user.click(within(recent).getByRole("button", { name: "Undo review of edge #7" }));
    await waitFor(() => expect(api.resetEdgeReview).toHaveBeenCalledWith(7));
  });

  it("shows why an undo failed (e.g. a non-admin)", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [entry(7)], total: 1 });
    vi.mocked(api.confirmEdge).mockResolvedValue({ ok: true });
    vi.mocked(api.resetEdgeReview).mockRejectedValue(new Error("admin authorization required"));
    renderPage();

    await user.click(await screen.findByRole("button", { name: /^confirm$/i }));
    const recent = await screen.findByRole("region", { name: "Recently reviewed" });
    await user.click(within(recent).getByRole("button", { name: "Undo review of edge #7" }));
    expect(await within(recent).findByText(/undo failed: .*admin authorization required/i)).toBeInTheDocument();
  });
});

describe("AuditQueuePage server-side filters and paging", () => {
  it("sends filters to the server and resets to the first page", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [entry(1)],
      total: 1,
      resolvers: ["filename_stem", "phash_similarity"],
    });
    renderPage("/audit?trail=99");
    await screen.findByText("src-1.arw");
    expect(lastQueueParams()).toMatchObject({ beforeId: 99 });

    // Resolver options come from the whole queue, not the loaded page.
    expect(screen.getByRole("option", { name: "phash_similarity" })).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText("Tier"), "3");
    await waitFor(() => expect(lastQueueParams()).toMatchObject({ tier: 3, beforeId: 0 }));
    await user.selectOptions(screen.getByLabelText("Resolver"), "phash_similarity");
    await waitFor(() => expect(lastQueueParams()).toMatchObject({ tier: 3, resolver: "phash_similarity" }));
    await user.selectOptions(screen.getByLabelText("Relationship"), "DUPLICATE_OF");
    await user.selectOptions(screen.getByLabelText("Min confidence"), "0.8");
    await waitFor(() =>
      expect(lastQueueParams()).toMatchObject({
        tier: 3,
        resolver: "phash_similarity",
        relationshipType: "DUPLICATE_OF",
        minConfidence: 0.8,
        beforeId: 0,
      })
    );
  });

  it("keeps the filter panel when the filtered result is empty", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [], total: 0, resolvers: ["x"] });
    renderPage("/audit?tier=3");
    expect(await screen.findByText(/no edge candidates match the selected filters/i)).toBeInTheDocument();
    expect(screen.getByLabelText("Tier")).toHaveValue("3");
    expect(screen.getByRole("button", { name: "Clear Filters" })).toBeInTheDocument();
  });

  it("drops an unknown relationshipType from the URL instead of sending it", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [entry(1)], total: 1, resolvers: [] });
    renderPage("/audit?relationshipType=BOGUS");
    await screen.findByText("src-1.arw");
    expect(lastQueueParams().relationshipType).toBeUndefined();
    expect(screen.getByLabelText("Relationship")).toHaveValue("");
  });

  it("drops out-of-range tier, minConfidence and resolver from the URL", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [entry(1)], total: 1, resolvers: [] });
    renderPage(`/audit?tier=5&minConfidence=1.5&resolver=${"r".repeat(129)}`);
    await screen.findByText("src-1.arw");
    const p = lastQueueParams();
    expect(p.tier).toBeUndefined();
    expect(p.minConfidence).toBeUndefined();
    expect(p.resolver).toBeUndefined();
  });

  it("disables paging while the next page loads, so a double click can't skip one", async () => {
    const user = userEvent.setup();
    Element.prototype.scrollIntoView = vi.fn();
    const page1 = Array.from({ length: 50 }, (_, i) => entry(i + 1));
    let release: () => void = () => {};
    vi.mocked(api.listAuditQueue).mockImplementation(async (p: AuditQueueParams = {}) => {
      if (!p.beforeId) return { entries: page1, total: 150 };
      await new Promise<void>((r) => (release = r));
      return { entries: [entry(101)], total: 150 };
    });
    renderPage();
    await screen.findByText("src-1.arw");

    await user.click(screen.getByRole("button", { name: "Next page" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "Next page" })).toBeDisabled());
    expect(screen.getByRole("button", { name: "Previous page" })).toBeDisabled();
    release();
    await screen.findByText("src-101.arw");
    expect(vi.mocked(api.listAuditQueue).mock.calls.filter(([p]) => p?.beforeId === 50)).toHaveLength(1);
  });

  it("pages forward and back with Previous, and scrolls the page to the top", async () => {
    const user = userEvent.setup();
    const scrollSpy = vi.fn();
    Element.prototype.scrollIntoView = scrollSpy;
    const page1 = Array.from({ length: 50 }, (_, i) => entry(i + 1));
    const page2 = Array.from({ length: 50 }, (_, i) => entry(i + 101));
    vi.mocked(api.listAuditQueue).mockImplementation(async (p: AuditQueueParams = {}) => {
      if (!p.beforeId) return { entries: page1, total: 150 };
      if (p.beforeId === 50) return { entries: page2, total: 150 };
      return { entries: [entry(201)], total: 150 };
    });
    renderPage();

    await screen.findByText("src-1.arw");
    expect(screen.getByRole("button", { name: "Previous page" })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "Next page" }));
    await screen.findByText("src-101.arw");
    expect(scrollSpy).toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "Next page" }));
    await screen.findByText("src-201.arw");
    expect(lastQueueParams()).toMatchObject({ beforeId: 150 });

    // Previous goes back ONE page (to the page-2 cursor), not to the start.
    await user.click(screen.getByRole("button", { name: "Previous page" }));
    await screen.findByText("src-101.arw");
    expect(lastQueueParams()).toMatchObject({ beforeId: 50 });

    await user.click(screen.getByRole("button", { name: "Previous page" }));
    await screen.findByText("src-1.arw");
    expect(lastQueueParams()).toMatchObject({ beforeId: 0 });
  }, 20_000);
});

describe("AuditQueuePage cards", () => {
  afterEach(() => {
    vi.mocked(api.listAuditQueue).mockReset();
  });

  it("links both nodes to their asset pages", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [entry(4)], total: 1 });
    const router = renderPage();

    const parentLink = await screen.findByRole("link", { name: "src-4.arw" });
    expect(parentLink).toHaveAttribute("href", "/assets/40");
    expect(screen.getByRole("link", { name: "tgt-4.jpg" })).toHaveAttribute("href", "/assets/41");

    await user.click(parentLink);
    await waitFor(() => expect(router.state.location.pathname).toBe("/assets/40"));
  });
});

describe("AuditQueuePage batch selection", () => {
  it("selects every matching edge across pages and batches over exactly that selection", async () => {
    const user = userEvent.setup();
    const all = Array.from({ length: 70 }, (_, i) => entry(i + 1, { tier: 3 }));
    vi.mocked(api.listAuditQueue).mockImplementation(async (p: AuditQueueParams = {}) => {
      const start = p.beforeId ? all.findIndex((e) => e.id === p.beforeId) + 1 : 0;
      return { entries: all.slice(start, start + (p.limit ?? 50)), total: all.length };
    });
    vi.mocked(api.rejectEdge).mockResolvedValue({ ok: true });
    renderPage("/audit?tier=3");

    await screen.findByText("src-1.arw");
    await user.click(screen.getByRole("button", { name: "Select all 70 matching" }));
    expect(await screen.findByText(/70 selected \(filtered\)/)).toBeInTheDocument();
    // The select-all fetch carries the active filter.
    expect(vi.mocked(api.listAuditQueue).mock.calls.some(([p]) => p?.limit === 500 && p?.tier === 3)).toBe(true);

    await user.click(screen.getByRole("button", { name: "Reject selected (70)" }));
    const dialog = await screen.findByRole("dialog", { name: /batch reject edges/i });
    expect(within(dialog).getByText(/70/)).toBeInTheDocument();
    expect(api.rejectEdge).not.toHaveBeenCalled();
    await user.click(within(dialog).getByRole("button", { name: "Reject Selected" }));

    await waitFor(() => expect(api.rejectEdge).toHaveBeenCalledTimes(70));
    expect(api.rejectEdge).toHaveBeenCalledWith(70);
    // Every batch decision is undoable, singly or all at once.
    expect(await screen.findByRole("button", { name: "Undo review of edge #70" })).toBeInTheDocument();
    vi.mocked(api.resetEdgeReview).mockResolvedValue({ ok: true, previousState: "REJECTED" });
    await user.click(screen.getByRole("button", { name: "Undo all (70)" }));
    await waitFor(() => expect(api.resetEdgeReview).toHaveBeenCalledTimes(70));
    await waitFor(() => expect(screen.queryByRole("region", { name: "Recently reviewed" })).not.toBeInTheDocument());
  }, 30_000);

  it("clears the selection when the filters change", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [entry(1), entry(2)], total: 2 });
    renderPage();

    await user.click(await screen.findByRole("checkbox", { name: "Select edge #1" }));
    expect(screen.getByText(/1 selected/)).toBeInTheDocument();

    await user.selectOptions(screen.getByLabelText("Tier"), "2");
    await waitFor(() => expect(screen.queryByText(/1 selected/)).not.toBeInTheDocument());
  });
});
