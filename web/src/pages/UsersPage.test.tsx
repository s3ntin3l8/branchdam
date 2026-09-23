import { render, screen, waitFor } from "@testing-library/react";
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

function makeMutation(
  overrides: Partial<{
    mutate: ReturnType<typeof vi.fn>;
    mutateAsync: ReturnType<typeof vi.fn>;
    reset: ReturnType<typeof vi.fn>;
    isPending: boolean;
    isError: boolean;
    error: unknown;
  }> = {},
) {
  const mutation = {
    mutate: vi.fn(
      (
        _vars: unknown,
        options?: { onSuccess?: (data?: unknown) => void },
      ) => {
        options?.onSuccess?.();
      },
    ),
    mutateAsync: vi.fn(),
    reset: vi.fn(),
    isPending: false,
    isError: false,
    error: null,
    ...overrides,
  };
  if (!overrides.reset) {
    // Mirror the real useMutation reset: clear error state so the
    // reopen path (reset on dialog open) can be asserted.
    mutation.reset = vi.fn(() => {
      mutation.isError = false;
      mutation.error = null;
    });
  }
  return mutation;
}

function stubMutations() {
  const create = makeMutation();
  const resetPassword = makeMutation();
  const disable = makeMutation();
  const enable = makeMutation();
  const update = makeMutation();
  const revoke = makeMutation({
    mutate: vi.fn(
      (
        id: number,
        options?: {
          onSuccess?: (res: { ok: boolean; id: number; revokedCount: number }) => void;
        },
      ) => {
        options?.onSuccess?.({ ok: true, id, revokedCount: 1 });
      },
    ),
    mutateAsync: vi.fn().mockResolvedValue({ ok: true, id: 1, revokedCount: 1 }),
  });

  (useCreateUser as ReturnType<typeof vi.fn>).mockReturnValue(create);
  (useAdminResetPassword as ReturnType<typeof vi.fn>).mockReturnValue(resetPassword);
  (useDisableUser as ReturnType<typeof vi.fn>).mockReturnValue(disable);
  (useEnableUser as ReturnType<typeof vi.fn>).mockReturnValue(enable);
  (useUpdateUser as ReturnType<typeof vi.fn>).mockReturnValue(update);
  (useRevokeUserSessions as ReturnType<typeof vi.fn>).mockReturnValue(revoke);
  return { create, resetPassword, disable, enable, update, revoke };
}

function stubUsers(users: ReturnType<typeof makeUser>[]) {
  (useUsers as ReturnType<typeof vi.fn>).mockReturnValue({
    data: { users, total: users.length },
    isLoading: false,
  });
}

