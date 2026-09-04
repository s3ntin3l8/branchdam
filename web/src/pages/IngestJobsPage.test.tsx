import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import IngestJobsPage from "./IngestJobsPage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    listJobs: vi.fn(),
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
});
