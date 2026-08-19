import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { describe, expect, it, vi } from "vitest";
import AssetDetailPage from "./AssetDetailPage";
import { api } from "../api/client";
import type { Asset } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    getAsset: vi.fn(),
    getAssetLineage: vi.fn(),
    pruneCache: vi.fn(),
  },
}));

const asset: Asset = {
  id: 42,
  nodeUuid: "uuid-42",
  filePath: "/mnt/scratch/proxy.jpg",
  fileName: "proxy.jpg",
  fileExt: "jpg",
  sizeBytes: 12345,
  indexingStatus: "INDEXED_SHALLOW",
  graphStatus: "UNLINKED",
  lifecycleState: "ACTIVE",
  storageLocationId: 7,
};

function renderWithClient() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/assets/42"]}>
        <Routes>
          <Route path="/assets/:id" element={<AssetDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("AssetDetailPage purge control", () => {
  it("checks eligibility then executes on confirm", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.pruneCache).mockResolvedValueOnce({
      executed: false,
      candidates: [{ nodeId: 42, filePath: asset.filePath, sizeBytes: asset.sizeBytes, purged: false }],
    });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: /purge cache/i }));
    await waitFor(() =>
      expect(api.pruneCache).toHaveBeenCalledWith({ storageLocationId: 7, nodeIds: [42] }),
    );
    await waitFor(() => expect(screen.getByRole("button", { name: /confirm purge/i })).toBeInTheDocument());

    vi.mocked(api.pruneCache).mockResolvedValueOnce({
      executed: true,
      candidates: [{ nodeId: 42, filePath: asset.filePath, sizeBytes: asset.sizeBytes, purged: true }],
    });
    await userEvent.click(screen.getByRole("button", { name: /confirm purge/i }));
    await waitFor(() =>
      expect(api.pruneCache).toHaveBeenCalledWith({ storageLocationId: 7, nodeIds: [42], execute: true }),
    );
    await waitFor(() => expect(screen.getByText("Cache purged.")).toBeInTheDocument());
  });

  it("reports ineligible when the plan returns no candidates", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.pruneCache).mockResolvedValueOnce({ executed: false, candidates: [] });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: /purge cache/i }));
    await waitFor(() => expect(screen.getByText(/not eligible for pruning/i)).toBeInTheDocument());
  });
});