function stubSelf() {
  (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: { localUserId: 1, isAdmin: true } });
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

  it("toggle-admin cancel opens ConfirmDialog and skips mutation", async () => {
    const user = userEvent.setup();
    const { update } = stubMutations();
    const confirmSpy = vi.spyOn(window, "confirm");

    const other = makeUser({ id: 2, username: "bob" });
    stubSelf();
    stubUsers([other]);

    renderPage();
    await user.click(screen.getByRole("button", { name: /make admin/i }));

    const dialog = screen.getByRole("dialog", { name: /grant admin privileges/i });
    expect(dialog).toBeInTheDocument();
    expect(dialog).toHaveTextContent(/Grant admin privileges to\s*bob\?/);
    expect(confirmSpy).not.toHaveBeenCalled();
    expect(update.reset).toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(
      screen.queryByRole("dialog", { name: /grant admin privileges/i }),
    ).not.toBeInTheDocument();
    expect(update.mutate).not.toHaveBeenCalled();
  });

  it("toggle-admin confirm calls updateUser with granted admin", async () => {
    const user = userEvent.setup();
    const { update } = stubMutations();

    const other = makeUser({ id: 2, username: "bob" });
    stubSelf();
    stubUsers([other]);

    renderPage();
    await user.click(screen.getByRole("button", { name: /make admin/i }));
    await user.click(screen.getByRole("button", { name: /confirm grant admin/i }));

    expect(update.mutate).toHaveBeenCalledWith(
      { userId: 2, input: { isAdmin: true } },
      expect.objectContaining({ onSuccess: expect.any(Function) }),
    );
    await waitFor(() => {
      expect(
        screen.queryByRole("dialog", { name: /grant admin privileges/i }),
      ).not.toBeInTheDocument();
    });
  });

  it("remove-admin confirm calls updateUser with admin revoked", async () => {
    const user = userEvent.setup();
    const { update } = stubMutations();

    const admin = makeUser({ id: 2, username: "bob", isAdmin: true });
    stubSelf();
    stubUsers([admin]);

    renderPage();
    await user.click(screen.getByRole("button", { name: /remove admin/i }));

    expect(
      screen.getByRole("dialog", { name: /remove admin privileges/i }),
    ).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /confirm remove admin/i }));
    expect(update.mutate).toHaveBeenCalledWith(
      { userId: 2, input: { isAdmin: false } },
      expect.objectContaining({ onSuccess: expect.any(Function) }),
    );
    await waitFor(() => {
      expect(
        screen.queryByRole("dialog", { name: /remove admin privileges/i }),
      ).not.toBeInTheDocument();
    });
  });

  it("opens ConfirmDialog for disable instead of window.confirm", async () => {
    const user = userEvent.setup();
    const { disable } = stubMutations();
    const confirmSpy = vi.spyOn(window, "confirm");

    const other = makeUser({ id: 2, username: "bob" });
    stubSelf();
    stubUsers([other]);

    renderPage();
    await user.click(screen.getByRole("button", { name: /^disable$/i }));

    expect(screen.getByRole("dialog", { name: /disable user/i })).toBeInTheDocument();
    expect(confirmSpy).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(disable.mutate).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /^disable$/i }));
    await user.click(screen.getByRole("button", { name: /confirm disable/i }));
    expect(disable.mutate).toHaveBeenCalledWith(
      2,
      expect.objectContaining({ onSuccess: expect.any(Function) }),
    );
    await waitFor(() => {
      expect(
        screen.queryByRole("dialog", { name: /disable user/i }),
      ).not.toBeInTheDocument();
    });
  });

  it("opens ConfirmDialog for revoke sessions instead of window.confirm", async () => {
    const user = userEvent.setup();
    const { revoke } = stubMutations();
    const confirmSpy = vi.spyOn(window, "confirm");
    const alertSpy = vi.spyOn(window, "alert").mockImplementation(() => {});

    const other = makeUser({ id: 2, username: "bob" });
    stubSelf();
    stubUsers([other]);

    renderPage();
    await user.click(screen.getByRole("button", { name: /revoke sessions/i }));

    expect(screen.getByRole("dialog", { name: /revoke sessions/i })).toBeInTheDocument();
    expect(
      screen.getByText(/logged out everywhere/i),
    ).toBeInTheDocument();
    expect(confirmSpy).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: /confirm revoke/i }));
    expect(revoke.mutate).toHaveBeenCalledWith(
      2,
      expect.objectContaining({ onSuccess: expect.any(Function) }),
    );
    expect(alertSpy).toHaveBeenCalledWith("Revoked 1 active session.");
    await waitFor(() => {
      expect(
        screen.queryByRole("dialog", { name: /revoke sessions/i }),
      ).not.toBeInTheDocument();
    });
  });

  it("disable dialog surfaces a failed mutation and clears the error on reopen", async () => {
    const user = userEvent.setup();
    const { disable } = stubMutations();
    // Simulate useMutation's failure path: mutate rejects, dialog stays
    // open, and the mutation's error state feeds ConfirmDialog. The
    // mocked hook doesn't subscribe like useMutation, so nudge a
    // re-render via the search box after the failure.
    disable.mutate.mockImplementation(() => {
      disable.isError = true;
      disable.error = new Error("boom");
    });

    const other = makeUser({ id: 2, username: "bob" });
    stubSelf();
    stubUsers([other]);

    renderPage();
    await user.click(screen.getByRole("button", { name: /^disable$/i }));
    await user.click(screen.getByRole("button", { name: /confirm disable/i }));

    const search = screen.getByPlaceholderText(/search users/i);
    await user.type(search, "x");

    const dialog = screen.getByRole("dialog", { name: /disable user/i });
    expect(dialog).toBeInTheDocument();
    expect(dialog).toHaveTextContent(/Failed to disable user: Error: boom/);

    await user.click(screen.getByRole("button", { name: /^cancel$/i }));
    expect(
      screen.queryByRole("dialog", { name: /disable user/i }),
    ).not.toBeInTheDocument();

    // Reopen: reset-on-open must clear the stale error.
    await user.click(screen.getByRole("button", { name: /^disable$/i }));
    const reopened = screen.getByRole("dialog", { name: /disable user/i });
    expect(reopened).not.toHaveTextContent(/Failed to disable user/i);
    expect(disable.reset).toHaveBeenCalledTimes(2);
  });

  it("disable dialog shows pending label and disables buttons while pending", async () => {
    const user = userEvent.setup();
    const { disable } = stubMutations();
    disable.isPending = true;

    const other = makeUser({ id: 2, username: "bob" });
    stubSelf();
    stubUsers([other]);

    renderPage();
    await user.click(screen.getByRole("button", { name: /^disable$/i }));

    expect(screen.getByRole("button", { name: /disabling…/i })).toBeDisabled();
    expect(screen.getByRole("button", { name: /^cancel$/i })).toBeDisabled();
  });
});
