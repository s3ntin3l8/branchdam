import { useEffect, useId, useRef, useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { Link, useNavigate } from "react-router";
import { api } from "../api/client";
import type { MfaSetupResponse } from "../api/types";
import { otpauthSecret } from "../lib/otpauth";

// MfaSetupPage handles the TOTP enrollment flow for the signed-in local
// user (any user, not only admins -- the server routes are self-service):
// 1. On mount, POST /mfa/setup to get the otpauth URI and its QR code
// 2. User scans the QR (or types the key), enters a TOTP code
// 3. POST /mfa/enable with the code -> shows recovery codes once
//
// /mfa/setup mints a new pending secret on every call, so it must run
// exactly once per visit. It is started from an effect guarded by a ref:
// StrictMode runs effects twice in development, and calling mutate()
// during render (as an earlier version did) fired it on every render
// pass, minting a second secret the QR on screen no longer matched.
export default function MfaSetupPage() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();

  const [setup, setSetup] = useState<MfaSetupResponse | null>(null);
  const [recoveryCodes, setRecoveryCodes] = useState<string[] | null>(null);
  const [code, setCode] = useState("");
  const [showKey, setShowKey] = useState(false);
  const codeId = useId();

  const setupMutation = useMutation({
    mutationFn: api.mfaSetup,
    onSuccess: (data) => setSetup(data),
  });

  const enableMutation = useMutation({
    mutationFn: api.mfaEnable,
    onSuccess: (data) => {
      setRecoveryCodes(data.recoveryCodes);
      void queryClient.invalidateQueries({ queryKey: ["me"] });
    },
  });

  const started = useRef(false);
  const startSetup = setupMutation.mutate;
  useEffect(() => {
    if (started.current) return;
    started.current = true;
    startSetup();
  }, [startSetup]);

  if (setupMutation.isError) {
    return (
      <div className="flex min-h-full items-center justify-center p-4">
        <div className="w-full max-w-sm rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow">
          <h1 className="mb-2 text-lg font-semibold text-neutral-100">Set up MFA</h1>
          <p role="alert" className="text-sm text-red-400">
            {setupMutation.error.message}
          </p>
          <Link
            to="/account"
            className="mt-4 inline-block rounded bg-neutral-800 px-3 py-1.5 text-sm text-neutral-200 hover:bg-neutral-700"
          >
            Back to account
          </Link>
        </div>
      </div>
    );
  }

  if (!setup) {
    return (
      <div className="flex min-h-full items-center justify-center p-4 text-neutral-400">
        Setting up MFA…
      </div>
    );
  }

  if (recoveryCodes) {
    return (
      <div className="flex min-h-full items-center justify-center p-4">
        <div className="w-full max-w-sm rounded-lg border border-emerald-800 bg-neutral-900 p-6 shadow">
          <h1 className="mb-2 text-lg font-semibold text-emerald-300">MFA enabled</h1>
          <p className="mb-4 text-sm text-neutral-400">
            Save these recovery codes in a safe place. Each one signs you in once if you lose your
            authenticator. They will not be shown again.
          </p>
          <div className="mb-4 rounded border border-neutral-700 bg-neutral-800 p-3">
            <ul className="grid grid-cols-1 gap-1 sm:grid-cols-2" aria-label="Recovery codes">
              {recoveryCodes.map((c) => (
                <li key={c}>
                  <code className="text-xs text-neutral-300">{c}</code>
                </li>
              ))}
            </ul>
          </div>
          <button
            type="button"
            onClick={() => navigate("/account")}
            className="w-full rounded bg-brand-dark px-3 py-1.5 text-sm font-medium text-white hover:bg-indigo-700"
          >
            Done
          </button>
        </div>
      </div>
    );
  }

  const secret = otpauthSecret(setup.otpauthURI);
  return (
    <div className="flex min-h-full items-center justify-center p-4">
      <div className="w-full max-w-sm rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow">
        <h1 className="mb-2 text-lg font-semibold text-neutral-100">Set up MFA</h1>
        <p className="mb-4 text-sm text-neutral-400">
          Scan this QR code with your authenticator app.
        </p>
        {setup.qrSvg ? (
          <img
            alt="MFA QR code"
            width={200}
            height={200}
            src={`data:image/svg+xml;charset=utf-8,${encodeURIComponent(setup.qrSvg)}`}
            className="mx-auto mb-3 rounded bg-white"
          />
        ) : null}
        <button
          type="button"
          onClick={() => setShowKey((v) => !v)}
          aria-expanded={showKey || !setup.qrSvg}
          className="mb-3 text-xs text-neutral-400 underline hover:text-neutral-200"
        >
          {setup.qrSvg ? "Can't scan it? Enter a key instead" : "Enter this key in your authenticator app"}
        </button>
        {showKey || !setup.qrSvg ? (
          <div className="mb-4 space-y-2 rounded border border-neutral-700 bg-neutral-800 p-3">
            {secret ? (
              <p className="text-xs text-neutral-400">
                Key:{" "}
                <code data-testid="mfa-secret" className="break-all text-neutral-200">
                  {secret}
                </code>
              </p>
            ) : null}
            <p className="text-xs text-neutral-400">
              Or the full URI:{" "}
              <code className="break-all text-neutral-300">{setup.otpauthURI}</code>
            </p>
          </div>
        ) : null}
        <form
          onSubmit={(e) => {
            e.preventDefault();
            if (!code.trim()) return;
            enableMutation.mutate({ code: code.trim() });
          }}
          className="space-y-3"
        >
          <div>
            <label htmlFor={codeId} className="block text-xs text-neutral-400">
              Then enter the 6-digit code it shows
            </label>
            <input
              id={codeId}
              type="text"
              required
              autoFocus
              inputMode="numeric"
              autoComplete="one-time-code"
              value={code}
              onChange={(e) => setCode(e.target.value)}
              placeholder="123456"
              className="mt-1 w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
            />
          </div>
          {enableMutation.isError ? (
            <p role="alert" className="text-xs text-red-400">
              {enableMutation.error.message}
            </p>
          ) : null}
          <button
            type="submit"
            disabled={enableMutation.isPending || !code.trim()}
            className="w-full rounded bg-brand-dark px-3 py-1.5 text-sm font-medium text-white hover:bg-indigo-700 disabled:opacity-50"
          >
            {enableMutation.isPending ? "Verifying…" : "Enable MFA"}
          </button>
        </form>
        <Link
          to="/account"
          className="mt-3 block w-full text-center text-xs text-neutral-400 hover:text-neutral-200"
        >
          Cancel
        </Link>
      </div>
    </div>
  );
}
