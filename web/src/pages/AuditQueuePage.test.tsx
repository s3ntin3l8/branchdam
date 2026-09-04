import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryRouter, RouterProvider } from "react-router";
import AuditQueuePage from "./AuditQueuePage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    me: vi.fn().mockResolvedValue({ kind: "user", name: "admin" }),
    listAuditQueue: vi.fn(),
    confirmEdge: vi.fn(),
    rejectEdge: vi.fn(),
    createEdge: vi.fn(),
    listAssets: vi.fn().mockResolvedValue({ assets: [], total: 0 }),
    thumbnailUrl: vi.fn((id: number) => `/api/v1/assets/${id}/thumbnail`),
  },
}));

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const router = createMemoryRouter(
    [{ path: "/", element: ui }],
    { initialEntries: ["/"] },
  );
  return render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("AuditQueuePage", () => {
  it("shows an empty state when there is nothing to review", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [], total: 0 });
    renderWithClient(<AuditQueuePage />);

    expect(await screen.findByText(/nothing needs review/i)).toBeInTheDocument();
  });

  it("renders queue entries with side-by-side nodes and flags a missing parent", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 1,
          sourceNodeId: 10,
          targetNodeId: 20,
          relationshipType: "PROXY_OF",
          confidence: 0.6,
          tier: 3,
          resolver: "filename_stem",
          evidenceJson: "{}",
          parentAlive: false,
          parentMissing: true,
          sourceNode: {
            id: 10,
            nodeUuid: "uuid-10",
            fileName: "RAW_001.ARW",
            filePath: "/storage/RAW_001.ARW",
            thumbState: "READY",
          },
          targetNode: {
            id: 20,
            nodeUuid: "uuid-20",
            fileName: "EXPORT_001.JPG",
            filePath: "/storage/EXPORT_001.JPG",
            thumbState: "READY",
          },
          captureDeltaSeconds: 5,
          phashDistance: 2,
        },
      ],
      total: 1,
    });
    renderWithClient(<AuditQueuePage />);

    expect(await screen.findByText("PROXY_OF")).toBeInTheDocument();
    expect(screen.getByText(/parent missing/i)).toBeInTheDocument();
    expect(screen.getByText("RAW_001.ARW")).toBeInTheDocument();
    expect(screen.getByText("EXPORT_001.JPG")).toBeInTheDocument();
    expect(screen.getByText("5s")).toBeInTheDocument();
    expect(screen.getByText("2 bits")).toBeInTheDocument();
  });

  it("calls confirmEdge when Confirm is clicked", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 42,
          sourceNodeId: 1,
          targetNodeId: 2,
          relationshipType: "DERIVED_FROM",
          confidence: 0.75,
          tier: 2,
          resolver: "filename_stem",
          evidenceJson: "{}",
          parentAlive: true,
          parentMissing: false,
          sourceNode: { id: 1, nodeUuid: "uuid-1", fileName: "src.arw", filePath: "/src.arw", thumbState: "READY" },
          targetNode: { id: 2, nodeUuid: "uuid-2", fileName: "tgt.jpg", filePath: "/tgt.jpg", thumbState: "READY" },
        },
      ],
      total: 1,
    });
    vi.mocked(api.confirmEdge).mockResolvedValue({ ok: true });

    renderWithClient(<AuditQueuePage />);
    const button = await screen.findByRole("button", { name: /confirm/i });
    await userEvent.click(button);

    await waitFor(() => expect(api.confirmEdge).toHaveBeenCalledWith(42));
  });

  it("opens manual link modal and submits createEdge", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [], total: 0 });
    vi.mocked(api.createEdge).mockResolvedValue({
      id: 99,
      sourceNodeId: 100,
      targetNodeId: 200,
      relationshipType: "DERIVED_FROM",
      confidence: 1.0,
      reviewState: "CONFIRMED",
      resolver: "manual",
    });

    renderWithClient(<AuditQueuePage />);
    const manualBtn = await screen.findByRole("button", { name: /\+ manual link edge/i });
    await userEvent.click(manualBtn);

    expect(screen.getByText("Manual Link Edge")).toBeInTheDocument();

    const inputs = screen.getAllByRole("spinbutton");
    await userEvent.type(inputs[0], "100");
    await userEvent.type(inputs[1], "200");

    const submitBtn = screen.getByRole("button", { name: /create link/i });
    await userEvent.click(submitBtn);

    await waitFor(() =>
      expect(api.createEdge).toHaveBeenCalledWith({
        sourceNodeId: 100,
        targetNodeId: 200,
        relationshipType: "DERIVED_FROM",
      })
    );
  });

  it("surfaces error message when confirmEdge fails", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 42,
          sourceNodeId: 1,
          targetNodeId: 2,
          relationshipType: "DERIVED_FROM",
          confidence: 0.75,
          tier: 2,
          resolver: "filename_stem",
          evidenceJson: "{}",
          parentAlive: true,
          parentMissing: false,
          sourceNode: { id: 1, nodeUuid: "uuid-1", fileName: "s.arw", filePath: "/s.arw", thumbState: "READY" },
          targetNode: { id: 2, nodeUuid: "uuid-2", fileName: "t.jpg", filePath: "/t.jpg", thumbState: "READY" },
        },
      ],
      total: 1,
    });
    vi.mocked(api.confirmEdge).mockRejectedValue(new Error("Database write failure"));

    renderWithClient(<AuditQueuePage />);
    const button = await screen.findByRole("button", { name: /confirm/i });
    await userEvent.click(button);

    expect(await screen.findByText(/Failed to confirm edge: Error: Database write failure/i)).toBeInTheDocument();
  });

  it("surfaces error message when rejectEdge fails", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 43,
          sourceNodeId: 1,
          targetNodeId: 3,
          relationshipType: "DERIVED_FROM",
          confidence: 0.75,
          tier: 2,
          resolver: "filename_stem",
          evidenceJson: "{}",
          parentAlive: true,
          parentMissing: false,
          sourceNode: { id: 1, nodeUuid: "uuid-1", fileName: "s.arw", filePath: "/s.arw", thumbState: "READY" },
          targetNode: { id: 3, nodeUuid: "uuid-3", fileName: "t.jpg", filePath: "/t.jpg", thumbState: "READY" },
        },
      ],
      total: 1,
    });
    vi.mocked(api.rejectEdge).mockRejectedValue(new Error("Network disconnect"));

    renderWithClient(<AuditQueuePage />);
    const button = await screen.findByRole("button", { name: /reject/i });
    await userEvent.click(button);

    expect(await screen.findByText(/Failed to reject edge: Error: Network disconnect/i)).toBeInTheDocument();
  });

  it("uses keyset pagination: first page beforeId=0", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 100, sourceNodeId: 1, targetNodeId: 2, relationshipType: "DERIVED_FROM",
          confidence: 0.9, tier: 2, resolver: "x", evidenceJson: "{}",
          parentAlive: true, parentMissing: false,
          sourceNode: { id: 1, nodeUuid: "u-1", fileName: "s.arw", filePath: "/s.arw", thumbState: "READY" },
          targetNode: { id: 2, nodeUuid: "u-2", fileName: "t.jpg", filePath: "/t.jpg", thumbState: "READY" },
        },
        {
          id: 101, sourceNodeId: 3, targetNodeId: 4, relationshipType: "DERIVED_FROM",
          confidence: 0.85, tier: 2, resolver: "x", evidenceJson: "{}",
          parentAlive: true, parentMissing: false,
          sourceNode: { id: 3, nodeUuid: "u-3", fileName: "s2.arw", filePath: "/s2.arw", thumbState: "READY" },
          targetNode: { id: 4, nodeUuid: "u-4", fileName: "t2.jpg", filePath: "/t2.jpg", thumbState: "READY" },
        },
      ],
      total: 5, // more than the page size, so Next is enabled
    });

    renderWithClient(<AuditQueuePage />);
    await screen.findByText("s.arw"); // wait for first page to render

    // First page: no cursor, beforeId=0.
    expect(api.listAuditQueue).toHaveBeenLastCalledWith(
      expect.objectContaining({ limit: 50, beforeId: 0 }),
    );
  });

  it("Next button is disabled when entries.length < PAGE_SIZE (last page)", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 200, sourceNodeId: 1, targetNodeId: 2, relationshipType: "DERIVED_FROM",
          confidence: 0.9, tier: 2, resolver: "x", evidenceJson: "{}",
          parentAlive: true, parentMissing: false,
          sourceNode: { id: 1, nodeUuid: "u-1", fileName: "s.arw", filePath: "/s.arw", thumbState: "READY" },
          targetNode: { id: 2, nodeUuid: "u-2", fileName: "t.jpg", filePath: "/t.jpg", thumbState: "READY" },
        },
      ],
      total: 1,
    });

    renderWithClient(<AuditQueuePage />);
    const nextBtn = await screen.findByRole("button", { name: "Next page" });
    expect(nextBtn).toBeDisabled();
  });

  it("renders pagination buttons with aria-label attributes", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 200, sourceNodeId: 1, targetNodeId: 2, relationshipType: "DERIVED_FROM",
          confidence: 0.9, tier: 2, resolver: "x", evidenceJson: "{}",
          parentAlive: true, parentMissing: false,
          sourceNode: { id: 1, nodeUuid: "u-1", fileName: "s.arw", filePath: "/s.arw", thumbState: "READY" },
          targetNode: { id: 2, nodeUuid: "u-2", fileName: "t.jpg", filePath: "/t.jpg", thumbState: "READY" },
        },
      ],
      total: 1,
    });

    renderWithClient(<AuditQueuePage />);
    expect(await screen.findByRole("button", { name: "First page" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Next page" })).toBeInTheDocument();
  });

  it("restores focus to trigger button after closing manual link modal", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [], total: 0 });

    renderWithClient(<AuditQueuePage />);
    const openBtn = await screen.findByRole("button", { name: "+ Manual Link Edge" });
    openBtn.focus();
    expect(document.activeElement).toBe(openBtn);

    await user.click(openBtn);
    expect(await screen.findByRole("dialog")).toBeInTheDocument();

    const cancelBtn = screen.getByRole("button", { name: "Cancel" });
    await user.click(cancelBtn);

    await waitFor(() => {
      expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
      expect(document.activeElement).toBe(openBtn);
    });
  });

  it("opens node picker for target node, selects an asset, and submits createEdge", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [], total: 0 });
    vi.mocked(api.listAssets).mockResolvedValue({
      assets: [
        {
          id: 55,
          nodeUuid: "uuid-target-55",
          filePath: "/exports/final_render.jpg",
          fileName: "final_render.jpg",
          fileExt: ".jpg",
          sizeBytes: 4000000,
          indexingStatus: "INDEXED_FULL",
          graphStatus: "UNLINKED",
          lifecycleState: "ACTIVE",
          storageLocationId: 1,
          cameraModel: "Sony A7IV",
          thumbState: "READY",
        },
      ],
      total: 1,
    });
    vi.mocked(api.createEdge).mockResolvedValue({
      id: 101,
      sourceNodeId: 42,
      targetNodeId: 55,
      relationshipType: "FINAL_EXPORT",
      confidence: 1.0,
      reviewState: "CONFIRMED",
      resolver: "manual",
    });

    renderWithClient(<AuditQueuePage />);
    const manualBtn = await screen.findByRole("button", { name: /\+ manual link edge/i });
    await user.click(manualBtn);

    const inputs = screen.getAllByRole("spinbutton");
    await user.type(inputs[0], "42");

    // Click "Pick Node" for target node
    const pickTargetBtn = screen.getByRole("button", { name: "Select Target Asset" });
    await user.click(pickTargetBtn);

    // NodePickerModal opens
    expect(await screen.findByRole("dialog", { name: "Select Child (Target) Node" })).toBeInTheDocument();
    expect(screen.getByText("final_render.jpg")).toBeInTheDocument();

    // Select the asset
    const assetCard = screen.getByText("final_render.jpg");
    await user.click(assetCard);

    // NodePickerModal closes and target input is populated
    await waitFor(() => {
      expect(screen.queryByRole("dialog", { name: "Select Child (Target) Node" })).not.toBeInTheDocument();
    });
    expect(inputs[1]).toHaveValue(55);

    // Select relationship type and submit
    const relSelect = screen.getByRole("combobox");
    await user.selectOptions(relSelect, "FINAL_EXPORT");

    const submitBtn = screen.getByRole("button", { name: /create link/i });
    await user.click(submitBtn);

    await waitFor(() =>
      expect(api.createEdge).toHaveBeenCalledWith({
        sourceNodeId: 42,
        targetNodeId: 55,
        relationshipType: "FINAL_EXPORT",
      })
    );
  });
});
