import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import NodePickerModal from "./NodePickerModal";
import { api } from "../api/client";
import type { Asset } from "../api/types";

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
    expect(screen.getByText(/showing 3 of 3 assets/i)).toBeInTheDocument();
  });

  it("filters candidate nodes by filename", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAssets).mockResolvedValue({ assets: mockAssets, total: 3 });
    renderWithClient(
      <NodePickerModal isOpen={true} onClose={vi.fn()} onSelect={vi.fn()} />
    );

    await screen.findByText("DSC_0001.ARW");
    const input = screen.getByPlaceholderText(/search by filename/i);
    await user.type(input, "CLIP");

    expect(screen.getByText("CLIP_9999.MP4")).toBeInTheDocument();
    expect(screen.queryByText("DSC_0001.ARW")).not.toBeInTheDocument();
    expect(screen.queryByText("DSC_0001_edit.JPG")).not.toBeInTheDocument();
    expect(screen.getByText(/showing 1 of 3 assets/i)).toBeInTheDocument();
  });

  it("filters candidate nodes by camera model", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAssets).mockResolvedValue({ assets: mockAssets, total: 3 });
    renderWithClient(
      <NodePickerModal isOpen={true} onClose={vi.fn()} onSelect={vi.fn()} />
    );

    await screen.findByText("DSC_0001.ARW");
    const input = screen.getByPlaceholderText(/search by filename/i);
    await user.type(input, "Canon");

    expect(screen.getByText("DSC_0001_edit.JPG")).toBeInTheDocument();
    expect(screen.queryByText("DSC_0001.ARW")).not.toBeInTheDocument();
    expect(screen.queryByText("CLIP_9999.MP4")).not.toBeInTheDocument();
  });

  it("filters candidate nodes by UUID", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAssets).mockResolvedValue({ assets: mockAssets, total: 3 });
    renderWithClient(
      <NodePickerModal isOpen={true} onClose={vi.fn()} onSelect={vi.fn()} />
    );

    await screen.findByText("DSC_0001.ARW");
    const input = screen.getByPlaceholderText(/search by filename/i);
    await user.type(input, "gamma-303");

    expect(screen.getByText("CLIP_9999.MP4")).toBeInTheDocument();
    expect(screen.queryByText("DSC_0001.ARW")).not.toBeInTheDocument();
  });

  it("filters candidate nodes by numeric ID", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAssets).mockResolvedValue({ assets: mockAssets, total: 3 });
    renderWithClient(
      <NodePickerModal isOpen={true} onClose={vi.fn()} onSelect={vi.fn()} />
    );

    await screen.findByText("DSC_0001.ARW");
    const input = screen.getByPlaceholderText(/search by filename/i);
    await user.type(input, "101");

    expect(screen.getByText("DSC_0001.ARW")).toBeInTheDocument();
    expect(screen.queryByText("DSC_0001_edit.JPG")).not.toBeInTheDocument();
  });

  it("shows empty search state and clears search on clear button click", async () => {
    const user = userEvent.setup();
    vi.mocked(api.listAssets).mockResolvedValue({ assets: mockAssets, total: 3 });
    renderWithClient(
      <NodePickerModal isOpen={true} onClose={vi.fn()} onSelect={vi.fn()} />
    );

    await screen.findByText("DSC_0001.ARW");
    const input = screen.getByPlaceholderText(/search by filename/i);
    await user.type(input, "nonexistent");

    expect(screen.getByText(/no assets matching/i)).toBeInTheDocument();

    const clearBtn = screen.getByRole("button", { name: "Clear search" });
    await user.click(clearBtn);

    expect(screen.getByText("DSC_0001.ARW")).toBeInTheDocument();
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
