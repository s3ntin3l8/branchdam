import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import StorageHealthPage from "./StorageHealthPage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    getStorageHealth: vi.fn(),
    pruneCache: vi.fn(),
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

describe("StorageHealthPage", () => {
  it("renders storage location gauges and queue depth metrics", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValueOnce({
      locations: [
        {
          id: 1,
          name: "Scratch Mount",
          rootPath: "/mnt/scratch",
          tier: "TIER1_LOCAL_SCRATCH",
          readOnly: false,
          prunable: true,
          isActive: true,
          nodeCount: 1500,
          totalBytes: 1000000000000,
          usedBytes: 400000000000,
          freeBytes: 600000000000,
          isDegraded: false,
        },
        {
          id: 2,
          name: "Master Archive",
          rootPath: "/mnt/archive",
          tier: "TIER3_MASTER_ARCHIVE",
          readOnly: true,
          prunable: false,
          isActive: true,
          nodeCount: 0,
          totalBytes: 0,
          usedBytes: 0,
          freeBytes: 0,
          isDegraded: true,
          degradedMessage: "permission denied",
        },
      ],
      queues: {
        workerPoolInFlight: 3,
        workerPoolQueued: 12,
        workerPoolCapacity: 64,
        workerCount: 4,
        runningScanJobs: 1,
      },
    });

    renderWithClient(<StorageHealthPage />);

    await waitFor(() => {
      expect(screen.getByText("Storage Health")).toBeInTheDocument();
    });

    expect(screen.getByText("Scratch Mount")).toBeInTheDocument();
    expect(screen.getByText("TIER1_LOCAL_SCRATCH")).toBeInTheDocument();
    expect(screen.getByText("HEALTHY")).toBeInTheDocument();
    expect(screen.getByText("1,500")).toBeInTheDocument();

    expect(screen.getByText("Master Archive")).toBeInTheDocument();
    expect(screen.getByText("DEGRADED")).toBeInTheDocument();
    expect(screen.getByText("permission denied")).toBeInTheDocument();

    expect(screen.getByText("3")).toBeInTheDocument();
    expect(screen.getByText("12 / 64")).toBeInTheDocument();
    expect(screen.getByText("4")).toBeInTheDocument();
    expect(screen.getByText("1")).toBeInTheDocument();
  });

  it("only shows the purge control on prunable locations", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValueOnce({
      locations: [
        {
          id: 1, name: "Scratch Mount", rootPath: "/mnt/scratch", tier: "TIER1_LOCAL_SCRATCH",
          readOnly: false, prunable: true, isActive: true, nodeCount: 1, totalBytes: 100, usedBytes: 50,
          freeBytes: 50, isDegraded: false,
        },
        {
          id: 2, name: "Master Archive", rootPath: "/mnt/archive", tier: "TIER3_MASTER_ARCHIVE",
          readOnly: true, prunable: false, isActive: true, nodeCount: 0, totalBytes: 0, usedBytes: 0,
          freeBytes: 0, isDegraded: false,
        },
      ],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());

    expect(screen.getAllByRole("button", { name: /purge cache/i })).toHaveLength(1);
  });

  it("runs a dry-run plan then executes on confirm", async () => {
    // mockResolvedValue (not -Once): the purge mutation's onSuccess
    // invalidates ["storage-health"], triggering a refetch this test must
    // also satisfy, or TanStack Query surfaces "Query data cannot be
    // undefined" and the page renders its error state instead.
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [
        {
          id: 1, name: "Scratch Mount", rootPath: "/mnt/scratch", tier: "TIER1_LOCAL_SCRATCH",
          readOnly: false, prunable: true, isActive: true, nodeCount: 1, totalBytes: 100, usedBytes: 50,
          freeBytes: 50, isDegraded: false,
        },
      ],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });
    vi.mocked(api.pruneCache).mockResolvedValueOnce({
      executed: false,
      candidates: [{ nodeId: 42, filePath: "/mnt/scratch/proxy.jpg", sizeBytes: 12345, purged: false }],
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: /purge cache/i }));
    await waitFor(() => expect(api.pruneCache).toHaveBeenCalledWith({ storageLocationId: 1 }));
    await waitFor(() => expect(screen.getByText(/1 file eligible/)).toBeInTheDocument());

    vi.mocked(api.pruneCache).mockResolvedValueOnce({
      executed: true,
      candidates: [{ nodeId: 42, filePath: "/mnt/scratch/proxy.jpg", sizeBytes: 12345, purged: true }],
    });
    await userEvent.click(screen.getByRole("button", { name: /confirm purge/i }));
    await waitFor(() =>
      expect(api.pruneCache).toHaveBeenCalledWith({ storageLocationId: 1, execute: true }),
    );
    await waitFor(() => expect(screen.getByText("Purged 1 file.")).toBeInTheDocument());
  });
});
