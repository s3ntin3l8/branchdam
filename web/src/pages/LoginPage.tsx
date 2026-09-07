import { useState } from "react";
import { useQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router";
import { api } from "../api/client";

// LoginPage is the entry point for local-auth deployments. It calls
// /api/v1/setup/status on mount: if the users table is empty, it renders
// the first-user setup form (POST /api/v1/setup/admin creates the first
// admin and auto-logs them in via Set-Cookie); otherwise it renders the
// standard login form. Forward-only deployments never reach this page --
// the SPA's index route handles them via the regular SPA shell.
//
// When mode is "forward" or "both", the user has the option to log in
// via the forward-auth SSO button (just navigates to "/" -- Traefik
// handles the forward-auth flow itself). When mode is "local", the
// forward-auth button is hidden.
export default function LoginPage() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();

  const { data: status } = useQuery({
    queryKey: ["setup-status"],
    queryFn: api.setupStatus,
    retry: false,
  });

  const setupMutation = useMutation({
    mutationFn: api.setupAdmin,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["me"] });
      void queryClient.invalidateQueries({ queryKey: ["setup-status"] });
      navigate("/", { replace: true });
    },
  });

  const loginMutation = useMutation({
    mutationFn: api.login,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["me"] });
      navigate("/", { replace: true });
    },
  });

  const resetRequestMutation = useMutation({
    mutationFn: api.requestPasswordReset,
  });

  if (!status) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-neutral-950 text-neutral-400">
        Loading…
      </div>
    );
  }

  if (status.readyForSetup) {
    return <SetupForm onSubmit={(input) => setupMutation.mutate(input)} pending={setupMutation.isPending} error={setupMutation.error?.message} />;
  }

  return (
    <div className="flex min-h-screen items-center justify-center bg-neutral-950">
      <div className="w-full max-w-sm rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow">
        <div className="mb-4 flex items-center gap-2">
          <span className="text-lg font-semibold">
            <span className="font-normal">branch</span>DAM
          </span>
        </div>
        {status.mode !== "forward" ? (
          <LoginForm
            onSubmit={(input) => loginMutation.mutate(input)}
            pending={loginMutation.isPending}
            error={loginMutation.error?.message}
            onSso={status.mode === "both" ? () => navigate("/") : undefined}
            resetPending={resetRequestMutation.isPending}
            resetSent={resetRequestMutation.isSuccess}
            resetError={resetRequestMutation.error?.message}
            onReset={(email) => resetRequestMutation.mutate({ email })}
            onResetDismiss={() => resetRequestMutation.reset()}
          />
        ) : (
          <ForwardOnly onSso={() => navigate("/")} />
        )}
      </div>
    </div>
  );
}

function SetupForm({
  onSubmit,
  pending,
  error,
}: {
  onSubmit: (input: { username: string; email: string; password: string }) => void;
  pending: boolean;
  error: string | undefined;
}) {
  const [username, setUsername] = useState("");
  const [email, setEmail] = useState("");
  const [password, setPassword] = useState("");
  const [confirm, setConfirm] = useState("");

  const mismatch = confirm.length > 0 && password !== confirm;

  return (
    <div className="flex min-h-screen items-center justify-center bg-neutral-950">
      <div className="w-full max-w-sm rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow">
        <h1 className="mb-2 text-lg font-semibold text-neutral-100">Create the first admin</h1>
        <p className="mb-4 text-sm text-neutral-400">
          No users exist yet. This account gets full admin access. You can create more users
          via the admin UI once setup is complete.
        </p>
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (!username || password.length < 8 || mismatch) return;
            onSubmit({ username, email, password });
          }}
          className="space-y-3"
        >
          <label className="block">
            <span className="text-xs text-neutral-400">Username</span>
            <input
              type="text"
              required
              autoFocus
              value={username}
              onChange={(e) => setUsername(e.target.value)}
              className="mt-1 w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
            />
          </label>
          <label className="block">
            <span className="text-xs text-neutral-400">Email (optional)</span>
            <input
              type="email"
              value={email}
              onChange={(e) => setEmail(e.target.value)}
              className="mt-1 w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
            />
          </label>
          <label className="block">
            <span className="text-xs text-neutral-400">Password (min 8 chars)</span>
            <input
              type="password"
              required
              value={password}
              onChange={(e) => setPassword(e.target.value)}
              className="mt-1 w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
            />
          </label>
          <label className="block">
            <span className="text-xs text-neutral-400">Confirm password</span>
            <input
              type="password"
              required
              value={confirm}
              onChange={(e) => setConfirm(e.target.value)}
              className="mt-1 w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
            />
          </label>
          {mismatch ? <p className="text-xs text-red-400">Passwords do not match</p> : null}
          {error ? <p className="text-xs text-red-400">{error}</p> : null}
          <button
            type="submit"
            disabled={pending || !username || password.length < 8 || mismatch}
            className="w-full rounded bg-brand px-3 py-1.5 text-sm font-medium text-white hover:bg-brand-dark disabled:opacity-50"
          >
            {pending ? "Creating…" : "Create admin"}
          </button>
        </form>
      </div>
    </div>
  );
}

