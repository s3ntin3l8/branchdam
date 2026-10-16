import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor, within } from "@testing-library/react";
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

    expect(screen.getByRole("tablist", { name: "Ingest method" })).toBeInTheDocument();
    const upload = screen.getByRole("tab", { name: /manual web upload/i });
    const scan = screen.getByRole("tab", { name: /storage scan/i });
    expect(upload).toHaveAttribute("aria-selected", "true");
    expect(scan).toHaveAttribute("aria-selected", "false");
    expect(screen.queryByRole("button", { pressed: true })).not.toBeInTheDocument();
    // The selected tab controls the panel, and the panel is named by it.
    const panel = screen.getByRole("tabpanel", { name: /manual web upload/i });
    expect(upload).toHaveAttribute("aria-controls", panel.id);
    expect(scan).not.toHaveAttribute("aria-controls");
    expect(within(panel).getByText(/drag and drop media files or folders here/i)).toBeInTheDocument();
  });

  it("moves between tabs with the arrow keys, Home and End", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    renderWithClient(<IngestPage />);

    const upload = screen.getByRole("tab", { name: /manual web upload/i });
    const scan = screen.getByRole("tab", { name: /storage scan/i });
    // Roving tabindex: only the selected tab is in the Tab order.
    expect(upload).toHaveAttribute("tabindex", "0");
    expect(scan).toHaveAttribute("tabindex", "-1");

    upload.focus();
    await userEvent.keyboard("{ArrowRight}");
    expect(scan).toHaveFocus();
    expect(scan).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tabpanel", { name: /storage scan/i })).toBeInTheDocument();

    await userEvent.keyboard("{ArrowRight}");
    expect(upload).toHaveFocus();
    expect(upload).toHaveAttribute("aria-selected", "true");

    await userEvent.keyboard("{End}");
    expect(scan).toHaveFocus();
    await userEvent.keyboard("{Home}");
    expect(upload).toHaveFocus();
    await userEvent.keyboard("{ArrowLeft}");
    expect(scan).toHaveFocus();
    expect(scan).toHaveAttribute("aria-selected", "true");
  });

  it("switches to Storage Scan tab and lists locations in the picker", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    renderWithClient(<IngestPage />);

    await userEvent.click(screen.getByRole("tab", { name: /storage scan/i }));

    expect(await screen.findByRole("combobox")).toBeInTheDocument();
    expect(await screen.findByRole("option", { name: /exports · TIER2_EXPORTS/ })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /archive · TIER3_MASTER_ARCHIVE · read-only/ })).toBeInTheDocument();
    expect(screen.getByText(/read-only locations/i)).toBeInTheDocument();
    // Tier 3 is writable by default (invariant 3); the hint must not claim
    // the master archive tier is read-only as such.
    expect(screen.getByText(/read-only locations/i)).toHaveTextContent(/Tier 3 \(master archive\) is\s+writable by default/);
    expect(screen.queryByText(/e\.g\. TIER3_MASTER_ARCHIVE/)).not.toBeInTheDocument();
  });

  it("fires startScan with the selected location id and differential:false", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    vi.mocked(api.startScan).mockResolvedValue({ jobId: 7 });
    renderWithClient(<IngestPage />);

    await userEvent.click(screen.getByRole("tab", { name: /storage scan/i }));

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

    await userEvent.click(screen.getByRole("tab", { name: /storage scan/i }));

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

    await userEvent.click(screen.getByRole("tab", { name: /storage scan/i }));

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

    await userEvent.click(screen.getByRole("tab", { name: /storage scan/i }));

    expect(await screen.findByText("FULL_SCAN")).toBeInTheDocument();
    expect(screen.getByText("FAILED")).toBeInTheDocument();
    expect(screen.getByText(/simulated walk failure/)).toBeInTheDocument();
  });

  it("surfaces an inline error message when startScan fails", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    vi.mocked(api.startScan).mockRejectedValue(new Error("Storage location not found"));
    renderWithClient(<IngestPage />);

    await userEvent.click(screen.getByRole("tab", { name: /storage scan/i }));

    await screen.findByRole("option", { name: /exports · TIER2_EXPORTS/ });
    const select = await screen.findByRole("combobox");
    await userEvent.selectOptions(select, "1");
    await userEvent.click(screen.getByRole("button", { name: /^scan$/i }));

    expect(await screen.findByText(/Failed to start scan: Error: Storage location not found/i)).toBeInTheDocument();
  });

  it("renders WATCH jobs in CANCELLED state as STOPPED and RUNNING as ACTIVE in recent jobs table", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({
      jobs: [
        {
          id: 17,
          kind: "WATCH",
          state: "RUNNING",
          filesSeen: 10,
          filesHashed: 10,
          filesFailed: 0,
          edgesCreated: 3,
        },
        {
          id: 16,
          kind: "WATCH",
          state: "CANCELLED",
          filesSeen: 0,
          filesHashed: 0,
          filesFailed: 0,
          edgesCreated: 0,
        },
      ],
    });
    renderWithClient(<IngestPage />);

    await userEvent.click(screen.getByRole("tab", { name: /storage scan/i }));

    expect(await screen.findByText("ACTIVE")).toBeInTheDocument();
    expect(screen.getByText("STOPPED")).toBeInTheDocument();
  });
});
