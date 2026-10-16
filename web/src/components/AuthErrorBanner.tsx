import { useEffect, useState } from "react";
import {
  API_AUTH_ERROR_EVENT,
  API_AUTH_SUCCESS_EVENT,
  type ApiAuthErrorDetail,
} from "../api/client";

// AuthErrorBanner is the global "your session is gone" strip. It shows only
// for a 401: a 403 is a per-page "not allowed" answer (an admin-only list
// for a non-admin, a CSRF refusal) that the page renders inline, and a
// signed-out local/both-mode session is redirected to /login by Layout
// instead. It clears when /api/v1/me next confirms a signed-in session,
// not on any 2xx, so a parallel query that succeeds a moment after a 401
// can't make it flicker.
export function AuthErrorBanner() {
  const [error, setError] = useState<{ message: string } | null>(null);

  useEffect(() => {
    const handler = (e: Event) => {
      const detail = (e as CustomEvent<ApiAuthErrorDetail>).detail;
      if (!detail || detail.status !== 401) return;
      setError((prev) => (prev && prev.message === detail.message ? prev : { message: detail.message }));
    };
    const clearHandler = () => setError(null);
    window.addEventListener(API_AUTH_ERROR_EVENT, handler);
    window.addEventListener(API_AUTH_SUCCESS_EVENT, clearHandler);
    return () => {
      window.removeEventListener(API_AUTH_ERROR_EVENT, handler);
      window.removeEventListener(API_AUTH_SUCCESS_EVENT, clearHandler);
    };
  }, []);

  if (!error) return null;

  return (
    <div
      role="alert"
      className="fixed top-0 left-0 right-0 z-[100] border-b border-red-800 bg-red-950 p-3 text-center text-sm text-red-300"
    >
      <span className="font-semibold">Authentication required</span>
      {" — "}
      {error.message}
      <button
        type="button"
        onClick={() => setError(null)}
        className="ml-3 text-red-400 hover:text-red-300"
        aria-label="Dismiss"
      >
        &times;
      </button>
    </div>
  );
}
