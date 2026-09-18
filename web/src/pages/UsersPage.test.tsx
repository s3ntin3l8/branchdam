import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { describe, it, expect, vi, beforeEach } from "vitest";
import UsersPage from "./UsersPage";

vi.mock("../hooks/queries", () => ({
  useMe: vi.fn(),
  useUsers: vi.fn(),
  useCreateUser: vi.fn(),
  useAdminResetPassword: vi.fn(),
  useDisableUser: vi.fn(),
  useEnableUser: vi.fn(),
  useUpdateUser: vi.fn(),
  useRevokeUserSessions: vi.fn(),
}));

import {
  useMe,
  useUsers,
  useCreateUser,
  useAdminResetPassword,
  useDisableUser,
  useEnableUser,
  useUpdateUser,
  useRevokeUserSessions,
} from "../hooks/queries";

function makeUser(overrides: Record<string, unknown> = {}) {
  return {
    id: 1,
    username: "alice",
    email: "alice@example.com",
    source: "local",
    authProvider: "local",
    isAdmin: false,
    mfaEnabled: false,
    disabledAt: undefined,
    createdAt: 1700000000,
    lastSeenAt: 1700001000,
    ...overrides,
  };
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <UsersPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

function stubMutations() {
  const mutateAsync = vi.fn();
  (useCreateUser as ReturnType<typeof vi.fn>).mockReturnValue({ mutateAsync, isPending: false });
  (useAdminResetPassword as ReturnType<typeof vi.fn>).mockReturnValue({ mutateAsync, isPending: false });
  (useDisableUser as ReturnType<typeof vi.fn>).mockReturnValue({ mutateAsync, isPending: false });
  (useEnableUser as ReturnType<typeof vi.fn>).mockReturnValue({ mutateAsync, isPending: false });
  (useUpdateUser as ReturnType<typeof vi.fn>).mockReturnValue({ mutateAsync, isPending: false });
  (useRevokeUserSessions as ReturnType<typeof vi.fn>).mockReturnValue({
    mutateAsync: vi.fn().mockResolvedValue({ revokedCount: 1 }),
    isPending: false,
  });
  return { mutateAsync };
}

function stubUsers(users: ReturnType<typeof makeUser>[]) {
  (useUsers as ReturnType<typeof vi.fn>).mockReturnValue({
    data: { users, total: users.length },
    isLoading: false,
  });
}

beforeEach(() => {
  vi.clearAllMocks();
  stubMutations();
});

describe("UsersPage row actions", () => {
  it("shows Re-enable button for disabled non-self users", () => {
    const disabled = makeUser({ id: 2, username: "bob", disabledAt: "2026-01-01T00:00:00Z" });
    (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: { localUserId: 1, isAdmin: true } });
    stubUsers([disabled]);

    renderPage();
    expect(screen.getByRole("button", { name: /re-enable/i })).toBeInTheDocument();
  });

  it("does NOT show Re-enable for self", () => {
    const self = makeUser({ id: 1, username: "alice", disabledAt: "2026-01-01T00:00:00Z" });
    (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: { localUserId: 1, isAdmin: true } });
    stubUsers([self]);

    renderPage();
    expect(screen.queryByRole("button", { name: /re-enable/i })).not.toBeInTheDocument();
  });

  it("shows Make Admin for non-admin non-self users", () => {
    const other = makeUser({ id: 2, username: "bob" });
    (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: { localUserId: 1, isAdmin: true } });
    stubUsers([other]);

    renderPage();
    expect(screen.getByRole("button", { name: /make admin/i })).toBeInTheDocument();
  });

  it("shows Remove Admin for admin non-self users", () => {
    const admin = makeUser({ id: 2, username: "bob", isAdmin: true });
    (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: { localUserId: 1, isAdmin: true } });
    stubUsers([admin]);

    renderPage();
    expect(screen.getByRole("button", { name: /remove admin/i })).toBeInTheDocument();
  });

  it("does NOT show admin toggles for self", () => {
    const self = makeUser({ id: 1, username: "alice" });
    (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: { localUserId: 1, isAdmin: true } });
    stubUsers([self]);

    renderPage();
    expect(screen.queryByRole("button", { name: /make admin/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /remove admin/i })).not.toBeInTheDocument();
  });

  it("shows Revoke Sessions for non-self users", () => {
    const other = makeUser({ id: 2, username: "bob" });
    (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: { localUserId: 1, isAdmin: true } });
    stubUsers([other]);

    renderPage();
    expect(screen.getByRole("button", { name: /revoke sessions/i })).toBeInTheDocument();
  });

  it("does NOT show Revoke Sessions for self", () => {
    const self = makeUser({ id: 1, username: "alice" });
    (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: { localUserId: 1, isAdmin: true } });
    stubUsers([self]);

    renderPage();
    expect(screen.queryByRole("button", { name: /revoke sessions/i })).not.toBeInTheDocument();
  });

  it("does NOT show admin buttons for system user", () => {
    const system = makeUser({ id: 999, username: "system", authProvider: "system" });
    (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: { localUserId: 1, isAdmin: true } });
    stubUsers([system]);

    renderPage();
    expect(screen.queryByRole("button", { name: /make admin/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /revoke sessions/i })).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /disable/i })).not.toBeInTheDocument();
  });

  it("confirm-cancel on handleToggleAdmin skips mutation", async () => {
    const user = userEvent.setup();
    const { mutateAsync } = stubMutations();
    (useUpdateUser as ReturnType<typeof vi.fn>).mockReturnValue({ mutateAsync, isPending: false });

    const other = makeUser({ id: 2, username: "bob" });
    (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: { localUserId: 1, isAdmin: true } });
    stubUsers([other]);

    vi.spyOn(window, "confirm").mockReturnValue(false);

    renderPage();
    await user.click(screen.getByRole("button", { name: /make admin/i }));

    expect(window.confirm).toHaveBeenCalled();
    expect(mutateAsync).not.toHaveBeenCalled();
  });

  it("confirm-accept on handleToggleAdmin calls updateUser", async () => {
    const user = userEvent.setup();
    const { mutateAsync } = stubMutations();
    (useUpdateUser as ReturnType<typeof vi.fn>).mockReturnValue({ mutateAsync, isPending: false });

    const other = makeUser({ id: 2, username: "bob" });
    (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: { localUserId: 1, isAdmin: true } });
    stubUsers([other]);

    vi.spyOn(window, "confirm").mockReturnValue(true);

    renderPage();
    await user.click(screen.getByRole("button", { name: /make admin/i }));

    expect(mutateAsync).toHaveBeenCalledWith({ userId: 2, input: { isAdmin: true } });
  });
});
