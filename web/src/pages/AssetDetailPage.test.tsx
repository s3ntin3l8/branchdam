import { act, render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { MemoryRouter, Route, Routes, useNavigate, type NavigateFunction } from "react-router";
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
    deleteAsset: vi.fn(),
    trashAsset: vi.fn(),
    restoreAsset: vi.fn(),
    thumbnailUrl: vi.fn((id: number) => `/api/v1/assets/${id}/thumbnail`),
    streamUrl: vi.fn((id: number) => `/api/v1/assets/${id}/stream`),
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
  thumbState: "PENDING",
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

const otherAsset: Asset = { ...asset, id: 43, fileName: "a43.jpg", filePath: "/mnt/scratch/a43.jpg" };

// renderAt42AfterVisiting43 renders the page at /assets/43 first, then
// navigates to /assets/42: 43 is then in the query cache, so navigating back
// to it renders straight away with no loading state in between -- the case
// where the page instance (and its controls' local state) is reused.
async function renderAt42AfterVisiting43() {
  vi.mocked(api.getAsset).mockImplementation(async (id: number) => (id === 43 ? otherAsset : asset));
  vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
  vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });

  let navigate: NavigateFunction | undefined;
  function NavigateProbe() {
    navigate = useNavigate();
    return null;
  }
  render(
    <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
      <MemoryRouter initialEntries={["/assets/43"]}>
        <NavigateProbe />
        <Routes>
          <Route path="/assets/:id" element={<AssetDetailPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
  await waitFor(() => expect(screen.getByText("a43.jpg")).toBeInTheDocument());
  const goTo = async (path: string) => {
    await act(async () => {
      await navigate?.(path);
    });
  };
  await goTo("/assets/42");
  await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());
  return goTo;
}

describe("AssetDetailPage purge control", () => {
  // A purge planned for 42 must not survive a navigation to 43, where
  // Confirm Purge would execute 43 under 42's candidate list.
  it("drops a planned purge when navigating to another asset", async () => {
    vi.mocked(api.pruneCache).mockReset();
    vi.mocked(api.pruneCache).mockResolvedValueOnce({
      executed: false,
      candidates: [{ nodeId: 42, filePath: asset.filePath, sizeBytes: asset.sizeBytes, purged: false }],
    });
    const goTo = await renderAt42AfterVisiting43();

    await userEvent.click(await screen.findByRole("button", { name: /purge cache/i }));
    await waitFor(() => expect(screen.getByRole("button", { name: /confirm purge/i })).toBeInTheDocument());

    await goTo("/assets/43");
    await waitFor(() => expect(screen.getByText("a43.jpg")).toBeInTheDocument());

    expect(screen.queryByRole("button", { name: /confirm purge/i })).not.toBeInTheDocument();
    expect(api.pruneCache).toHaveBeenCalledTimes(1);
  });

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

describe("AssetDetailPage trash control", () => {
  it("does not mutate until the dialog is confirmed, and Cancel dismisses it", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    await userEvent.click(await screen.findByRole("button", { name: /trash/i }));
    expect(await screen.findByRole("dialog", { name: /trash/i })).toBeInTheDocument();
    expect(api.trashAsset).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.trashAsset).not.toHaveBeenCalled();
  });

  it("trashes on confirm and closes the dialog", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.trashAsset).mockResolvedValueOnce({ ok: true });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    await userEvent.click(await screen.findByRole("button", { name: /trash/i }));
    await userEvent.click(await screen.findByRole("button", { name: /confirm trash/i }));

    await waitFor(() => expect(api.trashAsset).toHaveBeenCalledWith(42, { keepExports: false }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });

  it("Escape dismisses the dialog without mutating", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    await userEvent.click(await screen.findByRole("button", { name: /trash/i }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();

    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.trashAsset).not.toHaveBeenCalled();
  });
});

