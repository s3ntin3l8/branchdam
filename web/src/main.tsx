import { StrictMode, Suspense, lazy } from "react";
import { createRoot } from "react-dom/client";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createBrowserRouter, RouterProvider } from "react-router";
import { Layout } from "./App";
import "./styles/index.css";

const AssetListPage = lazy(() => import("./pages/AssetListPage"));
const AssetDetailPage = lazy(() => import("./pages/AssetDetailPage"));
const AuditQueuePage = lazy(() => import("./pages/AuditQueuePage"));
const IngestPage = lazy(() => import("./pages/IngestPage"));
const IngestJobsPage = lazy(() => import("./pages/IngestJobsPage"));
const SettingsPage = lazy(() => import("./pages/SettingsPage"));
const StorageHealthPage = lazy(() => import("./pages/StorageHealthPage"));

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      staleTime: 15_000,
      retry: 1,
    },
  },
});

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

// Data router (createBrowserRouter + RouterProvider) is required for
// useBlocker in useDirtyFormGuard -- the declarative <BrowserRouter> only
// exposes the context useBlocker needs in dev/test, and throws in
// production builds. Errors there crash /settings on mount.
const router = createBrowserRouter([
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
    ],
  },
]);

const rootEl = document.getElementById("root");
if (!rootEl) {
  throw new Error("no #root element found");
}

createRoot(rootEl).render(
  <StrictMode>
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  </StrictMode>,
);
