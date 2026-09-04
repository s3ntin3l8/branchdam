import { lazy, Suspense } from "react";
import { NavLink, Outlet, Route, Routes } from "react-router";
import { useEventStream } from "./hooks/useEventStream";
import { useMe, useUnlinkedCount } from "./hooks/queries";
import { AuthErrorBanner } from "./components/AuthErrorBanner";
import { ErrorBoundary } from "./components/ErrorBoundary";
import BrandMark from "./components/BrandMark";

const AssetListPage = lazy(() => import("./pages/AssetListPage"));
const AssetDetailPage = lazy(() => import("./pages/AssetDetailPage"));
const AuditQueuePage = lazy(() => import("./pages/AuditQueuePage"));
const IngestPage = lazy(() => import("./pages/IngestPage"));
const IngestJobsPage = lazy(() => import("./pages/IngestJobsPage"));
const SettingsPage = lazy(() => import("./pages/SettingsPage"));
const StorageHealthPage = lazy(() => import("./pages/StorageHealthPage"));

function NavItem({ to, children }: { to: string; children: React.ReactNode }) {
  return (
    <NavLink
      to={to}
      className={({ isActive }) =>
        `flex items-center justify-between rounded px-3 py-2 text-sm font-medium ${
          isActive ? "bg-neutral-800 text-white" : "text-neutral-400 hover:bg-neutral-900 hover:text-neutral-200"
        }`
      }
    >
      {children}
    </NavLink>
  );
}

// Layout wraps every route. It owns the SSE hook, the auth banner, and the
// nav sidebar. useEventStream must run here (not in App's body) so it's
// inside the Router context that useNavigation/useBlocker require.
export function Layout() {
  const { disconnected } = useEventStream();
  const { data: me } = useMe();
  const { data: unlinkedCount } = useUnlinkedCount();

  return (
    <div className="flex h-screen">
      <AuthErrorBanner />
      <nav className="w-56 shrink-0 border-r border-neutral-800 p-4">
        <div className="mb-6 flex items-center gap-2">
          <BrandMark className="h-5 w-5 text-brand" />
          <span className="text-lg font-semibold">
            <span className="font-normal">branch</span>DAM
          </span>
        </div>
        <div className="space-y-1">
          <NavItem to="/assets">
            <span>Assets</span>
            {unlinkedCount && unlinkedCount > 0 ? (
              <span className="rounded bg-amber-900/80 px-1.5 py-0.5 text-xs text-amber-200" title={`${unlinkedCount} unlinked nodes`}>
                {unlinkedCount}
              </span>
            ) : null}
          </NavItem>
          <NavItem to="/audit">Audit Queue</NavItem>
          <NavItem to="/ingest">Ingest</NavItem>
          <NavItem to="/jobs">Ingest Jobs</NavItem>
          <NavItem to="/storage-health">Storage Health</NavItem>
          <NavItem to="/settings">Settings</NavItem>
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
          <div className="absolute bottom-4 text-xs text-neutral-500">Signed in as {me.name}</div>
        )}
      </nav>
      <main className="flex-1 overflow-auto">
        <ErrorBoundary>
          <Outlet />
        </ErrorBoundary>
      </main>
    </div>
  );
}

// App is kept as a thin wrapper that re-renders the original imperative
// <Routes> tree for any code paths (tests, legacy callers) that mount it
// directly. Production now uses the data-router created in main.tsx.
export default function App() {
  return (
    <Routes>
      <Route element={<Layout />}>
        <Route path="/" element={
          <Suspense fallback={<div className="p-6 text-neutral-400">Loading assets…</div>}>
            <AssetListPage />
          </Suspense>
        } />
        <Route path="/assets" element={
          <Suspense fallback={<div className="p-6 text-neutral-400">Loading assets…</div>}>
            <AssetListPage />
          </Suspense>
        } />
        <Route path="/assets/:id" element={
          <Suspense fallback={<div className="p-6 text-neutral-400">Loading asset…</div>}>
            <AssetDetailPage />
          </Suspense>
        } />
        <Route path="/audit" element={
          <Suspense fallback={<div className="p-6 text-neutral-400">Loading audit queue…</div>}>
            <AuditQueuePage />
          </Suspense>
        } />
        <Route path="/ingest" element={
          <Suspense fallback={<div className="p-6 text-neutral-400">Loading ingest…</div>}>
            <IngestPage />
          </Suspense>
        } />
        <Route path="/jobs" element={
          <Suspense fallback={<div className="p-6 text-neutral-400">Loading jobs…</div>}>
            <IngestJobsPage />
          </Suspense>
        } />
        <Route path="/storage-health" element={
          <Suspense fallback={<div className="p-6 text-neutral-400">Loading storage health…</div>}>
            <StorageHealthPage />
          </Suspense>
        } />
        <Route path="/settings" element={
          <Suspense fallback={<div className="p-6 text-neutral-400">Loading settings…</div>}>
            <SettingsPage />
          </Suspense>
        } />
      </Route>
    </Routes>
  );
}
