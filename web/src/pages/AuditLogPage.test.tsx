import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import AuditLogPage from "./AuditLogPage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    listAudit: vi.fn(),
    me: vi.fn().mockResolvedValue({ isAdmin: true, kind: "user", name: "admin" }),
    listUsers: vi.fn().mockResolvedValue({
      users: [{ id: 7, username: "bob", authProvider: "local", externalUid: "bob", createdAt: 1, lastSeenAt: 1 }],
      total: 1,
    }),
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

  it("shows access restricted when user is not admin", async () => {
    vi.mocked(api.me).mockResolvedValueOnce({
      authenticated: true,
      isAdmin: false,
      kind: "user",
      name: "regular_user",
    });

    renderWithClient(<AuditLogPage />);

    await waitFor(() => {
      expect(screen.getByText(/Access Restricted/i)).toBeInTheDocument();
    });
    expect(screen.getByText(/Administrator privileges are required/i)).toBeInTheDocument();
    expect(api.listAudit).not.toHaveBeenCalled();
  });
});

describe("AuditLogPage: filters and loading", () => {
  it("shows a loading state, not an empty log, while /me is still loading", async () => {
    let resolveMe: (v: unknown) => void = () => {};
    vi.mocked(api.me).mockImplementationOnce(() => new Promise((r) => (resolveMe = r)) as never);
    vi.mocked(api.listAudit).mockResolvedValue({ entries: [], total: 0 });
    renderWithClient(<AuditLogPage />);

    expect(screen.getByText("Loading audit log…")).toBeInTheDocument();
    expect(screen.queryByText(/no audit entries/i)).not.toBeInTheDocument();

    resolveMe({ isAdmin: true, kind: "user", name: "admin", authenticated: true });
    expect(await screen.findByText(/no audit entries recorded/i)).toBeInTheDocument();
  });

  it("sends the actor and an inclusive date range to the server", async () => {
    vi.mocked(api.listAudit).mockResolvedValue({ entries: [], total: 0 });
    renderWithClient(<AuditLogPage />);

    await screen.findByRole("option", { name: "bob" });
    fireEvent.change(screen.getByLabelText("Actor"), { target: { value: "7" } });
    fireEvent.change(screen.getByLabelText("From"), { target: { value: "2026-03-01" } });
    fireEvent.change(screen.getByLabelText("To"), { target: { value: "2026-03-02" } });

    const since = Math.floor(new Date(2026, 2, 1).getTime() / 1000);
    const until = Math.floor(new Date(2026, 2, 3).getTime() / 1000);
    await waitFor(() =>
      expect(api.listAudit).toHaveBeenLastCalledWith(
        expect.objectContaining({ actorUserId: 7, sinceUnix: since, untilUnix: until }),
      ),
    );
  });

  it("flags a reversed date range instead of querying it", async () => {
    vi.mocked(api.listAudit).mockResolvedValue({ entries: [], total: 0 });
    renderWithClient(<AuditLogPage />);
    await screen.findByText(/no audit entries recorded/i);
    vi.mocked(api.listAudit).mockClear();

    fireEvent.change(screen.getByLabelText("From"), { target: { value: "2026-03-05" } });
    fireEvent.change(screen.getByLabelText("To"), { target: { value: "2026-03-01" } });

    expect(await screen.findByRole("alert")).toHaveTextContent(/must not be before/);
    expect(api.listAudit).not.toHaveBeenCalledWith(expect.objectContaining({ untilUnix: expect.any(Number) }));
  });

  it("shows only the range alert while the range is invalid, not results for an unsent query", async () => {
    vi.mocked(api.listAudit).mockResolvedValue({
      entries: [
        {
          id: 1,
          source: "actor_audit",
          actorKind: "user",
          actorName: "alice",
          event: "settings.updated",
          resourceType: "app_setting",
          resourceId: "metadata.autoInherit",
          detailsJson: "{}",
          createdAt: 1700000000,
        },
      ],
      total: 1,
    });
    renderWithClient(<AuditLogPage />);
    await screen.findByText("settings.updated");

    // An empty result for a valid range is still reported as such.
    vi.mocked(api.listAudit).mockResolvedValue({ entries: [], total: 0 });
    fireEvent.change(screen.getByLabelText("From"), { target: { value: "2026-03-05" } });
    expect(await screen.findByText("No audit entries match the specified filters.")).toBeInTheDocument();
    vi.mocked(api.listAudit).mockClear();

    fireEvent.change(screen.getByLabelText("To"), { target: { value: "2026-03-01" } });

    expect(await screen.findByRole("alert")).toHaveTextContent(/must not be before/);
    expect(screen.queryByText(/No audit entries/)).not.toBeInTheDocument();
    expect(screen.queryByText(/Loading audit log/)).not.toBeInTheDocument();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
    expect(screen.queryByText(/records$/)).not.toBeInTheDocument();
    expect(api.listAudit).not.toHaveBeenCalled();

    // Fixing the range brings the results back.
    fireEvent.change(screen.getByLabelText("To"), { target: { value: "2026-03-09" } });
    expect(await screen.findByText("No audit entries match the specified filters.")).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("applies filters to the login stream, without the resource type", async () => {
    vi.mocked(api.listAudit).mockResolvedValue({ entries: [], total: 0 });
    renderWithClient(<AuditLogPage />);
    await screen.findByText(/no audit entries recorded/i);

    fireEvent.change(screen.getByLabelText(/filter resource type/i), { target: { value: "asset" } });
    fireEvent.change(screen.getByLabelText("Log Stream"), { target: { value: "login" } });
    expect(screen.queryByLabelText(/filter resource type/i)).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(/filter event/i), { target: { value: "bad-password" } });

    await waitFor(() =>
      expect(api.listAudit).toHaveBeenLastCalledWith(
        expect.objectContaining({ type: "login", event: "bad-password", resourceType: undefined }),
      ),
    );
  });
});
