import { render, screen, waitFor, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import IngestJobsPage from "./IngestJobsPage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    listJobs: vi.fn(),
    cancelJob: vi.fn(),
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

describe("IngestJobsPage", () => {
  it("renders historical scan jobs and filter controls", async () => {
    vi.mocked(api.listJobs).mockResolvedValueOnce({
      jobs: [
        {
          id: 101,
          kind: "FULL_SCAN",
          state: "COMPLETED",
          filesSeen: 1200,
          filesHashed: 1200,
          filesFailed: 0,
          edgesCreated: 45,
        },
      ],
      total: 1,
    });

    renderWithClient(<IngestJobsPage />);

    await waitFor(() => {
      expect(screen.getByText("#101")).toBeInTheDocument();
    });

    expect(screen.getAllByText("FULL_SCAN").length).toBeGreaterThan(0);
    expect(screen.getAllByText("COMPLETED").length).toBeGreaterThan(0);
    expect(screen.getAllByText("1,200").length).toBeGreaterThan(0);
    expect(screen.getByRole("button", { name: "Previous page" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Next page" })).toBeInTheDocument();
  });

  it("renders WATCH jobs in CANCELLED state as STOPPED and RUNNING as ACTIVE", async () => {
    vi.mocked(api.listJobs).mockResolvedValueOnce({
      jobs: [
        {
          id: 17,
          kind: "WATCH",
          state: "RUNNING",
          filesSeen: 4,
          filesHashed: 4,
          filesFailed: 0,
          edgesCreated: 2,
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
      total: 2,
    });

    renderWithClient(<IngestJobsPage />);

    await waitFor(() => {
      expect(screen.getByText("#17")).toBeInTheDocument();
      expect(screen.getByText("#16")).toBeInTheDocument();
    });

    const table = screen.getByRole("table");
    expect(within(table).getByText("ACTIVE")).toBeInTheDocument();
    expect(within(table).getByText("STOPPED")).toBeInTheDocument();
    expect(within(table).queryByText("CANCELLED")).not.toBeInTheDocument();
  });

  it("updates filters when clicking quick preset buttons", async () => {
    vi.mocked(api.listJobs).mockResolvedValue({
      jobs: [],
      total: 0,
    });

    renderWithClient(<IngestJobsPage />);

    const user = (await import("@testing-library/user-event")).default.setup();

    // Click "Watchers" preset
    await user.click(screen.getByRole("button", { name: "Watchers" }));
    await waitFor(() => {
      expect(api.listJobs).toHaveBeenCalledWith(expect.objectContaining({ kind: "WATCH" }));
    });

    // Click "Full Scans" preset
    await user.click(screen.getByRole("button", { name: "Full Scans" }));
    await waitFor(() => {
      expect(api.listJobs).toHaveBeenCalledWith(expect.objectContaining({ kind: "FULL_SCAN" }));
    });

    // Click "Incremental" preset
    await user.click(screen.getByRole("button", { name: "Incremental" }));
    await waitFor(() => {
      expect(api.listJobs).toHaveBeenCalledWith(expect.objectContaining({ kind: "INCREMENTAL" }));
    });

    // Click "All Types" preset
    await user.click(screen.getByRole("button", { name: "All Types" }));
    await waitFor(() => {
      expect(api.listJobs).toHaveBeenCalledWith(expect.objectContaining({ kind: undefined }));
    });
  });

  it("cancels a running scan job when clicking Cancel", async () => {
    vi.mocked(api.listJobs).mockResolvedValueOnce({
      jobs: [
        {
          id: 42,
          kind: "FULL_SCAN",
          state: "RUNNING",
          filesSeen: 10,
          filesHashed: 5,
          filesFailed: 0,
          edgesCreated: 0,
        },
      ],
      total: 1,
    });
    vi.mocked(api.cancelJob).mockResolvedValueOnce({ ok: true });

    renderWithClient(<IngestJobsPage />);

    await waitFor(() => {
      expect(screen.getByText("#42")).toBeInTheDocument();
    });

    const cancelButton = screen.getByRole("button", { name: "Cancel" });
    expect(cancelButton).toBeInTheDocument();

    const user = (await import("@testing-library/user-event")).default.setup();
    await user.click(cancelButton);

    await waitFor(() => {
      expect(api.cancelJob).toHaveBeenCalledWith(42);
    });
  });
});
