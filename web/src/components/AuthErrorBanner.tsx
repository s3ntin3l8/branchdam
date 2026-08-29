import { useEffect, useState } from "react";

export function AuthErrorBanner() {
  const [error, setError] = useState<{ status: number; message: string } | null>(null);

  useEffect(() => {
    const handler = (e: Event) => {
      const detail = (e as CustomEvent).detail as { status: number; message: string };
      setError((prev) => {
        if (prev && prev.status === detail.status && prev.message === detail.message) {
          return prev;
        }
        return detail;
      });
    };
    const clearHandler = () => setError(null);
    window.addEventListener("api-auth-error", handler);
    window.addEventListener("api-auth-success", clearHandler);
    return () => {
      window.removeEventListener("api-auth-error", handler);
      window.removeEventListener("api-auth-success", clearHandler);
    };
  }, []);

  if (!error) return null;

  return (
    <div className="fixed top-0 left-0 right-0 z-[100] border-b border-red-800 bg-red-950 p-3 text-center text-sm text-red-200">
      <span className="font-semibold">
        {error.status === 401 ? "Authentication required" : "Access denied"}
      </span>
      {" — "}
      {error.message}
      <button
        type="button"
        onClick={() => setError(null)}
        className="ml-3 text-red-400 hover:text-red-200"
        aria-label="Dismiss"
      >
        &times;
      </button>
    </div>
  );
}
