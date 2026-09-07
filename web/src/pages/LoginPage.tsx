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
}: {
  onSubmit: (input: { username: string; password: string }) => void;
  pending: boolean;
  error: string | undefined;
  onSso?: () => void;
}) {
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
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
