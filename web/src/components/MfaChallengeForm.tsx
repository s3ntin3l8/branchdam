import { useState } from "react";

// MfaChallengeForm renders the TOTP / recovery-code input used by both
// the login flow (LoginPage) and the reload-during-challenge path
// (Layout). Extracted here so both consumers share the same UI.
export default function MfaChallengeForm({
  onSubmit,
  pending,
  error,
}: {
  onSubmit: (code: string) => void;
  pending: boolean;
  error: string | undefined;
}) {
  const [code, setCode] = useState("");

  return (
    <>
      <h1 className="mb-2 text-lg font-semibold text-neutral-100">Two-factor authentication</h1>
      <p className="mb-4 text-sm text-neutral-400">
        Enter the 6-digit code from your authenticator app, or a recovery code.
      </p>
      <form
        onSubmit={(e) => {
          e.preventDefault();
          if (!code) return;
          onSubmit(code);
        }}
        className="space-y-3"
      >
        <label className="block">
          <span className="text-xs text-neutral-400">Verification code</span>
          <input
            type="text"
            required
            autoFocus
            autoComplete="one-time-code"
            value={code}
            onChange={(e) => setCode(e.target.value)}
            placeholder="123456"
            className="mt-1 w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
          />
        </label>
        {error ? <p className="text-xs text-red-400">{error}</p> : null}
        <button
          type="submit"
          disabled={pending || !code}
          className="w-full rounded bg-brand-dark px-3 py-1.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:opacity-50"
        >
          {pending ? "Verifying…" : "Verify"}
        </button>
      </form>
    </>
  );
}
