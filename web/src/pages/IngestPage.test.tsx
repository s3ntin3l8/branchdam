import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import IngestPage from "./IngestPage";
import { api } from "../api/client";
import type { StorageLocation } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    listStorageLocations: vi.fn(),
    startScan: vi.fn(),
    listProgress: vi.fn(),
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

const locations: StorageLocation[] = [
  { id: 1, name: "exports", rootPath: "/storage/exports", tier: "TIER2_EXPORTS", readOnly: false, prunable: false },
  { id: 2, name: "archive", rootPath: "/storage/archive", tier: "TIER3_MASTER_ARCHIVE", readOnly: true, prunable: false },
];

describe("IngestPage", () => {
  it("renders tabs and defaults to Manual Web Upload tab", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    renderWithClient(<IngestPage />);

    expect(screen.getByRole("button", { name: /manual web upload/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /storage scan/i })).toBeInTheDocument();
    expect(screen.getByText(/drag and drop media files or folders here/i)).toBeInTheDocument();
  });

  it("switches to Storage Scan tab and lists locations in the picker", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    renderWithClient(<IngestPage />);

    await userEvent.click(screen.getByRole("button", { name: /storage scan/i }));

    expect(await screen.findByRole("combobox")).toBeInTheDocument();
    expect(await screen.findByRole("option", { name: /exports · TIER2_EXPORTS/ })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /archive · TIER3_MASTER_ARCHIVE · read-only/ })).toBeInTheDocument();
    expect(screen.getByText(/read-only locations/i)).toBeInTheDocument();
  });

  it("fires startScan with the selected location id and differential:false", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    vi.mocked(api.startScan).mockResolvedValue({ jobId: 7 });
    renderWithClient(<IngestPage />);

    await userEvent.click(screen.getByRole("button", { name: /storage scan/i }));

    await screen.findByRole("option", { name: /exports · TIER2_EXPORTS/ });
    const select = await screen.findByRole("combobox");
    await userEvent.selectOptions(select, "1");
    await userEvent.click(screen.getByRole("button", { name: /^scan$/i }));

    await waitFor(() => expect(api.startScan).toHaveBeenCalledWith({ storageLocationId: 1, differential: false }));
  });

  it("hides the differential toggle for a non-Tier-3 location", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    renderWithClient(<IngestPage />);

    await userEvent.click(screen.getByRole("button", { name: /storage scan/i }));

    await screen.findByRole("option", { name: /exports · TIER2_EXPORTS/ });
    const select = await screen.findByRole("combobox");
    await userEvent.selectOptions(select, "1");

    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
  });

  it("shows the differential toggle for a Tier-3 location and fires startScan with differential:true when checked", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    vi.mocked(api.startScan).mockResolvedValue({ jobId: 8 });
    renderWithClient(<IngestPage />);

    await userEvent.click(screen.getByRole("button", { name: /storage scan/i }));

    await screen.findByRole("option", { name: /archive · TIER3_MASTER_ARCHIVE · read-only/ });
    const select = await screen.findByRole("combobox");
    await userEvent.selectOptions(select, "2");

    const checkbox = await screen.findByRole("checkbox");
    await userEvent.click(checkbox);
    await userEvent.click(screen.getByRole("button", { name: /^scan$/i }));

    await waitFor(() => expect(api.startScan).toHaveBeenCalledWith({ storageLocationId: 2, differential: true }));
  });

  it("renders a failed job row with its last error", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({
      jobs: [
        {
          id: 3,
          kind: "FULL_SCAN",
          state: "FAILED",
          filesSeen: 10,
          filesHashed: 8,
          filesFailed: 2,
          edgesCreated: 0,
          lastError: "simulated walk failure",
        },
      ],
    });
    renderWithClient(<IngestPage />);

    await userEvent.click(screen.getByRole("button", { name: /storage scan/i }));

    expect(await screen.findByText("FULL_SCAN")).toBeInTheDocument();
    expect(screen.getByText("FAILED")).toBeInTheDocument();
    expect(screen.getByText(/simulated walk failure/)).toBeInTheDocument();
  });

  it("surfaces an inline error message when startScan fails", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    vi.mocked(api.startScan).mockRejectedValue(new Error("Storage location not found"));
    renderWithClient(<IngestPage />);

    await userEvent.click(screen.getByRole("button", { name: /storage scan/i }));

    await screen.findByRole("option", { name: /exports · TIER2_EXPORTS/ });
    const select = await screen.findByRole("combobox");
    await userEvent.selectOptions(select, "1");
    await userEvent.click(screen.getByRole("button", { name: /^scan$/i }));

    expect(await screen.findByText(/Failed to start scan: Error: Storage location not found/i)).toBeInTheDocument();
  });
});
