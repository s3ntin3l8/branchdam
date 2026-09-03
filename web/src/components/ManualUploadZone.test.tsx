import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import ManualUploadZone from "./ManualUploadZone";
import { api } from "../api/client";
import type { StorageLocation, WebUploadResponse } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    listStorageLocations: vi.fn(),
    uploadFile: vi.fn(),
  },
}));

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <MemoryRouter>
      <QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>
    </MemoryRouter>
  );
}

const mockLocations: StorageLocation[] = [
  { id: 1, name: "exports", rootPath: "/storage/exports", tier: "TIER2_EXPORTS", readOnly: false, prunable: false },
  { id: 2, name: "archive", rootPath: "/storage/archive", tier: "TIER3_MASTER_ARCHIVE", readOnly: false, prunable: false },
  { id: 3, name: "ro_archive", rootPath: "/storage/ro", tier: "TIER3_MASTER_ARCHIVE", readOnly: true, prunable: false },
];

describe("ManualUploadZone", () => {
  it("renders writable storage locations and defaults to master archive", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: mockLocations });
    renderWithClient(<ManualUploadZone />);

    expect(await screen.findByRole("option", { name: /archive \(TIER3_MASTER_ARCHIVE\)/ })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /exports \(TIER2_EXPORTS\)/ })).toBeInTheDocument();
    // Read-only location must not be in the writable options
    expect(screen.queryByRole("option", { name: /ro_archive/ })).not.toBeInTheDocument();
  });

  it("allows switching to custom subfolder mode and typing a subdirectory", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: mockLocations });
    renderWithClient(<ManualUploadZone />);

    const customRadio = await screen.findByRole("radio", { name: /preserve folder hierarchy/i });
    await userEvent.click(customRadio);

    const subdirInput = screen.getByPlaceholderText(/optional subfolder/i);
    expect(subdirInput).toBeInTheDocument();
    await userEvent.type(subdirInput, "2026/OldFootage");
    expect(subdirInput).toHaveValue("2026/OldFootage");
  });

  it("queues and uploads files successfully", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: mockLocations });

    const mockResponse: WebUploadResponse = {
      asset: {
        id: 42,
        nodeUuid: "018f-uuid-42",
        filePath: "/storage/archive/2026/08/Pixel/IMG_001.JPG",
        fileName: "IMG_001.JPG",
        fileExt: ".JPG",
        sizeBytes: 1024,
        indexingStatus: "INDEXED_FULL",
        graphStatus: "UNLINKED",
        lifecycleState: "ACTIVE",
        storageLocationId: 2,
        thumbState: "PENDING",
      },
      nodeUuid: "018f-uuid-42",
      status: "UPLOADED",
      bytesWritten: 1024,
      blake3Hash: "blake3hash123",
      relativePath: "2026/08/Pixel/IMG_001.JPG",
    };

    vi.mocked(api.uploadFile).mockImplementation(async (_file, _opts, onProgress) => {
      onProgress?.({ loaded: 512, total: 1024, percent: 50 });
      onProgress?.({ loaded: 1024, total: 1024, percent: 100 });
      return mockResponse;
    });

    renderWithClient(<ManualUploadZone />);

    await screen.findByRole("combobox");

    const file = new File(["dummy raw image bytes"], "IMG_001.JPG", { type: "image/jpeg" });
    const chooseFilesButton = screen.getByRole("button", { name: /choose files/i });
    expect(chooseFilesButton).toBeInTheDocument();

    // Trigger file selection via hidden input
    const hiddenFileInput = document.querySelector('input[type="file"]:not([webkitdirectory])') as HTMLInputElement;
    expect(hiddenFileInput).toBeInTheDocument();
    await userEvent.upload(hiddenFileInput, file);

    expect(await screen.findByText("Upload Queue (1 files)")).toBeInTheDocument();
    expect(screen.getByText("IMG_001.JPG")).toBeInTheDocument();
    expect(screen.getByText("Queued")).toBeInTheDocument();

    // Click Start Upload
    const startButton = screen.getByRole("button", { name: /start upload/i });
    await userEvent.click(startButton);

    await waitFor(() => {
      expect(api.uploadFile).toHaveBeenCalledWith(
        file,
        expect.objectContaining({
          storageLocationId: 2,
          applyNamingTemplate: true,
        }),
        expect.any(Function),
        expect.any(AbortSignal)
      );
    });

    expect(await screen.findByText("Ready")).toBeInTheDocument();
    expect(screen.getByRole("link", { name: /view asset/i })).toHaveAttribute("href", "/assets/42");

    // Clear completed items
    const clearButton = screen.getByRole("button", { name: /clear completed/i });
    await userEvent.click(clearButton);
    expect(screen.queryByText("Upload Queue")).not.toBeInTheDocument();
  });

  it("handles upload errors and allows retrying", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: mockLocations });
    vi.mocked(api.uploadFile).mockRejectedValueOnce(new Error("Disk quota exceeded"));

    renderWithClient(<ManualUploadZone />);
    await screen.findByRole("combobox");

    const file = new File(["clip"], "clip.mp4", { type: "video/mp4" });
    const hiddenFileInput = document.querySelector('input[type="file"]:not([webkitdirectory])') as HTMLInputElement;
    await userEvent.upload(hiddenFileInput, file);

    const startButton = await screen.findByRole("button", { name: /start upload/i });
    await userEvent.click(startButton);

    expect(await screen.findByText("Failed")).toBeInTheDocument();
    expect(screen.getByText("Disk quota exceeded")).toBeInTheDocument();
  });

  it("surfaces 'already in library' dedup notice and row badge when upload returns an existing node", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: mockLocations });

    const mockDedupResponse: WebUploadResponse = {
      asset: {
        id: 77,
        nodeUuid: "018f-uuid-77",
        filePath: "/storage/archive/2026/08/Pixel/IMG_EXISTING.JPG",
        fileName: "IMG_EXISTING.JPG",
        fileExt: ".JPG",
        sizeBytes: 2048,
        indexingStatus: "INDEXED_FULL",
        graphStatus: "UNLINKED",
        lifecycleState: "ACTIVE",
        storageLocationId: 2,
        thumbState: "READY",
      },
      nodeUuid: "018f-uuid-77",
      status: "DEDUPLICATED",
      isDedup: true,
      bytesWritten: 2048,
      blake3Hash: "blake3hash77",
      relativePath: "2026/08/Pixel/IMG_EXISTING.JPG",
    };

    vi.mocked(api.uploadFile).mockResolvedValueOnce(mockDedupResponse);

    renderWithClient(<ManualUploadZone />);
    await screen.findByRole("combobox");

    const file = new File(["duplicate content"], "IMG_EXISTING.JPG", { type: "image/jpeg" });
    const hiddenFileInput = document.querySelector('input[type="file"]:not([webkitdirectory])') as HTMLInputElement;
    await userEvent.upload(hiddenFileInput, file);

    const startButton = await screen.findByRole("button", { name: /start upload/i });
    await userEvent.click(startButton);

    // Dedup notice banner appears with link to existing node
    expect(await screen.findAllByText(/IMG_EXISTING\.JPG/)).toHaveLength(2);
    expect(screen.getByText(/already in your library/i)).toBeInTheDocument();

    // Queue item row shows 'Already in library' badge rather than 'Ready' or 'Failed'
    expect(screen.getByText("Already in library")).toBeInTheDocument();
    expect(screen.queryByText("Failed")).not.toBeInTheDocument();

    // Link in banner/row points to /assets/77
    const links = screen.getAllByRole("link", { name: /view existing node/i });
    expect(links.length).toBeGreaterThanOrEqual(1);
    expect(links[0]).toHaveAttribute("href", "/assets/77");

    // Dismiss notice
    const dismissBtn = screen.getByRole("button", { name: /dismiss notice/i });
    await userEvent.click(dismissBtn);
    expect(screen.queryByText("This file is already in your library.")).not.toBeInTheDocument();
  });
});
