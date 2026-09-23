import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import UsersPage from "./UsersPage";
import type { AttributionUser, ListUsersResponse, Me } from "../api/types";

// Mock the api client at the module level (same pattern as
// CompanionPairingsPage/AssetListPage tests) so the real
// hooks/queries.ts useMutation instances run. That exercises the actual
// error / pending / reset semantics ConfirmDialog is wired to, instead
// of stubbing the hook layer.
const meMock = vi.fn<() => Promise<Me>>();
const listUsersMock = vi.fn<(params?: {
  limit?: number;
  offset?: number;
}) => Promise<ListUsersResponse>>();
const createUserMock = vi.fn<(input: unknown) => Promise<unknown>>();
const adminResetPasswordMock = vi.fn<(userId: number) => Promise<unknown>>();
const disableUserMock = vi.fn<(userId: number) => Promise<unknown>>();
const enableUserMock = vi.fn<(userId: number) => Promise<unknown>>();
const updateUserMock = vi.fn<(userId: number, input: unknown) => Promise<unknown>>();
const revokeUserSessionsMock = vi.fn<(userId: number) => Promise<unknown>>();

vi.mock("../api/client", () => ({
  api: {
    me: () => meMock(),
    listUsers: (params?: { limit?: number; offset?: number }) => listUsersMock(params),
    createUser: (input: unknown) => createUserMock(input),
    adminResetPassword: (userId: number) => adminResetPasswordMock(userId),
    disableUser: (userId: number) => disableUserMock(userId),
    enableUser: (userId: number) => enableUserMock(userId),
    updateUser: (userId: number, input: unknown) => updateUserMock(userId, input),
    revokeUserSessions: (userId: number) => revokeUserSessionsMock(userId),
  },
  ApiError: class ApiError extends Error {
    status: number;
    constructor(status: number, message: string) {
      super(message);
      this.status = status;
      this.name = "ApiError";
    }
  },
}));

function makeUser(overrides: Partial<AttributionUser> = {}): AttributionUser {
  return {
    id: 1,
    username: "alice",
    email: "alice@example.com",
    source: "local",
    authProvider: "local",
    externalUid: "",
    isAdmin: false,
    mfaEnabled: false,
    disabledAt: undefined,
    createdAt: 1700000000,
    lastSeenAt: 1700001000,
    ...overrides,
  };
}

