import { useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { Link, useSearchParams } from "react-router";
import { api } from "../api/client";

// PasswordResetPage is the SPA endpoint for the password-reset link
// emailed to a user (the link in PasswordResetHTML/Text points at
// {baseURL}/password-reset?token=<plaintext>). The user lands here
// from their email client, pastes a new password, and submits; the
// page POSTs to /api/v1/password-reset/confirm with {token, newPassword}.
//
// Three states drive the UI:
//   - token missing from the URL: render an error pointing at /login
//     (the user clicked the link wrong, or the link was truncated)
//   - mutation pending: render the form with submit disabled
//   - mutation succeeded: render a success panel with a link to /login
//   - mutation failed: render the form with the error message inline
//
// The page intentionally lives OUTSIDE the Layout shell: users
// arriving here are typically unauthenticated (the link comes from an
// email, not from a logged-in session), so wrapping it in the sidebar
// nav would force a /me fetch and a redirect loop. The /login and
// /password-reset routes share the same min-h-screen standalone
// chrome.
export default function PasswordResetPage() {
  const [searchParams] = useSearchParams();
  const token = searchParams.get("token") ?? "";

  if (!token) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-neutral-950">
        <div className="w-full max-w-sm rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow">
          <h1 className="mb-2 text-lg font-semibold text-neutral-100">Invalid reset link</h1>
          <p className="mb-4 text-sm text-neutral-400">
            This page expects a one-time token in the URL. Open the link from your reset
            email exactly as it was sent -- it should look like{" "}
            <code className="rounded bg-neutral-800 px-1 py-0.5 text-xs">/password-reset?token=...</code>.
          </p>
          <Link
            to="/login"
            className="block w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-center text-sm text-neutral-200 hover:bg-neutral-700"
          >
            Back to sign in
          </Link>
        </div>
      </div>
    );
  }

  return <ResetForm token={token} />;
}

function ResetForm({ token }: { token: string }) {
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");

  const mismatch = confirm.length > 0 && password !== confirm;
  const tooShort = password.length > 0 && password.length < 8;

  const confirmMutation = useMutation({
    mutationFn: api.confirmPasswordReset,
  });

  if (confirmMutation.isSuccess) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-neutral-950">
        <div className="w-full max-w-sm rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow">
          <h1 className="mb-2 text-lg font-semibold text-neutral-100">Password updated</h1>
          <p className="mb-4 text-sm text-neutral-400">
            Your password has been changed. Sign in with your new password to continue.
          </p>
          <Link
            to="/login"
            className="block w-full rounded bg-brand px-3 py-1.5 text-center text-sm font-medium text-white hover:bg-brand-dark"
          >
            Sign in
          </Link>
        </div>
      </div>
    );
  }

  const submitDisabled =
    confirmMutation.isPending || !password || !confirm || mismatch || tooShort;

  return (
    <div className="flex min-h-screen items-center justify-center bg-neutral-950">
      <div className="w-full max-w-sm rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow">
        <h1 className="mb-2 text-lg font-semibold text-neutral-100">Set a new password</h1>
        <p className="mb-4 text-sm text-neutral-400">
          Choose a new password for your branchDAM account. The reset link can only be used once.
        </p>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (submitDisabled) return;
            confirmMutation.mutate({ token, newPassword: password });
          }}
          className="space-y-3"
        >
          <label className="block">
            <span className="text-xs text-neutral-400">New password (min 8 chars)</span>
            <input
              type="password"
              required
              autoFocus
              minLength={8}
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="mt-1 w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
            />
          </label>
          <label className="block">
            <span className="text-xs text-neutral-400">Confirm new password</span>
            <input
              type="password"
              required
              minLength={8}
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              className="mt-1 w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
            />
          </label>
          {mismatch ? <p className="text-xs text-red-400">Passwords do not match</p> : null}
          {tooShort ? <p className="text-xs text-red-400">Password must be at least 8 characters</p> : null}
          {confirmMutation.error ? (
            <p data-testid="confirm-error" className="text-xs text-red-400">
              {confirmMutation.error.message}
            </p>
          ) : null}
          <button
            type="submit"
            disabled={submitDisabled}
            className="w-full rounded bg-brand px-3 py-1.5 text-sm font-medium text-white hover:bg-brand-dark disabled:opacity-50"
          >
            {confirmMutation.isPending ? "Updating…" : "Update password"}
          </button>
        </form>
        <p className="mt-4 text-xs text-neutral-500">
          Lost the link?{" "}
          <Link to="/login" className="text-neutral-400 hover:text-neutral-200">
            Request a new one from the sign-in page
          </Link>
          .
        </p>
      </div>
    </div>
  );
}