describe("AssetDetailPage MISSING asset", () => {
  it("offers neither Trash nor Free Up Space for a MISSING asset", async () => {
    vi.mocked(api.getAsset).mockResolvedValue({ ...asset, lifecycleState: "MISSING" });
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    expect(screen.getByText("Missing on disk")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /^trash$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /free up space/i })).not.toBeInTheDocument();
    // Its file is gone, so there is nothing to write inherited tags into.
    expect(screen.queryByRole("button", { name: /inherit metadata/i })).not.toBeInTheDocument();
  });
});

describe("AssetDetailPage TRASHED asset", () => {
  it("does not offer Inherit Metadata for a TRASHED asset", async () => {
    vi.mocked(api.getAsset).mockResolvedValue({ ...asset, lifecycleState: "TRASHED" });
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    // Its file was moved into .trash/: Restore first, then inherit.
    expect(screen.getByRole("button", { name: /restore asset/i })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /inherit metadata/i })).not.toBeInTheDocument();
  });
});

describe("AssetDetailPage inherit-metadata control", () => {
  function mockPage() {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
  }
  const plannedTags = {
    "EXIF:Make": "SONY",
    "EXIF:LensModel": "FE 24-70mm F2.8 GM",
    "XMP-dc:Identifier": "uuid-child",
    "XMP-xmpMM:DerivedFrom": "uuid-parent",
  };

  // The endpoint rewrites the file in place: the button must only ask the
  // server for a dry-run plan, show it, and write nothing until Confirm.
  it("previews the planned tags with a dry run and writes only on confirm", async () => {
    vi.mocked(api.inheritMetadata).mockReset();
    mockPage();
    vi.mocked(api.inheritMetadata)
      .mockResolvedValueOnce({ inherited: plannedTags, dryRun: true, parentId: 7, parentFileName: "master.arw" })
      .mockResolvedValueOnce({ inherited: plannedTags });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    await userEvent.click(await screen.findByRole("button", { name: /inherit metadata/i }));

    const dialog = await screen.findByRole("dialog", { name: "Inherit Metadata" });
    expect(api.inheritMetadata).toHaveBeenCalledTimes(1);
    expect(api.inheritMetadata).toHaveBeenCalledWith(42, { dryRun: true });
    expect(within(dialog).getByText("master.arw")).toBeInTheDocument();
    expect(within(dialog).getByText("EXIF:LensModel")).toBeInTheDocument();
    expect(within(dialog).getByText("FE 24-70mm F2.8 GM")).toBeInTheDocument();

    await userEvent.click(within(dialog).getByRole("button", { name: "Write Tags" }));

    await waitFor(() => expect(api.inheritMetadata).toHaveBeenCalledTimes(2));
    expect(api.inheritMetadata).toHaveBeenLastCalledWith(42);
    await waitFor(() => expect(screen.getByText("Inherited 2 tags.")).toBeInTheDocument());
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  // React reuses the page across /assets/:id changes, so the preview must
  // be scoped to the asset: one opened on 42 must not survive a navigation
  // to 43, where Write Tags would write 43 under 42's previewed plan.
  it("drops an open preview when navigating to another asset", async () => {
    vi.mocked(api.inheritMetadata).mockReset();
    vi.mocked(api.inheritMetadata).mockResolvedValueOnce({
      inherited: plannedTags,
      dryRun: true,
      parentId: 7,
      parentFileName: "master.arw",
    });
    const goTo = await renderAt42AfterVisiting43();

    await userEvent.click(await screen.findByRole("button", { name: /inherit metadata/i }));
    await screen.findByRole("dialog", { name: "Inherit Metadata" });

    await goTo("/assets/43");
    await waitFor(() => expect(screen.getByText("a43.jpg")).toBeInTheDocument());

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.inheritMetadata).toHaveBeenCalledTimes(1);
  });

  it("writes nothing when the preview is cancelled", async () => {
    vi.mocked(api.inheritMetadata).mockReset();
    mockPage();
    vi.mocked(api.inheritMetadata).mockResolvedValueOnce({ inherited: plannedTags, dryRun: true });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    await userEvent.click(await screen.findByRole("button", { name: /inherit metadata/i }));
    const dialog = await screen.findByRole("dialog", { name: "Inherit Metadata" });
    await userEvent.click(within(dialog).getByRole("button", { name: "Cancel" }));

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.inheritMetadata).toHaveBeenCalledTimes(1);
    expect(api.inheritMetadata).toHaveBeenCalledWith(42, { dryRun: true });
  });

  it("reports nothing new, without a dialog, when the dry run plans no tags", async () => {
    vi.mocked(api.inheritMetadata).mockReset();
    mockPage();
    vi.mocked(api.inheritMetadata).mockResolvedValueOnce({ inherited: {}, dryRun: true });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    await userEvent.click(await screen.findByRole("button", { name: /inherit metadata/i }));
    await waitFor(() => expect(screen.getByText("Nothing new to inherit.")).toBeInTheDocument());
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.inheritMetadata).toHaveBeenCalledTimes(1);
  });

  // The server's 409/422/503 responses carry the actual reason (no resolved
  // parent, read-only location, exiftool unavailable, ...) -- surfaced
  // as-is rather than re-derived client-side, since the eligibility rules
  // are the server's, not the SPA's, to own. The dry run runs the same
  // checks, so the refusal arrives before any confirm is offered.
  it("surfaces the server's error message from the dry run", async () => {
    vi.mocked(api.inheritMetadata).mockReset();
    mockPage();
    vi.mocked(api.inheritMetadata).mockRejectedValueOnce(
      new Error("asset has no resolved parent edge to inherit from"),
    );

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    await userEvent.click(await screen.findByRole("button", { name: /inherit metadata/i }));
    await waitFor(() =>
      expect(screen.getByText(/asset has no resolved parent edge to inherit from/i)).toBeInTheDocument(),
    );
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
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

describe("AssetDetailPage sync status", () => {
  it("surfaces exhausted badge and retry count when sync is exhausted", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.getAssetSyncStatus).mockResolvedValue({
      sync: [
        {
          remote: "immich",
          syncStatus: "PUSH_FAILED",
          retryCount: 3,
          exhausted: true,
          lastError: "connection refused",
        },
      ],
    });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    expect(screen.getByText("immich")).toBeInTheDocument();
    expect(screen.getByText("PUSH_FAILED")).toBeInTheDocument();
    expect(screen.getByText("Exhausted (max retries reached)")).toBeInTheDocument();
    expect(screen.getByText("Retries: 3")).toBeInTheDocument();
    expect(screen.getByText("Error: connection refused")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
  });

  it("surfaces retry count without exhausted badge when retry count > 0 but not exhausted", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.getAssetSyncStatus).mockResolvedValue({
      sync: [
        {
          remote: "immich",
          syncStatus: "PUSH_FAILED",
          retryCount: 2,
          exhausted: false,
        },
      ],
    });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    expect(screen.getByText("Retries: 2")).toBeInTheDocument();
    expect(screen.queryByText("Exhausted (max retries reached)")).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: /retry/i })).toBeInTheDocument();
  });

  it("omits retry count when retryCount is 0", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.getAssetSyncStatus).mockResolvedValue({
      sync: [
        {
          remote: "immich",
          syncStatus: "PUSHED",
          retryCount: 0,
          exhausted: false,
        },
      ],
    });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    expect(screen.getByText("PUSHED")).toBeInTheDocument();
    expect(screen.queryByText(/retries:/i)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /retry/i })).not.toBeInTheDocument();
  });

  it("clicking retry triggers api.retrySync", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.getAssetSyncStatus).mockResolvedValue({
      sync: [
        {
          remote: "immich",
          syncStatus: "PUSH_FAILED",
          retryCount: 3,
          exhausted: true,
        },
      ],
    });
    vi.mocked(api.retrySync).mockResolvedValue({ requeued: 1 });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    const retryButton = screen.getByRole("button", { name: /retry/i });
    await userEvent.click(retryButton);

    await waitFor(() => expect(api.retrySync).toHaveBeenCalledWith(42));
  });

  it("surfaces error message when retrySync fails", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.getAssetSyncStatus).mockResolvedValue({
      sync: [
        {
          remote: "immich",
          syncStatus: "PUSH_FAILED",
          retryCount: 3,
          exhausted: true,
        },
      ],
    });
    vi.mocked(api.retrySync).mockRejectedValueOnce(new Error("server unavailable"));

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    const retryButton = screen.getByRole("button", { name: /retry/i });
    await userEvent.click(retryButton);

    await waitFor(() =>
      expect(screen.getByText(/failed to retry sync: error: server unavailable/i)).toBeInTheDocument(),
    );
  });

  it("hides retry button for an ARCHIVED asset even if sync failed", async () => {
    vi.mocked(api.getAsset).mockResolvedValue({ ...asset, lifecycleState: "ARCHIVED" });
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });
    vi.mocked(api.getAssetSyncStatus).mockResolvedValue({
      sync: [
        {
          remote: "immich",
          syncStatus: "PUSH_FAILED",
          retryCount: 3,
          exhausted: true,
        },
      ],
    });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("proxy.jpg")).toBeInTheDocument());

    expect(screen.getByText("immich")).toBeInTheDocument();
    expect(screen.getByText("Exhausted (max retries reached)")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /retry/i })).not.toBeInTheDocument();
  });
});