function renderPage() {
  const qc = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <UsersPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function stubUsers(users: AttributionUser[]) {
  listUsersMock.mockResolvedValue({ users, total: users.length });
}

function stubSelf() {
  meMock.mockResolvedValue({
    kind: "user",
    authenticated: true,
    isAdmin: true,
    isLocal: true,
    localUserId: 1,
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  stubSelf();
  listUsersMock.mockResolvedValue({ users: [], total: 0 });
  createUserMock.mockResolvedValue({ user: makeUser() });
  adminResetPasswordMock.mockResolvedValue({
    user: { id: 2, username: "bob", isAdmin: false, source: "local", createdAt: 0, createdBy: "x" },
    newPassword: "temp-password",
    shownOnceNotice: "Copy now",
  });
  disableUserMock.mockResolvedValue({ ok: true, id: 2, disabledAt: 1700002000 });
  enableUserMock.mockResolvedValue({ ok: true, id: 2 });
  updateUserMock.mockResolvedValue({ ok: true, user: makeUser({ id: 2, username: "bob" }) });
  revokeUserSessionsMock.mockResolvedValue({ ok: true, id: 2, revokedCount: 1 });
});

afterEach(() => {
  // Restore window.alert/confirm spies even when a mid-test assertion fails,
  // so a leaked stub cannot bleed into later tests (clearAllMocks does not
  // restore implementations).
  vi.restoreAllMocks();
});

describe("UsersPage row actions", () => {
  it("shows Re-enable button for disabled non-self users", async () => {
    stubUsers([makeUser({ id: 2, username: "bob", disabledAt: 1700002000 })]);

    renderPage();
    expect(await screen.findByRole("button", { name: /re-enable/i })).toBeInTheDocument();
  });

  it("does NOT show Re-enable for self", async () => {
    stubUsers([makeUser({ id: 1, username: "alice", disabledAt: 1700002000 })]);

    renderPage();
    await screen.findByRole("heading", { name: /user management/i });
    expect(screen.queryByRole("button", { name: /re-enable/i })).not.toBeInTheDocument();
  });

  it("shows Make Admin for non-admin non-self users", async () => {
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    expect(await screen.findByRole("button", { name: /make admin/i })).toBeInTheDocument();
  });

  it("shows Remove Admin for admin non-self users", async () => {
    stubUsers([makeUser({ id: 2, username: "bob", isAdmin: true })]);

    renderPage();
    expect(await screen.findByRole("button", { name: /remove admin/i })).toBeInTheDocument();
  });

  it("does NOT show admin toggles for self", async () => {
    stubUsers([makeUser({ id: 1, username: "alice" })]);

    renderPage();
    await screen.findByText("alice");
    expect(screen.queryByRole("button", { name: /make admin/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /remove admin/i })).not.toBeInTheDocument();
  });

  it("shows Revoke Sessions for non-self users", async () => {
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    expect(await screen.findByRole("button", { name: /revoke sessions/i })).toBeInTheDocument();
  });

  it("does NOT show Revoke Sessions for self", async () => {
    stubUsers([makeUser({ id: 1, username: "alice" })]);

    renderPage();
    await screen.findByText("alice");
    expect(screen.queryByRole("button", { name: /revoke sessions/i })).not.toBeInTheDocument();
  });

  it("does NOT show admin buttons for system user", async () => {
    stubUsers([makeUser({ id: 999, username: "system", authProvider: "system" })]);

    renderPage();
    expect(await screen.findByText(/ID:\s*999/)).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /make admin/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /revoke sessions/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /disable/i })).not.toBeInTheDocument();
  });

  it("toggle-admin cancel opens ConfirmDialog and skips mutation", async () => {
    const user = userEvent.setup();
    const confirmSpy = vi.spyOn(window, "confirm");
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /make admin/i }));

    const dialog = screen.getByRole("dialog", { name: /grant admin privileges/i });
    expect(dialog).toBeInTheDocument();
    expect(dialog).toHaveTextContent(/Grant admin privileges to\s*bob\?/);
    expect(confirmSpy).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(
      screen.queryByRole("dialog", { name: /grant admin privileges/i }),
    ).not.toBeInTheDocument();
    expect(updateUserMock).not.toHaveBeenCalled();
  });

  it("toggle-admin confirm calls updateUser and closes on success", async () => {
    const user = userEvent.setup();
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /make admin/i }));
    await user.click(screen.getByRole("button", { name: /confirm grant admin/i }));

    expect(updateUserMock).toHaveBeenCalledWith(2, { isAdmin: true });
    await waitFor(() => {
      expect(
        screen.queryByRole("dialog", { name: /grant admin privileges/i }),
      ).not.toBeInTheDocument();
    });
  });

  it("remove-admin confirm calls updateUser with admin revoked and closes", async () => {
    const user = userEvent.setup();
    stubUsers([makeUser({ id: 2, username: "bob", isAdmin: true })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /remove admin/i }));

    expect(
      screen.getByRole("dialog", { name: /remove admin privileges/i }),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /confirm remove admin/i }));
    expect(updateUserMock).toHaveBeenCalledWith(2, { isAdmin: false });
    await waitFor(() => {
      expect(
        screen.queryByRole("dialog", { name: /remove admin privileges/i }),
      ).not.toBeInTheDocument();
    });
  });

  it("admin dialog surfaces a failed mutation and stays open", async () => {
    const user = userEvent.setup();
    updateUserMock.mockRejectedValue(new Error("boom"));
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /make admin/i }));
    await user.click(screen.getByRole("button", { name: /confirm grant admin/i }));

    const dialog = await screen.findByRole("dialog", { name: /grant admin privileges/i });
    await waitFor(() => {
      expect(dialog).toHaveTextContent(/Failed to update user: Error: boom/);
    });
  });

  it("opens ConfirmDialog for disable instead of window.confirm", async () => {
    const user = userEvent.setup();
    const confirmSpy = vi.spyOn(window, "confirm");
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /^disable$/i }));

    expect(screen.getByRole("dialog", { name: /disable user/i })).toBeInTheDocument();
    expect(confirmSpy).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(disableUserMock).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /^disable$/i }));
    await user.click(screen.getByRole("button", { name: /confirm disable/i }));
    expect(disableUserMock).toHaveBeenCalledWith(2);
    await waitFor(() => {
      expect(
        screen.queryByRole("dialog", { name: /disable user/i }),
      ).not.toBeInTheDocument();
    });
  });

  it("disable dialog surfaces a failed mutation and clears the error on reopen", async () => {
    const user = userEvent.setup();
    disableUserMock.mockRejectedValueOnce(new Error("boom"));
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /^disable$/i }));
    await user.click(screen.getByRole("button", { name: /confirm disable/i }));

    const dialog = await screen.findByRole("dialog", { name: /disable user/i });
    await waitFor(() => {
      expect(dialog).toHaveTextContent(/Failed to disable user: Error: boom/);
    });

    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(
      screen.queryByRole("dialog", { name: /disable user/i }),
    ).not.toBeInTheDocument();

    // Reopen: reset-on-open must clear the stale error.
    await user.click(screen.getByRole("button", { name: /^disable$/i }));
    const reopened = screen.getByRole("dialog", { name: /disable user/i });
    expect(reopened).not.toHaveTextContent(/Failed to disable user/i);
  });

  it("disable dialog stays open with pending label while the request is in flight", async () => {
    const user = userEvent.setup();
    disableUserMock.mockReturnValue(new Promise(() => {}));
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /^disable$/i }));
    await user.click(screen.getByRole("button", { name: /confirm disable/i }));

    const dialog = await screen.findByRole("dialog", { name: /disable user/i });
    await waitFor(() => {
      expect(screen.getByRole("button", { name: /disabling…/i })).toBeDisabled();
    });
    expect(screen.getByRole("button", { name: /^cancel$/i })).toBeDisabled();
    expect(dialog).toBeInTheDocument();
  });

  it("opens ConfirmDialog for revoke sessions instead of window.confirm", async () => {
    const user = userEvent.setup();
    const confirmSpy = vi.spyOn(window, "confirm");
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /revoke sessions/i }));

    expect(screen.getByRole("dialog", { name: /revoke sessions/i })).toBeInTheDocument();
    expect(screen.getByText(/logged out everywhere/i)).toBeInTheDocument();
    expect(confirmSpy).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /confirm revoke/i }));
    expect(revokeUserSessionsMock).toHaveBeenCalledWith(2);
    await waitFor(() => {
      expect(
        screen.queryByRole("dialog", { name: /revoke sessions/i }),
      ).not.toBeInTheDocument();
    });
    expect(await screen.findByRole("status")).toHaveTextContent("Revoked 1 active session.");
  });

  it("revoke success notice does not call window.alert and is dismissible", async () => {
    const user = userEvent.setup();
    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /revoke sessions/i }));
    await user.click(screen.getByRole("button", { name: /confirm revoke/i }));

    const notice = await screen.findByRole("status");
    expect(notice).toHaveTextContent("Revoked 1 active session.");
    expect(alertSpy).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /dismiss notice/i }));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("revoke dialog surfaces a failed mutation and stays open without a notice", async () => {
    const user = userEvent.setup();
    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});
    revokeUserSessionsMock.mockRejectedValue(new Error("nope"));
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /revoke sessions/i }));
    await user.click(screen.getByRole("button", { name: /confirm revoke/i }));

    const dialog = await screen.findByRole("dialog", { name: /revoke sessions/i });
    await waitFor(() => {
      expect(dialog).toHaveTextContent(/Failed to revoke sessions: Error: nope/);
    });
    expect(alertSpy).not.toHaveBeenCalled();
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("re-enable failure shows a sticky inline error notice instead of alert", async () => {
    const user = userEvent.setup();
    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});
    enableUserMock.mockRejectedValue(new Error("boom"));
    stubUsers([makeUser({ id: 2, username: "bob", disabledAt: 1700002000 })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /re-enable/i }));

    const notice = await screen.findByRole("alert");
    expect(notice).toHaveTextContent("Failed to re-enable bob: boom");
    expect(alertSpy).not.toHaveBeenCalled();

    // Opening another row-action dialog must not clear a sticky error.
    await user.click(screen.getByRole("button", { name: /revoke sessions/i }));
    expect(screen.getByRole("alert")).toHaveTextContent("Failed to re-enable bob: boom");
    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
  });

  it("opening a row-action dialog clears a success notice", async () => {
    const user = userEvent.setup();
    stubUsers([makeUser({ id: 2, username: "bob" })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /revoke sessions/i }));
    await user.click(screen.getByRole("button", { name: /confirm revoke/i }));
    await screen.findByRole("status");

    await user.click(screen.getByRole("button", { name: /disable/i }));
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("re-enable attempt clears any previous notice", async () => {
    const user = userEvent.setup();
    enableUserMock.mockRejectedValueOnce(new Error("boom"));
    stubUsers([makeUser({ id: 2, username: "bob", disabledAt: 1700002000 })]);

    renderPage();
    await user.click(await screen.findByRole("button", { name: /re-enable/i }));
    await screen.findByRole("alert");

    // A new attempt clears the stale error before showing this attempt's outcome.
    await user.click(screen.getByRole("button", { name: /re-enable/i }));
    await waitFor(() => {
      expect(screen.queryByRole("alert")).not.toBeInTheDocument();
      expect(screen.queryByRole("status")).not.toBeInTheDocument();
    });
    expect(enableUserMock).toHaveBeenCalledTimes(2);
  });
});
