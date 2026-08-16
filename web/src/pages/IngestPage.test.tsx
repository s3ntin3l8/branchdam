import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import IngestPage from "./IngestPage";
import { api } from "../api/client";
import type { StorageLocation } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    listStorageLocations: vi.fn(),
    startScan: vi.fn(),
    listProgress: vi.fn(),
  },
}));

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>);
}

const locations: StorageLocation[] = [
  { id: 1, name: "exports", rootPath: "/storage/exports", tier: "TIER2_EXPORTS", readOnly: false, prunable: false },
  { id: 2, name: "archive", rootPath: "/storage/archive", tier: "TIER3_MASTER_ARCHIVE", readOnly: true, prunable: false },
];

describe("IngestPage", () => {
  it("lists locations in the picker", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    renderWithClient(<IngestPage />);

    expect(await screen.findByRole("combobox")).toBeInTheDocument();
    expect(await screen.findByRole("option", { name: /exports · TIER2_EXPORTS/ })).toBeInTheDocument();
    expect(screen.getByRole("option", { name: /archive · TIER3_MASTER_ARCHIVE · read-only/ })).toBeInTheDocument();
    expect(screen.getByText(/read-only locations/i)).toBeInTheDocument();
  });

  it("fires startScan with the selected location id", async () => {
    vi.mocked(api.listStorageLocations).mockResolvedValue({ locations });
    vi.mocked(api.listProgress).mockResolvedValue({ jobs: [] });
    vi.mocked(api.startScan).mockResolvedValue({ jobId: 7 });
    renderWithClient(<IngestPage />);

    await screen.findByRole("option", { name: /exports · TIER2_EXPORTS/ });
    const select = await screen.findByRole("combobox");
    await userEvent.selectOptions(select, "1");
    await userEvent.click(screen.getByRole("button", { name: /scan/i }));

    await waitFor(() => expect(api.startScan).toHaveBeenCalledWith(1));
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

    expect(await screen.findByText("FULL_SCAN")).toBeInTheDocument();
    expect(screen.getByText("FAILED")).toBeInTheDocument();
    expect(screen.getByText(/simulated walk failure/)).toBeInTheDocument();
  });
});
