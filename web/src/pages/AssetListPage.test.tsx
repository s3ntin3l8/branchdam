import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import AssetListPage from "./AssetListPage";
import { api } from "../api/client";
import type { Asset } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    listAssets: vi.fn(),
    getAssetFacets: vi.fn(),
    listStorageLocations: vi.fn(),
    thumbnailUrl: vi.fn((id: number) => `/api/v1/assets/${id}/thumbnail`),
    deleteAsset: vi.fn(),
    restoreAsset: vi.fn(),
  },
}));

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>
  );
}

function baseAsset(overrides: Partial<Asset>): Asset {
  return {
    id: 1,
    nodeUuid: "uuid-1",
    filePath: "/scratch/photo1.jpg",
    fileName: "photo1.jpg",
    fileExt: ".jpg",
    sizeBytes: 2048,
    indexingStatus: "INDEXED_FULL",
    graphStatus: "UNLINKED",
    lifecycleState: "ACTIVE",
    storageLocationId: 1,
    cameraModel: "Sony A7IV",
    thumbState: "READY",
    ...overrides,
  };
}

describe("AssetListPage", () => {
  it("renders asset list, filters panel, and pagination", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValueOnce({ cameraModels: ["Sony A7IV", "Canon R5"] });
    vi.mocked(api.listStorageLocations).mockResolvedValueOnce({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValueOnce({
      assets: [baseAsset({})],
      total: 1,
    });

    renderWithClient(<AssetListPage />);

    await waitFor(() => {
      expect(screen.getByText("/scratch/photo1.jpg")).toBeInTheDocument();
    });

    expect(screen.getAllByText("Sony A7IV").length).toBeGreaterThan(0);
    expect(screen.getByText("Unlinked Only")).toBeInTheDocument();
    expect(screen.getByText("Showing 1 to 1 of 1 items")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Previous page" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Next page" })).toBeInTheDocument();
  });
});

describe("AssetListPage per-row archive", () => {
  it("does not mutate until confirmed, and Cancel dismisses the dialog", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValue({ assets: [baseAsset({ id: 1 })], total: 1 });

    renderWithClient(<AssetListPage />);
    await waitFor(() => expect(screen.getByText("/scratch/photo1.jpg")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: "Archive" }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(api.deleteAsset).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.deleteAsset).not.toHaveBeenCalled();
  });

  it("archives on confirm", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValue({ assets: [baseAsset({ id: 1 })], total: 1 });
    vi.mocked(api.deleteAsset).mockResolvedValueOnce({ ok: true });

    renderWithClient(<AssetListPage />);
    await waitFor(() => expect(screen.getByText("/scratch/photo1.jpg")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: "Archive" }));
    await userEvent.click(await screen.findByRole("button", { name: /confirm archive/i }));

    await waitFor(() => expect(api.deleteAsset).toHaveBeenCalledWith(1));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });
});

describe("AssetListPage batch archive", () => {
  const assets = [
    baseAsset({ id: 1, filePath: "/scratch/a.jpg", fileName: "a.jpg" }),
    baseAsset({ id: 2, filePath: "/scratch/b.jpg", fileName: "b.jpg" }),
    baseAsset({ id: 3, filePath: "/scratch/c.jpg", fileName: "c.jpg", lifecycleState: "ARCHIVED" }),
  ];

  it("select-all only selects non-archived rows on the page", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValue({ assets, total: 3 });

    renderWithClient(<AssetListPage />);
    await waitFor(() => expect(screen.getByText("/scratch/a.jpg")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("checkbox", { name: "Select all on this page" }));

    expect(screen.getByRole("checkbox", { name: "Select a.jpg" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Select b.jpg" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Select c.jpg" })).toBeDisabled();
    expect(screen.getByRole("checkbox", { name: "Select c.jpg" })).not.toBeChecked();
    expect(screen.getByText("2 selected")).toBeInTheDocument();
  });

  it("issues one archive call per selected id on confirm", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValue({ assets, total: 3 });
    vi.mocked(api.deleteAsset).mockResolvedValue({ ok: true });

    renderWithClient(<AssetListPage />);
    await waitFor(() => expect(screen.getByText("/scratch/a.jpg")).toBeInTheDocument());
    const listCallsBeforeBatch = vi.mocked(api.listAssets).mock.calls.length;

    await userEvent.click(screen.getByRole("checkbox", { name: "Select all on this page" }));
    await userEvent.click(screen.getByRole("button", { name: "Archive selected" }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /confirm archive/i }));

    await waitFor(() => expect(api.deleteAsset).toHaveBeenCalledTimes(2));
    expect(api.deleteAsset).toHaveBeenCalledWith(1);
    expect(api.deleteAsset).toHaveBeenCalledWith(2);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    await waitFor(() => expect(screen.queryByText(/selected/)).not.toBeInTheDocument());

    // The list refetches exactly ONCE after the whole batch settles, not
    // once per archived row -- confirms the batch loop calls api.deleteAsset
    // directly instead of the useDeleteAsset hook (whose per-call
    // invalidation would refetch mid-batch and re-render the rows the loop
    // is still iterating over).
    await waitFor(() => expect(vi.mocked(api.listAssets).mock.calls.length).toBe(listCallsBeforeBatch + 1));
  });

  it("keeps failed ids selected and surfaces the failure count on partial failure", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValue({ assets, total: 3 });
    vi.mocked(api.deleteAsset).mockImplementation((id: number) =>
      id === 2 ? Promise.reject(new Error("archive failed")) : Promise.resolve({ ok: true })
    );

    renderWithClient(<AssetListPage />);
    await waitFor(() => expect(screen.getByText("/scratch/a.jpg")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("checkbox", { name: "Select all on this page" }));
    await userEvent.click(screen.getByRole("button", { name: "Archive selected" }));
    await userEvent.click(await screen.findByRole("button", { name: /confirm archive/i }));

    await waitFor(() => expect(api.deleteAsset).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.getByText(/1 failed to archive/)).toBeInTheDocument());
    expect(screen.getByText("1 selected")).toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: "Select b.jpg" })).toBeChecked();
    expect(screen.getByRole("checkbox", { name: "Select a.jpg" })).not.toBeChecked();
  });
});
