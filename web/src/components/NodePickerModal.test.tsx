import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import NodePickerModal, { NODE_PICKER_PAGE_SIZE } from "./NodePickerModal";
import { api } from "../api/client";
import type { Asset, AssetQueryParams } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    listAssets: vi.fn(),
    thumbnailUrl: vi.fn((id: number) => `/api/v1/assets/${id}/thumbnail`),
  },
}));

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      {ui}
    </QueryClientProvider>,
  );
}

const mockAssets: Asset[] = [
  {
    id: 101,
    nodeUuid: "uuid-alpha-101",
    filePath: "/storage/photos/DSC_0001.ARW",
    fileName: "DSC_0001.ARW",
    fileExt: ".ARW",
    sizeBytes: 25000000,
    indexingStatus: "INDEXED_FULL",
    graphStatus: "UNLINKED",
    lifecycleState: "ACTIVE",
    storageLocationId: 1,
    cameraModel: "Sony A7IV",
    thumbState: "READY",
  },
  {
    id: 202,
    nodeUuid: "uuid-beta-202",
    filePath: "/storage/exports/DSC_0001_edit.JPG",
    fileName: "DSC_0001_edit.JPG",
    fileExt: ".JPG",
    sizeBytes: 5000000,
    indexingStatus: "INDEXED_FULL",
    graphStatus: "LINKED",
    lifecycleState: "ACTIVE",
    storageLocationId: 2,
    cameraModel: "Canon R5",
    thumbState: "READY",
  },
  {
    id: 303,
    nodeUuid: "uuid-gamma-303",
    filePath: "/storage/video/CLIP_9999.MP4",
    fileName: "CLIP_9999.MP4",
    fileExt: ".MP4",
    sizeBytes: 150000000,
    indexingStatus: "INDEXED_SHALLOW",
    graphStatus: "UNLINKED",
    lifecycleState: "ACTIVE",
    storageLocationId: 1,
    thumbState: "PENDING",
  },
];

// serverSearch stands in for GET /api/v1/assets: ?q= is a case-insensitive
// file_name substring and limit/offset page the matches.
function serverSearch(library: Asset[]) {
  return async (params: AssetQueryParams = {}) => {
    const q = params.q?.toLowerCase();
    const matches = q ? library.filter((a) => a.fileName.toLowerCase().includes(q)) : library;
    const offset = params.offset ?? 0;
    const limit = params.limit ?? 50;
    return { assets: matches.slice(offset, offset + limit), total: matches.length };
  };
}

function makeAssets(n: number): Asset[] {
  return Array.from({ length: n }, (_, i) => ({
    ...mockAssets[0],
    id: 1000 + i,
    nodeUuid: `uuid-filler-${i}`,
    fileName: `FILLER_${String(i).padStart(4, "0")}.JPG`,
    filePath: `/storage/filler/FILLER_${String(i).padStart(4, "0")}.JPG`,
  }));
}