describe("AssetDetailPage media preview", () => {
  // The inline preview is the thumbnail; the master is only streamed once
  // the operator opens the lightbox.
  it("previews images from the thumbnail and streams the master only in the lightbox", async () => {
    const readyAsset: Asset = { ...asset, thumbState: "READY" };
    vi.mocked(api.getAsset).mockResolvedValue(readyAsset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [readyAsset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });

    renderWithClient();
    await waitFor(() => expect(screen.getByRole("heading", { name: "proxy.jpg" })).toBeInTheDocument());

    const preview = screen.getByTestId("asset-image-preview");
    expect(within(preview).getByRole("img", { name: "proxy.jpg" })).toHaveAttribute(
      "src",
      "/api/v1/assets/42/thumbnail",
    );
    expect(document.querySelector('img[src="/api/v1/assets/42/stream"]')).toBeNull();

    await userEvent.click(screen.getByRole("button", { name: /view full resolution/i }));

    const lightbox = screen.getByRole("dialog", { name: "proxy.jpg" });
    expect(within(lightbox).getByRole("img", { name: "proxy.jpg" })).toHaveAttribute("src", "/api/v1/assets/42/stream");

    await userEvent.click(screen.getByTestId("lightbox-close-button"));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /view full resolution/i }));
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("renders video player when asset has video extension", async () => {
    const videoAsset: Asset = { ...asset, fileName: "video.mp4", fileExt: "mp4" };
    vi.mocked(api.getAsset).mockResolvedValue(videoAsset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [videoAsset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });

    renderWithClient();
    await waitFor(() => expect(screen.getByText("video.mp4")).toBeInTheDocument());

    const video = screen.getByTestId("asset-video-player");
    expect(video).toBeInTheDocument();
    expect(video.querySelector("source")).toHaveAttribute("src", "/api/v1/assets/42/stream");
    expect(video.querySelector("source")).toHaveAttribute("type", "video/mp4");
  });

  it("switches stream source when proxy pair is available in lineage", async () => {
    const originalAsset: Asset = { ...asset, id: 42, fileName: "clip.mov", fileExt: "mov" };
    const proxyAsset: Asset = { ...asset, id: 43, fileName: "clip_proxy.mp4", fileExt: "mp4" };
    vi.mocked(api.getAsset).mockResolvedValue(originalAsset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({
      rootId: 42,
      nodes: [originalAsset, proxyAsset],
      edges: [
        {
          id: 101,
          sourceNodeId: 42,
          targetNodeId: 43,
          relationshipType: "PROXY_OF",
          confidence: 1.0,
          reviewState: "CONFIRMED",
          resolver: "test",
        },
      ],
    });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [prunableLocation] });

    renderWithClient();
    await waitFor(() => expect(screen.getByRole("heading", { name: "clip.mov" })).toBeInTheDocument());

    const originalBtn = screen.getByRole("button", { name: /original \(mov\)/i });
    const proxyBtn = screen.getByRole("button", { name: /proxy \(mp4\)/i });
    expect(originalBtn).toBeInTheDocument();
    expect(proxyBtn).toBeInTheDocument();

    // Initially original is selected
    expect(screen.getByTestId("asset-video-player").querySelector("source")).toHaveAttribute(
      "src",
      "/api/v1/assets/42/stream",
    );

    // Switch to proxy
    await userEvent.click(proxyBtn);
    expect(screen.getByTestId("asset-video-player").querySelector("source")).toHaveAttribute(
      "src",
      "/api/v1/assets/43/stream",
    );
  });
});