function LoginForm({
  onSubmit,
  pending,
  error,
  onSso,
  resetPending,
  resetSent,
  resetError,
  onReset,
  onResetDismiss,
}: {
  onSubmit: (input: { username: string; password: string }) => void;
  pending: boolean;
  error: string | undefined;
  onSso?: () => void;
  resetPending: boolean;
  resetSent: boolean;
  resetError: string | undefined;
  onReset: (email: string) => void;
  onResetDismiss: () => void;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [resetOpen, setResetOpen] = useState(false);
  const [resetEmail, setResetEmail] = useState("");

  return (
    <>
      <h1 className="mb-4 text-lg font-semibold text-neutral-100">Sign in</h1>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!username || !password) return;
          onSubmit({ username, password });
        }}
        className="space-y-3"
      >
        <label className="block">
          <span className="text-xs text-neutral-400">Username</span>
          <input
            type="text"
            required
            autoFocus
            value={username}
            onChange={(e) => setUsername(e.target.value)}
            className="mt-1 w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
          />
        </label>
        <label className="block">
          <span className="text-xs text-neutral-400">Password</span>
          <input
            type="password"
            required
            value={password}
            onChange={(e) => setPassword(e.target.value)}
            className="mt-1 w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
          />
        </label>
        {error ? <p className="text-xs text-red-400">{error}</p> : null}
        <button
          type="submit"
          disabled={pending}
          className="w-full rounded bg-brand px-3 py-1.5 text-sm font-medium text-white hover:bg-brand-dark disabled:opacity-50"
        >
          {pending ? "Signing in…" : "Sign in"}
        </button>
      </form>
      {!resetOpen ? (
        <button
          type="button"
          onClick={() => setResetOpen(true)}
          className="mt-3 w-full text-xs text-neutral-400 hover:text-neutral-200"
        >
          Forgot password?
        </button>
      ) : resetSent ? (
        <div
          data-testid="reset-sent"
          className="mt-3 rounded border border-emerald-800 bg-emerald-950/30 p-3 text-xs text-emerald-200"
        >
          <p className="font-medium">If an account exists, an operator has been notified.</p>
          <p className="mt-1 text-emerald-300/80">
            Check the admin "Pending password resets" panel or the server logs for the token.
          </p>
          <button
            type="button"
            onClick={() => {
              onResetDismiss();
              setResetOpen(false);
              setResetEmail("");
            }}
            className="mt-2 rounded border border-emerald-700 px-2 py-1 text-xs text-emerald-200 hover:bg-emerald-900/30"
          >
            Dismiss
          </button>
        </div>
      ) : (
        <form
          data-testid="reset-form"
          onSubmit={(e) => {
            e.preventDefault();
            if (!resetEmail) return;
            onReset(resetEmail);
          }}
          className="mt-3 space-y-2 rounded border border-neutral-800 bg-neutral-950/40 p-3"
        >
          <p className="text-xs text-neutral-400">
            Enter your email. If an account exists, the operator will be notified with a reset token.
          </p>
          <input
            type="email"
            required
            value={resetEmail}
            onChange={(e) => setResetEmail(e.target.value)}
            placeholder="you@example.com"
            className="w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
          />
          {resetError ? <p className="text-xs text-red-400">{resetError}</p> : null}
          <div className="flex gap-2">
            <button
              type="submit"
              disabled={resetPending || !resetEmail}
              className="flex-1 rounded bg-brand px-3 py-1.5 text-xs font-medium text-white hover:bg-brand-dark disabled:opacity-50"
            >
              {resetPending ? "Sending…" : "Send reset link"}
            </button>
            <button
              type="button"
              onClick={() => {
                onResetDismiss();
                setResetOpen(false);
                setResetEmail("");
              }}
              className="rounded border border-neutral-700 px-3 py-1.5 text-xs text-neutral-300 hover:bg-neutral-800"
            >
              Cancel
            </button>
          </div>
        </form>
      )}
      {onSso ? (
        <>
          <div className="my-4 flex items-center gap-2 text-xs text-neutral-500">
            <span className="flex-1 border-t border-neutral-800" />
            <span>or</span>
            <span className="flex-1 border-t border-neutral-800" />
          </div>
          <button
            type="button"
            onClick={onSso}
            className="w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-200 hover:bg-neutral-700"
          >
            Sign in with SSO
          </button>
        </>
      ) : null}
    </>
  );
}

function ForwardOnly({ onSso }: { onSso: () => void }) {
  return (
    <>
      <h1 className="mb-4 text-lg font-semibold text-neutral-100">Sign in</h1>
      <button
        type="button"
        onClick={onSso}
        className="w-full rounded bg-brand px-3 py-1.5 text-sm font-medium text-white hover:bg-brand-dark"
      >
        Sign in with SSO
      </button>
    </>
  );
}
