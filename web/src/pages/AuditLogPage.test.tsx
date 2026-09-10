import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import AuditLogPage from "./AuditLogPage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    listAudit: vi.fn(),
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

describe("AuditLogPage", () => {
  it("renders audit entries and handles details expansion", async () => {
    vi.mocked(api.listAudit).mockResolvedValueOnce({
      entries: [
        {
          id: 1,
          source: "actor_audit",
          actorKind: "user",
          actorName: "alice",
          event: "settings.updated",
          resourceType: "app_setting",
          resourceId: "metadata.autoInherit",
          detailsJson: JSON.stringify({ key: "metadata.autoInherit", value: true }),
          createdAt: 1700000000,
        },
      ],
      total: 1,
    });

    renderWithClient(<AuditLogPage />);

    await waitFor(() => {
      expect(screen.getByText("settings.updated")).toBeInTheDocument();
    });

    expect(screen.getByText("alice")).toBeInTheDocument();
    expect(screen.getByText("app_setting:")).toBeInTheDocument();
    expect(screen.getByText("metadata.autoInherit")).toBeInTheDocument();

    // Click "View Details"
    const viewDetailsBtn = screen.getByRole("button", { name: /view details/i });
    await userEvent.click(viewDetailsBtn);

    expect(screen.getByText(/Hide Details/i)).toBeInTheDocument();
  });

  it("shows empty state when no entries returned", async () => {
    vi.mocked(api.listAudit).mockResolvedValueOnce({
      entries: [],
      total: 0,
    });

    renderWithClient(<AuditLogPage />);

    await waitFor(() => {
      expect(screen.getByText(/no audit entries recorded/i)).toBeInTheDocument();
    });
  });

  it("queries with event and resourceType server-side filters", async () => {
    vi.mocked(api.listAudit).mockResolvedValue({
      entries: [],
      total: 0,
    });

    renderWithClient(<AuditLogPage />);

    const eventInput = screen.getByLabelText(/filter event/i);
    await userEvent.type(eventInput, "scan.started");

    await waitFor(() => {
      expect(api.listAudit).toHaveBeenCalledWith(
        expect.objectContaining({ event: "scan.started" })
      );
    });
  });
});
