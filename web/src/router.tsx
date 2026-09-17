import { Suspense, lazy } from "react";
import { createBrowserRouter, type RouteObject } from "react-router";
import { Layout } from "./App";

const AssetListPage = lazy(() => import("./pages/AssetListPage"));
const AssetDetailPage = lazy(() => import("./pages/AssetDetailPage"));
const AuditQueuePage = lazy(() => import("./pages/AuditQueuePage"));
const IngestPage = lazy(() => import("./pages/IngestPage"));
const IngestJobsPage = lazy(() => import("./pages/IngestJobsPage"));
const SettingsPage = lazy(() => import("./pages/SettingsPage"));
const StorageHealthPage = lazy(() => import("./pages/StorageHealthPage"));
const CompanionPairingsPage = lazy(() => import("./pages/CompanionPairingsPage"));
const AuditLogPage = lazy(() => import("./pages/AuditLogPage"));
const UsersPage = lazy(() => import("./pages/UsersPage"));
const LoginPage = lazy(() => import("./pages/LoginPage"));
const MfaSetupPage = lazy(() => import("./pages/MfaSetupPage"));
const PasswordResetPage = lazy(() => import("./pages/PasswordResetPage"));

// Minimal error element for the data router's errorElement. The full
// ErrorBoundary requires children, which the router's errorElement slot
// doesn't provide -- this is the boundary that catches uncaught render
// errors from the matched route element and from loader calls.
function RouterErrorElement() {
  return (
    <div className="p-6 m-4 rounded-lg border border-red-800 bg-neutral-900 text-neutral-200 max-w-xl">
      <h2 className="text-lg font-semibold text-red-400 mb-2">Something went wrong</h2>
      <p className="text-sm text-neutral-400 mb-4">An unexpected error occurred loading this page.</p>
      <button
        type="button"
        onClick={() => window.location.reload()}
        className="rounded border border-neutral-700 bg-neutral-800 px-3.5 py-1.5 text-xs font-medium text-neutral-300 hover:bg-neutral-700"
      >
        Reload Page
      </button>
    </div>
  );
}

// routeConfig is exported (separately from the createBrowserRouter call
// below) so router.test.tsx can assert on route placement -- e.g. that
// /password-reset stays registered -- without rendering the data router
// or touching the DOM.
//
// /password-reset is nested inside the Layout shell, same as /login: an
// unauthenticated visitor renders with the sidebar and fires
// useEventStream/useMe/useUnlinkedCount, but a 401 there only triggers
// the AuthErrorBanner -- no redirect loop. See PasswordResetPage.tsx's
// header comment for the full rationale.
export const routeConfig: RouteObject[] = [
  {
    path: "/",
    element: <Layout />,
    errorElement: <RouterErrorElement />,
    children: [
      { index: true, element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading assets…</div>}><AssetListPage /></Suspense> },
      { path: "assets", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading assets…</div>}><AssetListPage /></Suspense> },
      { path: "assets/:id", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading asset…</div>}><AssetDetailPage /></Suspense> },
      { path: "audit", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading audit queue…</div>}><AuditQueuePage /></Suspense> },
      { path: "ingest", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading ingest…</div>}><IngestPage /></Suspense> },
      { path: "jobs", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading jobs…</div>}><IngestJobsPage /></Suspense> },
      { path: "storage-health", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading storage health…</div>}><StorageHealthPage /></Suspense> },
      { path: "settings", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading settings…</div>}><SettingsPage /></Suspense> },
      { path: "companion", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading pairings…</div>}><CompanionPairingsPage /></Suspense> },
      { path: "audit-log", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading audit log…</div>}><AuditLogPage /></Suspense> },
      { path: "users", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading users…</div>}><UsersPage /></Suspense> },
      { path: "login", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading…</div>}><LoginPage /></Suspense> },
      { path: "mfa/setup", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading…</div>}><MfaSetupPage /></Suspense> },
      { path: "password-reset", element: <Suspense fallback={<div className="p-6 text-neutral-400">Loading…</div>}><PasswordResetPage /></Suspense> },
    ],
  },
];

// Data router (createBrowserRouter + RouterProvider) is required for
// useBlocker in useDirtyFormGuard -- the declarative <BrowserRouter> only
// exposes the context useBlocker needs in dev/test, and throws in
// production builds. Errors there crash /settings on mount.
export const router = createBrowserRouter(routeConfig);