describe("AssetDetailPage load errors", () => {
  it("says the asset is not found only for a 404", async () => {
    vi.mocked(api.getAsset).mockRejectedValue(Object.assign(new Error("asset not found"), { status: 404 }));
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });

    renderWithClient();
    expect(await screen.findByRole("heading", { name: "Asset not found" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /retry/i })).not.toBeInTheDocument();
  });

  it("reports other failures as a load error with a retry, not as not-found", async () => {
    vi.mocked(api.getAsset).mockRejectedValueOnce(
      Object.assign(new Error("internal server error"), { status: 500 }),
    );
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });

    renderWithClient();
    expect(await screen.findByRole("heading", { name: "Couldn't load this asset" })).toBeInTheDocument();
    expect(screen.getByText("internal server error")).toBeInTheDocument();
    expect(screen.queryByText("Asset not found")).not.toBeInTheDocument();

    vi.mocked(api.getAsset).mockResolvedValue(asset);
    await userEvent.click(screen.getByRole("button", { name: /retry/i }));
    expect(await screen.findByRole("heading", { name: "proxy.jpg" })).toBeInTheDocument();
  });

  it("explains a non-numeric id instead of fetching it", async () => {
    vi.mocked(api.getAsset).mockClear();
    const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={queryClient}>
        <MemoryRouter initialEntries={["/assets/018f-uuid-42"]}>
          <Routes>
            <Route path="/assets/:id" element={<AssetDetailPage />} />
          </Routes>
        </MemoryRouter>
      </QueryClientProvider>,
    );
    expect(screen.getByRole("heading", { name: "Invalid asset id" })).toBeInTheDocument();
    expect(api.getAsset).not.toHaveBeenCalled();
  });

  it("falls back to depth 2 for a fractional or out-of-range ?depth", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });

    const renderAt = (path: string) =>
      render(
        <QueryClientProvider client={new QueryClient({ defaultOptions: { queries: { retry: false } } })}>
          <MemoryRouter initialEntries={[path]}>
            <Routes>
              <Route path="/assets/:id" element={<AssetDetailPage />} />
            </Routes>
          </MemoryRouter>
        </QueryClientProvider>,
      );

    for (const raw of ["2.5", "9", "abc"]) {
      vi.mocked(api.getAssetLineage).mockClear();
      const { unmount } = renderAt(`/assets/42?depth=${raw}`);
      await waitFor(() => expect(api.getAssetLineage).toHaveBeenCalled());
      expect(api.getAssetLineage).toHaveBeenCalledWith(42, 2);
      expect(await screen.findByLabelText("Traversal depth")).toHaveValue("2");
      unmount();
    }

    // A whole number in range is honoured.
    vi.mocked(api.getAssetLineage).mockClear();
    renderAt("/assets/42?depth=4");
    await waitFor(() => expect(api.getAssetLineage).toHaveBeenCalledWith(42, 4));
    expect(await screen.findByLabelText("Traversal depth")).toHaveValue("4");
  });

  it("shows the size human-readable, with exact bytes on hover", async () => {
    vi.mocked(api.getAsset).mockResolvedValue(asset);
    vi.mocked(api.getAssetLineage).mockResolvedValue({ rootId: 42, nodes: [asset], edges: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });

    renderWithClient();
    const size = await screen.findByText("12.1 KB");
    expect(size).toHaveAttribute("title", `${(12345).toLocaleString()} bytes`);
  });
});
