import { useEffect, useMemo, useRef, useState } from "react";
import { useNavigate } from "react-router";
import {
  useAdminResetPassword,
  useCreateUser,
  useDisableUser,
  useEnableUser,
  useMe,
  useRevokeUserSessions,
  useUpdateUser,
  useUsers,
} from "../hooks/queries";
import { ApiError } from "../api/client";
import type { AttributionUser } from "../api/types";
import ConfirmDialog from "../components/ConfirmDialog";
import InlineNotice from "../components/InlineNotice";

function formatUnixTime(unix: number | undefined): string {
  if (!unix) return "—";
  return new Date(unix * 1000).toLocaleString();
}

interface ModalProps {
  open: boolean;
  onClose: () => void;
  title: string;
  children: React.ReactNode;
}

function Modal({ open, onClose, title, children }: ModalProps) {
  useEffect(() => {
    if (!open) return;
    const handleKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") onClose();
    };
    window.addEventListener("keydown", handleKeyDown);
    return () => window.removeEventListener("keydown", handleKeyDown);
  }, [open, onClose]);

  if (!open) return null;

  return (
    <div
      role="dialog"
      aria-modal="true"
      aria-labelledby="modal-title"
      className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 p-4"
      onClick={onClose}
    >
      <div
        className="w-full max-w-md rounded-lg border border-neutral-700 bg-neutral-900 p-6 shadow-xl"
        onClick={(e) => e.stopPropagation()}
      >
        <h2 id="modal-title" className="text-lg font-semibold text-neutral-100 mb-4">
          {title}
        </h2>
        {children}
      </div>
    </div>
  );
}

