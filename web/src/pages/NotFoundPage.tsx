import { Link } from "react-router";

// NotFoundPage is the router's catch-all ("*") inside the Layout shell, so
// a mistyped or stale URL keeps the sidebar and offers a way back instead
// of rendering an empty main pane.
export default function NotFoundPage() {
  return (
    <div className="p-6">
      <h1 className="mb-2 text-lg font-semibold text-neutral-100">Page not found</h1>
      <p className="mb-4 text-sm text-neutral-400">There is nothing at this address.</p>
      <Link to="/" className="text-sm text-brand hover:underline">
        Go to assets
      </Link>
    </div>
  );
}