describe("NodePickerModal", () => {
  it("does not render dialog when isOpen is false", () => {
    vi.mocked(api.listAssets).mockResolvedValue({ assets: mockAssets, total: 3 });
    renderWithClient(
      <NodePickerModal isOpen={false} onClose={vi.fn()} onSelect={vi.fn()} />
    );

    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("renders modal with default title, search input, and candidate assets", async () => {
    vi.mocked(api.listAssets).mockResolvedValue({ assets: mockAssets, total: 3 });
    renderWithClient(
      <NodePickerModal isOpen={true} onClose={vi.fn()} onSelect={vi.fn()} />
    );

    expect(await screen.findByRole("dialog")).toBeInTheDocument();
    expect(screen.getByText("Select Asset Node")).toBeInTheDocument();
    expect(screen.getByPlaceholderText(/search by filename/i)).toBeInTheDocument();
    expect(await screen.findByText("DSC_0001.ARW")).toBeInTheDocument();
    expect(screen.getByText("DSC_0001_edit.JPG")).toBeInTheDocument();
    expect(screen.getByText("CLIP_9999.MP4")).toBeInTheDocument();
    expect(screen.getByText(/showing 1–3 of 3 assets/i)).toBeInTheDocument();
  });

  it("searches server-side by filename instead of filtering the loaded page", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAssets).mockImplementation(serverSearch(mockAssets));
    renderWithClient(
      <NodePickerModal isOpen={true} onClose={vi.fn()} onSelect={vi.fn()} />
    );

    await screen.findByText("DSC_0001.ARW");
    const input = screen.getByPlaceholderText(/search by filename/i);
    await user.type(input, "CLIP");

    await waitFor(() =>
      expect(api.listAssets).toHaveBeenLastCalledWith({ limit: NODE_PICKER_PAGE_SIZE, offset: 0, q: "CLIP" })
    );
    await waitFor(() => expect(screen.queryByText("DSC_0001.ARW")).not.toBeInTheDocument());
    expect(screen.getByText("CLIP_9999.MP4")).toBeInTheDocument();
    expect(screen.queryByText("DSC_0001_edit.JPG")).not.toBeInTheDocument();
    expect(screen.getByText(/showing 1–1 of 1 asset/i)).toBeInTheDocument();
  });

  it("finds an asset that is not on the first page of the library", async () => {
    const user = userEvent.setup();
    // 150 filler assets ahead of the one we want: the old picker loaded a
    // single limit=100 page and filtered it client-side, so it could never
    // show the 151st.
    const library = [...makeAssets(150), { ...mockAssets[2], id: 9001, fileName: "DEEP_ARCHIVE.MP4" }];
    vi.mocked(api.listAssets).mockImplementation(serverSearch(library));
    renderWithClient(
      <NodePickerModal isOpen={true} onClose={vi.fn()} onSelect={vi.fn()} />
    );

    await screen.findByText("FILLER_0000.JPG");
    await user.type(screen.getByPlaceholderText(/search by filename/i), "deep");

    await waitFor(() =>
      expect(api.listAssets).toHaveBeenLastCalledWith(expect.objectContaining({ q: "deep", offset: 0 }))
    );
    expect(await screen.findByText("DEEP_ARCHIVE.MP4")).toBeInTheDocument();
  }, 20_000);

  it("pages through results with Previous/Next", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAssets).mockImplementation(serverSearch(makeAssets(60)));
    renderWithClient(
      <NodePickerModal isOpen={true} onClose={vi.fn()} onSelect={vi.fn()} />
    );

    await screen.findByText("FILLER_0000.JPG");
    expect(screen.getByText(/showing 1–25 of 60 assets/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Previous page" })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "Next page" }));
    expect(await screen.findByText("FILLER_0025.JPG")).toBeInTheDocument();
    expect(api.listAssets).toHaveBeenLastCalledWith({ limit: NODE_PICKER_PAGE_SIZE, offset: 25, q: undefined });

    await user.click(screen.getByRole("button", { name: "Next page" }));
    expect(await screen.findByText("FILLER_0050.JPG")).toBeInTheDocument();
    expect(screen.getByText(/showing 51–60 of 60 assets/i)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Next page" })).toBeDisabled();

    await user.click(screen.getByRole("button", { name: "Previous page" }));
    expect(await screen.findByText("FILLER_0025.JPG")).toBeInTheDocument();
  }, 20_000);

  it("shows empty search state and clears search on clear button click", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAssets).mockImplementation(serverSearch(mockAssets));
    renderWithClient(
      <NodePickerModal isOpen={true} onClose={vi.fn()} onSelect={vi.fn()} />
    );

    await screen.findByText("DSC_0001.ARW");
    const input = screen.getByPlaceholderText(/search by filename/i);
    await user.type(input, "nonexistent");

    expect(await screen.findByText(/no assets matching/i)).toBeInTheDocument();

    const clearBtn = screen.getByRole("button", { name: "Clear search" });
    await user.click(clearBtn);

    expect(await screen.findByText("DSC_0001.ARW")).toBeInTheDocument();
    expect(screen.getByText("DSC_0001_edit.JPG")).toBeInTheDocument();
  });

  it("calls onSelect and onClose when a candidate asset is selected", async () => {
    const user = userEvent.setup();
    const handleSelect = vi.fn();
    const handleClose = vi.fn();
    vi.mocked(api.listAssets).mockResolvedValue({ assets: mockAssets, total: 3 });

    renderWithClient(
      <NodePickerModal
        isOpen={true}
        onClose={handleClose}
        onSelect={handleSelect}
      />
    );

    const selectBtn = await screen.findByText("DSC_0001_edit.JPG");
    await user.click(selectBtn);

    expect(handleSelect).toHaveBeenCalledWith(mockAssets[1]);
    expect(handleClose).toHaveBeenCalledTimes(1);
  });

  it("highlights selected asset when selectedAssetId matches", async () => {
    vi.mocked(api.listAssets).mockResolvedValue({ assets: mockAssets, total: 3 });
    renderWithClient(
      <NodePickerModal
        isOpen={true}
        onClose={vi.fn()}
        onSelect={vi.fn()}
        selectedAssetId={202}
      />
    );

    expect(await screen.findByText("Selected")).toBeInTheDocument();
  });

  it("closes when Cancel or close button or Escape key is pressed", async () => {
    const user = userEvent.setup();
    const handleClose = vi.fn();
    vi.mocked(api.listAssets).mockResolvedValue({ assets: mockAssets, total: 3 });

    renderWithClient(
      <NodePickerModal
        isOpen={true}
        onClose={handleClose}
        onSelect={vi.fn()}
      />
    );

    const cancelBtn = await screen.findByRole("button", { name: "Cancel" });
    await user.click(cancelBtn);
    expect(handleClose).toHaveBeenCalledTimes(1);

    const closeBtn = screen.getByRole("button", { name: "Close dialog" });
    await user.click(closeBtn);
    expect(handleClose).toHaveBeenCalledTimes(2);

    await user.keyboard("{Escape}");
    expect(handleClose).toHaveBeenCalledTimes(3);
  });
});