export default function UsersPage() {
  const [page, setPage] = useState(0);
  const [search, setSearch] = useState("");
  const navigate = useNavigate();
  const pageSize = 25;
  const { data: me } = useMe();
  const { data, isLoading, error } = useUsers({ limit: pageSize, offset: page * pageSize });

  const createUserMutation = useCreateUser();
  const resetPasswordMutation = useAdminResetPassword();
  const disableUserMutation = useDisableUser();
  const enableUserMutation = useEnableUser();
  const updateUserMutation = useUpdateUser();
  const revokeUserSessionsMutation = useRevokeUserSessions();

  // ConfirmDialog targets: replace the three window.confirm gates
  // (disable, toggle admin, revoke sessions) -- issue #478. newAdmin is
  // captured at click time so a list refetch can't flip the intent
  // while the dialog is open.
  const [disableTarget, setDisableTarget] = useState<AttributionUser | null>(null);
  const [adminTarget, setAdminTarget] = useState<{
    user: AttributionUser;
    newAdmin: boolean;
  } | null>(null);
  const [revokeSessionsTarget, setRevokeSessionsTarget] = useState<AttributionUser | null>(null);

  // Transient page-level notice replacing window.alert (issue #483):
  // revoke-sessions success and re-enable error both land here. The id keys
  // InlineNotice so re-showing a notice remounts it (fresh timer + scroll-
  // into-view) instead of inheriting the old countdown. Errors are sticky
  // (InlineNotice defaults autoDismissMs to 0 for error tone) so a failure
  // cannot vanish unread; only success notices auto-dismiss or get cleared
  // when a row-action dialog opens.
  const noticeSeq = useRef(0);
  const [notice, setNotice] = useState<{
    id: number;
    tone: "success" | "error";
    message: string;
  } | null>(null);
  const showNotice = (tone: "success" | "error", message: string) => {
    noticeSeq.current += 1;
    setNotice({ id: noticeSeq.current, tone, message });
  };
  const clearSuccessNotice = () => {
    setNotice((n) => (n && n.tone === "success" ? null : n));
  };

  // Create user modal state
  const [showCreateModal, setShowCreateModal] = useState(false);
  const [createUsername, setCreateUsername] = useState("");
  const [createEmail, setCreateEmail] = useState("");
  const [createPassword, setCreatePassword] = useState("");
  const [createIsAdmin, setCreateIsAdmin] = useState(false);
  const [createError, setCreateError] = useState<string | null>(null);

  // Reset password modal state
  const [resetTargetUser, setResetTargetUser] = useState<AttributionUser | null>(null);
  const [resetError, setResetError] = useState<string | null>(null);

  // Password reveal modal state (for either created user or reset password)
  const [revealPassword, setRevealPassword] = useState<{
    username: string;
    password: string;
    notice: string;
  } | null>(null);
  const [copied, setCopied] = useState(false);

  const handleOpenCreate = () => {
    setCreateUsername("");
    setCreateEmail("");
    setCreatePassword("");
    setCreateIsAdmin(false);
    setCreateError(null);
    setShowCreateModal(true);
  };

  const handleCreateSubmit = async (e: React.FormEvent) => {
    e.preventDefault();
    setCreateError(null);
    if (!createUsername.trim()) {
      setCreateError("Username is required.");
      return;
    }
    if (createPassword && createPassword.length < 8) {
      setCreateError("Password must be at least 8 characters.");
      return;
    }

    try {
      const res = await createUserMutation.mutateAsync({
        username: createUsername.trim(),
        email: createEmail.trim() || undefined,
        password: createPassword || undefined,
        isAdmin: createIsAdmin,
      });

      setShowCreateModal(false);
      if (res.temporaryPassword) {
        setRevealPassword({
          username: res.user.username,
          password: res.temporaryPassword,
          notice: res.shownOnceNotice || "Copy this password now; we won't show it again.",
        });
      }
    } catch (err) {
      if (err instanceof ApiError) {
        setCreateError(err.message);
      } else {
        setCreateError("Failed to create user.");
      }
    }
  };

  const handleConfirmReset = async () => {
    if (!resetTargetUser) return;
    setResetError(null);
    try {
      const res = await resetPasswordMutation.mutateAsync(resetTargetUser.id);
      const user = resetTargetUser;
      setResetTargetUser(null);
      setRevealPassword({
        username: user.username,
        password: res.newPassword,
        notice: res.shownOnceNotice || "Copy this password now; we won't show it again.",
      });
    } catch (err) {
      if (err instanceof ApiError) {
        setResetError(err.message);
      } else {
        setResetError("Failed to reset password.");
      }
    }
  };

  const openDisable = (user: AttributionUser) => {
    clearSuccessNotice();
    disableUserMutation.reset();
    setDisableTarget(user);
  };

  const handleReEnable = async (user: AttributionUser) => {
    // Clear any previous notice (success or stale error) when retrying so
    // only the outcome of this attempt is shown.
    setNotice(null);
    try {
      await enableUserMutation.mutateAsync(user.id);
    } catch (err) {
      const detail = err instanceof Error ? err.message : null;
      showNotice(
        "error",
        detail
          ? `Failed to re-enable ${user.username}: ${detail}`
          : `Failed to re-enable ${user.username}.`,
      );
    }
  };

  const openToggleAdmin = (user: AttributionUser) => {
    clearSuccessNotice();
    updateUserMutation.reset();
    setAdminTarget({ user, newAdmin: !user.isAdmin });
  };

  const openRevokeSessions = (user: AttributionUser) => {
    clearSuccessNotice();
    revokeUserSessionsMutation.reset();
    setRevokeSessionsTarget(user);
  };

  const handleCopyPassword = async () => {
    if (!revealPassword) return;
    try {
      await navigator.clipboard.writeText(revealPassword.password);
      setCopied(true);
      setTimeout(() => setCopied(false), 2000);
    } catch {
      // fallback
    }
  };

  const total = data?.total || 0;
  const totalPages = Math.ceil(total / pageSize);

  const filteredUsers = useMemo(() => {
    const list = data?.users || [];
    if (!search.trim()) return list;
    const q = search.toLowerCase();
    return list.filter(
      (u) =>
        u.username.toLowerCase().includes(q) ||
        (u.email && u.email.toLowerCase().includes(q)),
    );
  }, [data?.users, search]);

  return (
    <div className="p-6">
      <div className="mb-6 flex flex-wrap items-center justify-between gap-4">
        <div>
          <div className="flex items-center gap-3">
            <h1 className="text-xl font-semibold text-neutral-100">User Management</h1>
            <span className="rounded-full border border-neutral-700 bg-neutral-800 px-2.5 py-0.5 text-xs font-medium text-neutral-400">
              {total} {total === 1 ? "user" : "users"}
            </span>
          </div>
          <p className="mt-1 text-sm text-neutral-400">
            Manage local accounts, permissions, password resets, and view user attribution.
          </p>
        </div>
        <button
          type="button"
          onClick={handleOpenCreate}
          className="rounded bg-brand px-3 py-1.5 text-xs font-medium text-white hover:bg-brand/90"
        >
          Add User
        </button>
      </div>

      <div className="mb-4">
        <input
          type="text"
          value={search}
          onChange={(e) => setSearch(e.target.value)}
          placeholder="Search users by name, username, or email…"
          className="w-full max-w-sm rounded border border-neutral-800 bg-neutral-900 px-3 py-1.5 text-sm text-neutral-100 placeholder-neutral-500 focus:border-brand focus:outline-none"
        />
      </div>

      {notice && (
        <InlineNotice
          key={notice.id}
          tone={notice.tone}
          message={notice.message}
          onDismiss={() => setNotice(null)}
        />
      )}

      {isLoading ? (
        <div className="flex h-32 items-center justify-center text-neutral-500">
          Loading users…
        </div>
      ) : error ? (
        <div className="rounded border border-red-800/60 bg-red-950/40 p-4 text-sm text-red-300">
          Failed to load users: {error instanceof Error ? error.message : "Unknown error"}
        </div>
      ) : filteredUsers.length === 0 ? (
        <div className="rounded border border-neutral-800 bg-neutral-900/40 p-8 text-center text-neutral-400">
          No users found.
        </div>
      ) : (
        <div className="overflow-x-auto rounded-lg border border-neutral-800">
          <table className="w-full text-left text-sm text-neutral-200">
            <thead className="border-b border-neutral-800 bg-neutral-900/60 text-xs uppercase tracking-wider text-neutral-400">
              <tr>
                <th scope="col" className="px-4 py-3">User</th>
                <th scope="col" className="px-4 py-3">Email</th>
                <th scope="col" className="px-4 py-3">Role</th>
                <th scope="col" className="px-4 py-3">MFA</th>
                <th scope="col" className="px-4 py-3">Provider</th>
                <th scope="col" className="px-4 py-3">Created</th>
                <th scope="col" className="px-4 py-3">Last Seen</th>
                <th scope="col" className="px-4 py-3 text-right">Actions</th>
              </tr>
            </thead>
            <tbody className="divide-y divide-neutral-800/60">
              {filteredUsers.map((user) => {
                const isSystem = user.authProvider === "system" || user.username === "system";
                const isDisabled = Boolean(user.disabledAt);
                const isSelf = me?.localUserId !== undefined && user.id === me.localUserId;
                // Forward-link accounts (source !== "local") have no
                // password_hash, and their role is synced from the
                // identity provider's group membership on every request
                // (ResolveOrCreate), not toggled by an admin here. Hide
                // the local-only admin-toggle/reset-password actions for
                // them (issue #485) -- Revoke Sessions stays visible
                // since it's a harmless no-op for an account with no
                // local session to revoke, unlike the other two which
                // 503 outright when auth.mode is "forward".
                const isForwardLink = user.source !== "local";

                return (
                  <tr key={user.id} className="hover:bg-neutral-900/30">
                    <td className="px-4 py-3 font-medium text-neutral-100">
                      <div className="flex items-center gap-2">
                        <span>{user.username}</span>
                        {isDisabled && (
                          <span className="rounded bg-red-950/80 border border-red-800/60 px-1.5 py-0.5 text-[10px] text-red-300">
                            Disabled
                          </span>
                        )}
                      </div>
                      <span className="text-xs text-neutral-500 font-mono">ID: {user.id}</span>
                    </td>
                    <td className="px-4 py-3 text-neutral-400">
                      {user.email || "—"}
                    </td>
                    <td className="px-4 py-3">
                      {user.isAdmin ? (
                        <span className="rounded bg-brand/20 px-2 py-0.5 text-xs font-medium text-brand">
                          Admin
                        </span>
                      ) : (
                        <span className="rounded bg-neutral-800/80 px-2 py-0.5 text-xs text-neutral-400">
                          User
                        </span>
                      )}
                    </td>
                    <td className="px-4 py-3">
                      {user.mfaEnabled ? (
                        <span className="rounded border border-emerald-800/60 bg-emerald-950/80 px-2 py-0.5 text-xs text-emerald-300">
                          Enabled
                        </span>
                      ) : user.source === "local" && isSelf ? (
                        <button
                          type="button"
                          onClick={() => navigate("/mfa/setup")}
                          className="rounded border border-neutral-700 px-2 py-0.5 text-xs text-neutral-400 hover:border-neutral-500 hover:text-neutral-200"
                        >
                          Set up
                        </button>
                      ) : (
                        <span className="text-xs text-neutral-500">—</span>
                      )}
                    </td>
                    <td className="px-4 py-3">
                      <span className="rounded bg-neutral-800/80 px-2 py-0.5 text-xs font-mono text-neutral-400">
                        {user.authProvider}
                      </span>
                    </td>
                    <td className="px-4 py-3 text-xs text-neutral-400">
                      {formatUnixTime(user.createdAt)}
                    </td>
                    <td className="px-4 py-3 text-xs text-neutral-400">
                      {formatUnixTime(user.lastSeenAt)}
                    </td>
                    <td className="px-4 py-3 text-right">
                      {!isSystem && (
                        <div className="flex items-center justify-end gap-2">
                          {isDisabled && !isSelf && (
                            <button
                              type="button"
                              onClick={() => handleReEnable(user)}
                              className="rounded border border-emerald-800/60 px-2.5 py-1 text-xs text-emerald-400 hover:border-emerald-600 hover:text-emerald-300"
                            >
                              Re-enable
                            </button>
                          )}
                          {!isSelf && !isForwardLink && (
                            <button
                              type="button"
                              onClick={() => openToggleAdmin(user)}
                              className="rounded border border-neutral-700 px-2.5 py-1 text-xs text-neutral-300 hover:border-neutral-500 hover:text-white"
                            >
                              {user.isAdmin ? "Remove Admin" : "Make Admin"}
                            </button>
                          )}
                          {isForwardLink && (
                            <span className="text-xs text-neutral-500" title="Role and password are managed by the identity provider">
                              Managed by IdP
                            </span>
                          )}
                          {!isSelf && (
                            <button
                              type="button"
                              onClick={() => openRevokeSessions(user)}
                              className="rounded border border-neutral-700 px-2.5 py-1 text-xs text-neutral-300 hover:border-neutral-500 hover:text-white"
                            >
                              Revoke Sessions
                            </button>
                          )}
                          {!isForwardLink && (
                            <button
                              type="button"
                              onClick={() => {
                                setResetTargetUser(user);
                                setResetError(null);
                              }}
                              className="rounded border border-neutral-700 px-2.5 py-1 text-xs text-neutral-300 hover:border-neutral-500 hover:text-white"
                            >
                              Reset Password
                            </button>
                          )}
                          {!isDisabled && !isSelf && (
                            <button
                              type="button"
                              onClick={() => openDisable(user)}
                              className="rounded border border-neutral-800 px-2.5 py-1 text-xs text-red-400 hover:border-red-700 hover:text-red-300"
                            >
                              Disable
                            </button>
                          )}
                        </div>
                      )}
                    </td>
                  </tr>
                );
              })}
            </tbody>
          </table>
        </div>
      )}

      {totalPages > 1 && (
        <div className="mt-4 flex items-center justify-between text-xs text-neutral-400">
          <span>Page {page + 1} of {totalPages}</span>
          <div className="flex gap-2">
            <button
              type="button"
              disabled={page === 0}
              onClick={() => setPage((p) => Math.max(0, p - 1))}
              className="rounded border border-neutral-700 px-2.5 py-1 disabled:opacity-40"
            >
              Previous
            </button>
            <button
              type="button"
              disabled={page + 1 >= totalPages}
              onClick={() => setPage((p) => p + 1)}
              className="rounded border border-neutral-700 px-2.5 py-1 disabled:opacity-40"
            >
              Next
            </button>
          </div>
        </div>
      )}

      {/* Create User Modal */}
      <Modal open={showCreateModal} onClose={() => setShowCreateModal(false)} title="Create User">
        <form onSubmit={handleCreateSubmit} className="space-y-4">
          {createError && (
            <div className="rounded border border-red-800/60 bg-red-950/40 p-2.5 text-xs text-red-300">
              {createError}
            </div>
          )}
          <div>
            <label className="block text-xs font-medium text-neutral-300 mb-1">
              Username *
            </label>
            <input
              type="text"
              required
              value={createUsername}
              onChange={(e) => setCreateUsername(e.target.value)}
              placeholder="e.g. jdoe"
              className="w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100 placeholder-neutral-500 focus:border-brand focus:outline-none"
            />
          </div>
          <div>
            <label className="block text-xs font-medium text-neutral-300 mb-1">
              Email (optional)
            </label>
            <input
              type="email"
              value={createEmail}
              onChange={(e) => setCreateEmail(e.target.value)}
              placeholder="user@example.com"
              className="w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100 placeholder-neutral-500 focus:border-brand focus:outline-none"
            />
          </div>
          <div>
            <label className="block text-xs font-medium text-neutral-300 mb-1">
              Initial Password
            </label>
            <input
              type="password"
              value={createPassword}
              onChange={(e) => setCreatePassword(e.target.value)}
              placeholder="Leave blank to auto-generate"
              className="w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100 placeholder-neutral-500 focus:border-brand focus:outline-none"
            />
            <p className="mt-1 text-[11px] text-neutral-500">
              If left blank, a secure 16-byte temporary password will be generated for one-time display.
            </p>
          </div>
          <div className="flex items-center gap-2 pt-1">
            <input
              type="checkbox"
              id="create-is-admin"
              checked={createIsAdmin}
              onChange={(e) => setCreateIsAdmin(e.target.checked)}
              className="rounded border-neutral-700 bg-neutral-800 text-brand focus:ring-0"
            />
            <label htmlFor="create-is-admin" className="text-xs text-neutral-300">
              Grant Admin Privileges
            </label>
          </div>
          <div className="mt-6 flex justify-end gap-2 pt-2">
            <button
              type="button"
              onClick={() => setShowCreateModal(false)}
              className="rounded border border-neutral-700 px-3 py-1.5 text-xs text-neutral-300 hover:border-neutral-500"
            >
              Cancel
            </button>
            <button
              type="submit"
              disabled={createUserMutation.isPending}
              className="rounded bg-brand px-3 py-1.5 text-xs font-medium text-white hover:bg-brand/90 disabled:opacity-50"
            >
              {createUserMutation.isPending ? "Creating…" : "Create User"}
            </button>
          </div>
        </form>
      </Modal>

      {/* Reset Password Confirmation Modal */}
      <Modal
        open={Boolean(resetTargetUser)}
        onClose={() => setResetTargetUser(null)}
        title="Reset User Password"
      >
        <div className="space-y-4 text-sm text-neutral-300">
          {resetError && (
            <div className="rounded border border-red-800/60 bg-red-950/40 p-2.5 text-xs text-red-300">
              {resetError}
            </div>
          )}
          <p>
            Are you sure you want to reset the password for{" "}
            <span className="font-semibold text-neutral-100">{resetTargetUser?.username}</span>?
          </p>
          <p className="text-xs text-neutral-400">
            This will generate a new temporary password and immediately revoke all active sessions for this user.
          </p>
          <div className="mt-6 flex justify-end gap-2 pt-2">
            <button
              type="button"
              onClick={() => setResetTargetUser(null)}
              className="rounded border border-neutral-700 px-3 py-1.5 text-xs text-neutral-300 hover:border-neutral-500"
            >
              Cancel
            </button>
            <button
              type="button"
              disabled={resetPasswordMutation.isPending}
              onClick={handleConfirmReset}
              className="rounded bg-amber-600 px-3 py-1.5 text-xs font-medium text-white hover:bg-amber-500 disabled:opacity-50"
            >
              {resetPasswordMutation.isPending ? "Resetting…" : "Confirm Reset"}
            </button>
          </div>
        </div>
      </Modal>

      {/* Password Reveal Modal */}
      <Modal
        open={Boolean(revealPassword)}
        onClose={() => setRevealPassword(null)}
        title="Temporary Password Generated"
      >
        <div className="space-y-4">
          <p className="text-xs text-neutral-300">
            User <span className="font-semibold text-neutral-100">{revealPassword?.username}</span> has been provisioned with the following temporary password:
          </p>
          <div className="flex items-center justify-between rounded border border-neutral-700 bg-neutral-950 p-3">
            <code className="font-mono text-sm text-emerald-400 select-all">
              {revealPassword?.password}
            </code>
            <button
              type="button"
              onClick={handleCopyPassword}
              className="rounded border border-neutral-700 bg-neutral-800 px-2.5 py-1 text-xs text-neutral-200 hover:border-neutral-500"
            >
              {copied ? "Copied!" : "Copy"}
            </button>
          </div>
          <p className="text-xs font-medium text-amber-400">
            {revealPassword?.notice}
          </p>
          <div className="mt-6 flex justify-end">
            <button
              type="button"
              onClick={() => setRevealPassword(null)}
              className="rounded bg-brand px-3 py-1.5 text-xs font-medium text-white hover:bg-brand/90"
            >
              Done
            </button>
          </div>
        </div>
      </Modal>

      {disableTarget && (
        <ConfirmDialog
          titleId="disable-user-title"
          title="Disable User"
          body={
            <>
              Disable logins for{" "}
              <strong className="text-white">{disableTarget.username}</strong>?
            </>
          }
          confirmLabel="Confirm Disable"
          pendingLabel="Disabling…"
          isPending={disableUserMutation.isPending}
          error={disableUserMutation.isError ? disableUserMutation.error : undefined}
          errorLabel="Failed to disable user"
          onConfirm={() => {
            disableUserMutation.mutate(disableTarget.id, {
              onSuccess: () => setDisableTarget(null),
            });
          }}
          onCancel={() => setDisableTarget(null)}
        />
      )}

      {adminTarget && (
        <ConfirmDialog
          titleId={adminTarget.newAdmin ? "grant-admin-title" : "remove-admin-title"}
          title={adminTarget.newAdmin ? "Grant Admin Privileges" : "Remove Admin Privileges"}
          body={
            <>
              {adminTarget.newAdmin ? "Grant" : "Remove"} admin privileges{" "}
              {adminTarget.newAdmin ? "to" : "from"}{" "}
              <strong className="text-white">{adminTarget.user.username}</strong>?
            </>
          }
          confirmLabel={adminTarget.newAdmin ? "Confirm Grant Admin" : "Confirm Remove Admin"}
          pendingLabel="Updating…"
          isPending={updateUserMutation.isPending}
          error={updateUserMutation.isError ? updateUserMutation.error : undefined}
          errorLabel="Failed to update user"
          onConfirm={() => {
            updateUserMutation.mutate(
              { userId: adminTarget.user.id, input: { isAdmin: adminTarget.newAdmin } },
              { onSuccess: () => setAdminTarget(null) },
            );
          }}
          onCancel={() => setAdminTarget(null)}
        />
      )}

      {revokeSessionsTarget && (
        <ConfirmDialog
          titleId="revoke-sessions-title"
          title="Revoke Sessions"
          body={
            <>
              Revoke all active sessions for{" "}
              <strong className="text-white">{revokeSessionsTarget.username}</strong>? They will
              be logged out everywhere.
            </>
          }
          confirmLabel="Confirm Revoke"
          pendingLabel="Revoking…"
          isPending={revokeUserSessionsMutation.isPending}
          error={revokeUserSessionsMutation.isError ? revokeUserSessionsMutation.error : undefined}
          errorLabel="Failed to revoke sessions"
          onConfirm={() => {
            revokeUserSessionsMutation.mutate(revokeSessionsTarget.id, {
              onSuccess: (res) => {
                setRevokeSessionsTarget(null);
                showNotice(
                  "success",
                  `Revoked ${res.revokedCount} active session${res.revokedCount === 1 ? "" : "s"}.`,
                );
              },
            });
          }}
          onCancel={() => setRevokeSessionsTarget(null)}
        />
      )}
    </div>
  );
}
