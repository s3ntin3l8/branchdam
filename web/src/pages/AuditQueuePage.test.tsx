import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import AuditQueuePage from "./AuditQueuePage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    listAuditQueue: vi.fn(),
    confirmEdge: vi.fn(),
    rejectEdge: vi.fn(),
  },
}));

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>);
}

describe("AuditQueuePage", () => {
  it("shows an empty state when there is nothing to review", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [] });
    renderWithClient(<AuditQueuePage />);

    expect(await screen.findByText(/nothing needs review/i)).toBeInTheDocument();
  });

  it("renders queue entries and flags a missing parent", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 1,
          sourceNodeId: 10,
          targetNodeId: 20,
          relationshipType: "PROXY_OF",
          confidence: 0.6,
          resolver: "filename_stem",
          evidenceJson: "{}",
          parentAlive: false,
          parentMissing: true,
        },
      ],
    });
    renderWithClient(<AuditQueuePage />);

    expect(await screen.findByText("PROXY_OF")).toBeInTheDocument();
    expect(screen.getByText(/parent missing/i)).toBeInTheDocument();
    expect(screen.getByText(/confidence 0.60/)).toBeInTheDocument();
  });

  it("calls confirmEdge when Confirm is clicked", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 42,
          sourceNodeId: 1,
          targetNodeId: 2,
          relationshipType: "DERIVED_FROM",
          confidence: 0.75,
          resolver: "filename_stem",
          evidenceJson: "{}",
          parentAlive: true,
          parentMissing: false,
        },
      ],
    });
    vi.mocked(api.confirmEdge).mockResolvedValue({ ok: true });

    renderWithClient(<AuditQueuePage />);
    const button = await screen.findByRole("button", { name: /confirm/i });
    await userEvent.click(button);

    await waitFor(() => expect(api.confirmEdge).toHaveBeenCalledWith(42));
  });
});
