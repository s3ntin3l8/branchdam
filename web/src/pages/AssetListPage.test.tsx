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

  // Regression test for a Hermes finding: deleteAsset is one shared
  // mutation instance across every row. Without resetting it at the
  // per-row dialog's open/close boundary, a failed archive's error state
  // survives into the NEXT row's dialog before that row has been touched.
  it("does not leak a previous row's archive error into a different row's dialog", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValue({
      assets: [
        baseAsset({ id: 1, filePath: "/scratch/a.jpg", fileName: "a.jpg" }),
        baseAsset({ id: 2, filePath: "/scratch/b.jpg", fileName: "b.jpg" }),
      ],
      total: 2,
    });
    vi.mocked(api.deleteAsset).mockRejectedValueOnce(new Error("row a failed"));

    renderWithClient(<AssetListPage />);
    await waitFor(() => expect(screen.getByText("/scratch/a.jpg")).toBeInTheDocument());

    const archiveButtons = screen.getAllByRole("button", { name: "Archive" });

    // Row A: open, confirm, fail -> dialog stays open showing the error.
    await userEvent.click(archiveButtons[0]);
    await userEvent.click(await screen.findByRole("button", { name: /confirm archive/i }));
    await waitFor(() => expect(screen.getByText(/failed to archive: .*row a failed/i)).toBeInTheDocument());

    // Cancel row A's dialog, then open row B's dialog fresh.
    await userEvent.click(screen.getByRole("button", { name: /cancel/i }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();

    await userEvent.click(screen.getAllByRole("button", { name: "Archive" })[1]);
    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(screen.queryByText(/failed to archive/i)).not.toBeInTheDocument();
  });
});

describe("AssetListPage restore", () => {
  it("restores on click", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValue({
      assets: [baseAsset({ id: 5, filePath: "/scratch/archived.jpg", fileName: "archived.jpg", lifecycleState: "ARCHIVED" })],
      total: 1,
    });
    vi.mocked(api.restoreAsset).mockResolvedValueOnce({ ok: true });

    renderWithClient(<AssetListPage />);
    await waitFor(() => expect(screen.getByText("/scratch/archived.jpg")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: "Restore" }));
    await waitFor(() => expect(api.restoreAsset).toHaveBeenCalledWith(5));
  });

  it("surfaces an error when restore fails, scoped to that row", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValue({
      assets: [
        baseAsset({ id: 5, filePath: "/scratch/a.jpg", fileName: "a.jpg", lifecycleState: "ARCHIVED" }),
        baseAsset({ id: 6, filePath: "/scratch/b.jpg", fileName: "b.jpg", lifecycleState: "ARCHIVED" }),
      ],
      total: 2,
    });
    vi.mocked(api.restoreAsset).mockRejectedValueOnce(new Error("restore failed"));

    renderWithClient(<AssetListPage />);
    await waitFor(() => expect(screen.getByText("/scratch/a.jpg")).toBeInTheDocument());

    const restoreButtons = screen.getAllByRole("button", { name: "Restore" });
    await userEvent.click(restoreButtons[0]);

    await waitFor(() => expect(screen.getByText(/failed to restore: .*restore failed/i)).toBeInTheDocument());
    // The error is scoped to row A -- row B's Restore button shows no error.
    expect(screen.getAllByText(/failed to restore/i)).toHaveLength(1);
  });

  it("disables every row's Restore button while one restore is in flight", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValue({
      assets: [
        baseAsset({ id: 5, filePath: "/scratch/a.jpg", fileName: "a.jpg", lifecycleState: "ARCHIVED" }),
        baseAsset({ id: 6, filePath: "/scratch/b.jpg", fileName: "b.jpg", lifecycleState: "ARCHIVED" }),
      ],
      total: 2,
    });
    // Row A's restore never settles during this test -- it stands in for a
    // slow request so we can assert row B's button is inert while it's in
    // flight, which is what stops a second click from clobbering row A's
    // pending/error state on the single shared mutation.
    let resolveA: (() => void) | undefined;
    vi.mocked(api.restoreAsset).mockImplementationOnce(
      () => new Promise((resolve) => { resolveA = () => resolve({ ok: true }); })
    );

    renderWithClient(<AssetListPage />);
    await waitFor(() => expect(screen.getByText("/scratch/a.jpg")).toBeInTheDocument());

    const restoreButtons = screen.getAllByRole("button", { name: /restor/i });
    await userEvent.click(restoreButtons[0]);

    await waitFor(() => expect(screen.getAllByRole("button", { name: /restor/i })[0]).toHaveTextContent(/restoring/i));
    const [rowA, rowB] = screen.getAllByRole("button", { name: /restor/i });
    expect(rowA).toBeDisabled();
    expect(rowB).toBeDisabled();

    resolveA?.();
    await waitFor(() => expect(api.restoreAsset).toHaveBeenCalledWith(5));
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

  it("Cancel dismisses the batch dialog without archiving, keeping the selection", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValue({ assets, total: 3 });

    renderWithClient(<AssetListPage />);
    await waitFor(() => expect(screen.getByText("/scratch/a.jpg")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("checkbox", { name: "Select all on this page" }));
    await userEvent.click(screen.getByRole("button", { name: "Archive selected" }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /cancel/i }));

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.deleteAsset).not.toHaveBeenCalled();
    expect(screen.getByText("2 selected")).toBeInTheDocument();
  });

  it("Escape dismisses the batch dialog without archiving", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValue({ assets, total: 3 });

    renderWithClient(<AssetListPage />);
    await waitFor(() => expect(screen.getByText("/scratch/a.jpg")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("checkbox", { name: "Select all on this page" }));
    await userEvent.click(screen.getByRole("button", { name: "Archive selected" }));
    expect(await screen.findByRole("dialog")).toBeInTheDocument();

    await userEvent.keyboard("{Escape}");

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(api.deleteAsset).not.toHaveBeenCalled();
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
