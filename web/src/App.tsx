import { lazy, Suspense } from "react";
import { NavLink, Route, Routes } from "react-router";
import { useEventStream } from "./hooks/useEventStream";
import { useMe } from "./hooks/queries";

const AssetListPage = lazy(() => import("./pages/AssetListPage"));
const AssetDetailPage = lazy(() => import("./pages/AssetDetailPage"));
const AuditQueuePage = lazy(() => import("./pages/AuditQueuePage"));
const IngestPage = lazy(() => import("./pages/IngestPage"));

function NavItem({ to, children }: { to: string; children: React.ReactNode }) {
  return (
    <NavLink
      to={to}
      className={({ isActive }) =>
        `block rounded px-3 py-2 text-sm font-medium ${
          isActive ? "bg-neutral-800 text-white" : "text-neutral-400 hover:bg-neutral-900 hover:text-neutral-200"
        }`
      }
    >
      {children}
    </NavLink>
  );
}

export default function App() {
  useEventStream();
  const { data: me } = useMe();

  return (
    <div className="flex h-screen">
      <nav className="w-56 shrink-0 border-r border-neutral-800 p-4">
        <div className="mb-6 text-lg font-semibold">branchDAM</div>
        <div className="space-y-1">
          <NavItem to="/assets">Assets</NavItem>
          <NavItem to="/audit">Audit Queue</NavItem>
          <NavItem to="/ingest">Ingest</NavItem>
        </div>
        {me && me.kind === "user" && me.name && (
          <div className="absolute bottom-4 text-xs text-neutral-500">Signed in as {me.name}</div>
        )}
      </nav>
      <main className="flex-1 overflow-auto">
        <Suspense fallback={<div className="p-6 text-neutral-400">Loading…</div>}>
          <Routes>
            <Route path="/" element={<AssetListPage />} />
            <Route path="/assets" element={<AssetListPage />} />
            <Route path="/assets/:id" element={<AssetDetailPage />} />
            <Route path="/audit" element={<AuditQueuePage />} />
            <Route path="/ingest" element={<IngestPage />} />
          </Routes>
        </Suspense>
      </main>
    </div>
  );
}
