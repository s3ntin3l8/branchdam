import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes } from "react-router";
import { describe, expect, it, vi } from "vitest";
import AssetDetailPage from "./AssetDetailPage";
import { api } from "../api/client";
import type { Asset, StorageLocation } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    getAsset: vi.fn(),
    getAssetLineage: vi.fn(),
    getAssetSyncStatus: vi.fn().mockResolvedValue({ sync: [] }),
    retrySync: vi.fn(),
    pruneCache: vi.fn(),
    listStorageLocations: vi.fn(),
    inheritMetadata: vi.fn(),
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

const prunableLocation: StorageLocation = {
  id: 7,
  name: "scratch",
  rootPath: "/mnt/scratch",
  tier: "TIER1_LOCAL_SCRATCH",
  readOnly: false,
  prunable: true,
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
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.pruneCache).mockResolvedValueOnce({
      executed: false,
      candidates: [{ nodeId: 42, filePath: asset.filePath, sizeBytes: asset.sizeBytes, purged: false }],
    });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    const purgeButton = await screen.findByRole("button", { name: /purge cache/i });
    await userEvent.click(purgeButton);
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
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.pruneCache).mockResolvedValueOnce({ executed: false, candidates: [] });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    const purgeButton = await screen.findByRole("button", { name: /purge cache/i });
    await userEvent.click(purgeButton);
    await waitFor(() => expect(screen.getByText(/not eligible for pruning/i)).toBeInTheDocument());
  });

  // Execute re-verifies eligibility right before deleting (TOCTOU fix), so
  // a 200 response can still report purged=false for this asset's own
  // candidate -- e.g. the Tier-3 master went MISSING between the dry-run
  // and this click. The UI must key its message off that per-candidate
  // outcome, not off the request having merely succeeded.
  it("shows a failure message when the confirmed purge reports purged=false", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.pruneCache).mockResolvedValueOnce({
      executed: false,
      candidates: [{ nodeId: 42, filePath: asset.filePath, sizeBytes: asset.sizeBytes, purged: false }],
    });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    const purgeButton = await screen.findByRole("button", { name: /purge cache/i });
    await userEvent.click(purgeButton);
    await waitFor(() => expect(screen.getByRole("button", { name: /confirm purge/i })).toBeInTheDocument());

    vi.mocked(api.pruneCache).mockResolvedValueOnce({
      executed: true,
      candidates: [{
        nodeId: 42, filePath: asset.filePath, sizeBytes: asset.sizeBytes, purged: false,
        error: "node is no longer eligible for pruning: verified Tier-3 ancestor lost since Plan",
      }],
    });
    await userEvent.click(screen.getByRole("button", { name: /confirm purge/i }));

    await waitFor(() => expect(screen.getByText(/purge failed/i)).toBeInTheDocument());
    expect(screen.queryByText("Cache purged.")).not.toBeInTheDocument();
  });
});

describe("AssetDetailPage inherit-metadata control", () => {
  it("reports how many tags were inherited, excluding the always-emitted identity tags", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.inheritMetadata).mockResolvedValueOnce({
      inherited: {
        "EXIF:Make": "SONY",
        "EXIF:LensModel": "FE 24-70mm F2.8 GM",
        "XMP-dc:Identifier": "uuid-child",
        "XMP-xmpMM:DerivedFrom": "uuid-parent",
      },
    });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    const button = await screen.findByRole("button", { name: /inherit metadata/i });
    await userEvent.click(button);

    await waitFor(() => expect(api.inheritMetadata).toHaveBeenCalledWith(42));
    await waitFor(() => expect(screen.getByText("Inherited 2 tags.")).toBeInTheDocument());
  });

  it("reports nothing new when only identity tags come back", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.inheritMetadata).mockResolvedValueOnce({
      inherited: { "XMP-dc:Identifier": "uuid-child" },
    });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    await userEvent.click(await screen.findByRole("button", { name: /inherit metadata/i }));
    await waitFor(() => expect(screen.getByText("Nothing new to inherit.")).toBeInTheDocument());
  });

  // The server's 409/422/503 responses carry the actual reason (no resolved
  // parent, read-only location, exiftool unavailable, ...) -- surfaced
  // as-is rather than re-derived client-side, since the eligibility rules
  // are the server's, not the SPA's, to own.
  it("surfaces the server's error message on failure", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.inheritMetadata).mockRejectedValueOnce(
      new Error("asset has no resolved parent edge to inherit from"),
    );

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    await userEvent.click(await screen.findByRole("button", { name: /inherit metadata/i }));
    await waitFor(() =>
      expect(screen.getByText(/asset has no resolved parent edge to inherit from/i)).toBeInTheDocument(),
    );
  });

  it("hides the control for an ARCHIVED asset", async () => {
    vi.mocked(api.getAsset).mockResolvedValue({ ...asset, lifecycleState: "ARCHIVED" });
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    expect(screen.queryByRole("button", { name: /inherit metadata/i })).not.toBeInTheDocument();
  });
});
