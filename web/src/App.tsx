import { useEffect, useState } from "react";
import { Navigate, NavLink, Outlet, useLocation } from "react-router";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEventStream } from "./hooks/useEventStream";
import { useMe, useUnlinkedCount } from "./hooks/queries";
import { api, API_AUTH_ERROR_EVENT, ME_PATH, type ApiAuthErrorDetail } from "./api/client";
import { AuthErrorBanner } from "./components/AuthErrorBanner";
import { ErrorBoundary } from "./components/ErrorBoundary";
import { UserMenu } from "./components/UserMenu";
import BrandMark from "./components/BrandMark";
import MfaChallengeForm from "./components/MfaChallengeForm";

// isAuthFailure reports whether a /api/v1/me error means "no signed-in
// session": the server answers an anonymous request in local or both mode
// with 403 "authentication required" (auth.RequireAdmin with a nil
// Principal), and 401 is the generic "session lost". Duck-typed on
// .status rather than instanceof ApiError so a mocked api module in tests
// doesn't have to re-export the class.
function isAuthFailure(error: unknown): boolean {
  const status = (error as { status?: unknown } | null)?.status;
  return status === 401 || status === 403;
}

function NavItem({ to, children }: { to: string; children: React.ReactNode }) {
  return (
    <NavLink
      to={to}
      className={({ isActive }) =>
        `flex items-center justify-between rounded px-3 py-2 text-sm font-medium ${
          isActive ? "bg-neutral-800 text-neutral-100" : "text-neutral-400 hover:bg-neutral-900 hover:text-neutral-200"
        }`
      }
    >
      {children}
    </NavLink>
  );
}

// Layout wraps every signed-in route (router.tsx; /login and
// /password-reset render outside it). It owns the SSE hook, the auth
// banner, and the nav sidebar.
//
// A signed-out visitor in local or both auth mode (GET /api/v1/setup/status)
// is redirected to /login with the path they asked for in location state,
// and LoginPage returns them there after sign-in. Forward mode has no local
// login page, so it keeps rendering the shell and leaves sign-in to the
// reverse proxy.
//
// When me.mfaRequired is true (half-authed session after a page reload
// during an MFA challenge), the layout renders only the MFA challenge
// form instead of the full nav shell. This closes the reload-during-
// challenge gap identified in PR #459 round-3 suggestion #1: without
// this, a reload strands the SPA half-authenticated because the 403
// from MFAGate on every API call renders a dead shell.
export function Layout() {
  const meQuery = useMe();
  const me = meQuery.data;
  const location = useLocation();
  const queryClient = useQueryClient();

  // Below the md breakpoint the sidebar collapses behind a menu button in
  // a top bar and opens as a drawer over the page. It closes on navigation
  // (render-time state adjustment, same pattern as AssetListPage's filter
  // reset), on Escape and on a click on the backdrop. At md and up it is
  // always shown and navOpen has no effect.
  const [navOpen, setNavOpen] = useState(false);
  const [navPath, setNavPath] = useState(location.pathname);
  if (location.pathname !== navPath) {
    setNavPath(location.pathname);
    setNavOpen(false);
  }
  useEffect(() => {
    if (!navOpen) return;
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === "Escape") setNavOpen(false);
    };
    document.addEventListener("keydown", onKeyDown);
    return () => document.removeEventListener("keydown", onKeyDown);
  }, [navOpen]);

  // A failed /me keeps the last good data (TanStack Query retains it), so
  // "signed in" must also check the latest fetch didn't fail on auth.
  const authFailed = meQuery.isError === true && isAuthFailure(meQuery.error);
  const signedIn = !!me?.authenticated && !authFailed;
  // Signed out: the latest /me failed on auth, or (forward mode with no
  // identity headers) answered for an anonymous browser. LoginPage drops
  // the cached /me before navigating back here, so a pre-login 403 can't
  // bounce a freshly signed-in user straight back to /login.
  const signedOut =
    authFailed || (meQuery.isSuccess === true && me?.kind === "user" && !me.authenticated);

  // Only a signed-out visitor needs the auth mode: local and both mode
  // have a /login page to send them to; forward mode leaves sign-in to
  // the reverse proxy, so it keeps rendering the shell as before.
  const setupStatus = useQuery({
    queryKey: ["setup-status"],
    queryFn: api.setupStatus,
    enabled: signedOut,
    retry: false,
  });

  const { disconnected } = useEventStream({ enabled: signedIn && !me?.mfaRequired });
  const { data: unlinkedCount } = useUnlinkedCount();

  // Any 401/403 from another endpoint may mean the session just expired:
  // re-check /me so the redirect below can fire. Skipped for /me's own
  // failure, which would otherwise loop.
  useEffect(() => {
    const onAuthError = (e: Event) => {
      const detail = (e as CustomEvent<ApiAuthErrorDetail>).detail;
      if (!detail || detail.path === ME_PATH) return;
      // cancelRefetch: false joins an in-flight /me instead of restarting
      // it once per failing query on a page full of them.
      void queryClient.invalidateQueries({ queryKey: ["me"] }, { cancelRefetch: false });
    };
    window.addEventListener(API_AUTH_ERROR_EVENT, onAuthError);
    return () => window.removeEventListener(API_AUTH_ERROR_EVENT, onAuthError);
  }, [queryClient]);

  const mfaChallengeMutation = useMutation({
    mutationFn: api.mfaChallenge,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["me"] });
    },
  });

  const logoutMutation = useMutation({
    mutationFn: api.logout,
    onSuccess: () => {
      void queryClient.invalidateQueries({ queryKey: ["me"] });
    },
  });

  if (signedOut) {
    const mode = setupStatus.data?.mode;
    if (mode === "local" || mode === "both") {
      const from = `${location.pathname}${location.search}${location.hash}`;
      return <Navigate to="/login" replace state={{ from }} />;
    }
    if (setupStatus.isPending) {
      return (
        <div className="flex min-h-screen items-center justify-center bg-neutral-950 text-neutral-400">
          Checking sign-in…
        </div>
      );
    }
    // Forward mode, or the status probe failed: fall through to the shell.
  }

  if (me?.mfaRequired && !authFailed) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-neutral-950">
        <div className="w-full max-w-sm rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow">
          <div className="mb-4 flex items-center gap-2">
            <BrandMark className="h-5 w-5 text-brand" />
            <span className="text-lg font-semibold">
              <span className="font-normal">branch</span>DAM
            </span>
          </div>
          <MfaChallengeForm
            onSubmit={(code) => mfaChallengeMutation.mutate({ code })}
            pending={mfaChallengeMutation.isPending}
            error={mfaChallengeMutation.error?.message}
          />
          <button
            type="button"
            onClick={() => logoutMutation.mutate()}
            disabled={logoutMutation.isPending}
            className="mt-4 w-full text-xs text-neutral-500 hover:text-neutral-300 disabled:opacity-50"
          >
            {logoutMutation.isPending ? "Signing out…" : "Sign out"}
          </button>
        </div>
      </div>
    );
  }

  return (
    <div className="flex h-screen flex-col md:flex-row">
      <AuthErrorBanner />
      <header className="flex shrink-0 items-center justify-between border-b border-neutral-800 px-4 py-2 md:hidden">
        <div className="flex items-center gap-2">
          <BrandMark className="h-5 w-5 text-brand" />
          <span className="text-lg font-semibold">
            <span className="font-normal">branch</span>DAM
          </span>
        </div>
        <button
          type="button"
          onClick={() => setNavOpen((open) => !open)}
          aria-controls="app-nav"
          aria-expanded={navOpen}
          aria-label={navOpen ? "Close navigation" : "Open navigation"}
          className="rounded p-2 text-neutral-300 hover:bg-neutral-800"
        >
          <svg aria-hidden="true" viewBox="0 0 20 20" className="h-5 w-5" fill="none" stroke="currentColor" strokeWidth="1.5">
            {navOpen ? <path d="M5 5l10 10M15 5L5 15" strokeLinecap="round" /> : <path d="M3 6h14M3 10h14M3 14h14" strokeLinecap="round" />}
          </svg>
        </button>
      </header>
      {navOpen && (
        <div className="fixed inset-0 z-30 bg-black/60 md:hidden" aria-hidden="true" onClick={() => setNavOpen(false)} />
      )}
      <nav
        id="app-nav"
        aria-label="Main"
        className={`${
          navOpen ? "fixed inset-y-0 left-0 z-40 block overflow-y-auto shadow-xl" : "hidden"
        } w-56 shrink-0 border-r border-neutral-800 bg-neutral-950 p-4 md:relative md:z-auto md:block md:shadow-none`}
      >
        <div className="mb-6 hidden items-center gap-2 md:flex">
          <BrandMark className="h-5 w-5 text-brand" />
          <span className="text-lg font-semibold">
            <span className="font-normal">branch</span>DAM
          </span>
        </div>
        <div className="space-y-1">
          <NavItem to="/assets">
            <span>Assets</span>
            {unlinkedCount && unlinkedCount > 0 ? (
              <span className="rounded bg-amber-900 px-1.5 py-0.5 text-xs text-amber-200" title={`${unlinkedCount} unlinked nodes`}>
                {unlinkedCount}
              </span>
            ) : null}
          </NavItem>
          <NavItem to="/audit">Audit Queue</NavItem>
          <NavItem to="/ingest">Ingest</NavItem>
          <NavItem to="/jobs">Ingest Jobs</NavItem>
          <NavItem to="/storage-health">Storage Health</NavItem>
          <NavItem to="/companion">Companion Pairing</NavItem>
          {me?.isAdmin && <NavItem to="/audit-log">Audit Log</NavItem>}
          <NavItem to="/settings">Settings</NavItem>
          {me?.isAdmin && <NavItem to="/users">Users</NavItem>}
        </div>
        {disconnected && (
          <div
            role="status"
            aria-live="polite"
            className="mt-4 flex items-center gap-2 rounded border border-amber-800/60 bg-amber-950/60 px-2.5 py-1.5 text-xs text-amber-300"
          >
            <span className="h-2 w-2 shrink-0 rounded-full bg-amber-400 animate-pulse" />
            <span>Reconnecting…</span>
          </div>
        )}
        {me && me.kind === "user" && me.name && (
          <div className="absolute bottom-4 left-4 right-4">
            <UserMenu me={me} />
          </div>
        )}
      </nav>
      <main className="min-w-0 flex-1 overflow-auto">
        <ErrorBoundary>
          <Outlet />
        </ErrorBoundary>
      </main>
    </div>
  );
}
