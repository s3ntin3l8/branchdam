import { useState } from "react";
import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useNavigate } from "react-router";
import { api } from "../api/client";

// MfaSetupPage handles the TOTP enrollment flow:
// 1. On mount, POST /mfa/setup to get the otpauth URI + QR
// 2. User scans QR, enters a TOTP code
// 3. POST /mfa/enable with the code -> shows recovery codes once
export default function MfaSetupPage() {
  const queryClient = useQueryClient();
  const navigate = useNavigate();

  const [step, setStep] = useState<"loading" | "scan" | "verify" | "done">("loading");
  const [otpauthURI, setOtpauthURI] = useState("");
  const [recoveryCodes, setRecoveryCodes] = useState<string[]>([]);
  const [code, setCode] = useState("");
  const [error, setError] = useState<string | null>(null);

  const setupMutation = useMutation({
    mutationFn: api.mfaSetup,
    onSuccess: (data) => {
      setOtpauthURI(data.otpauthURI);
      setStep("scan");
    },
    onError: (err: Error) => {
      setError(err.message);
    },
  });

  const enableMutation = useMutation({
    mutationFn: api.mfaEnable,
    onSuccess: (data) => {
      setRecoveryCodes(data.recoveryCodes);
      setStep("done");
      void queryClient.invalidateQueries({ queryKey: ["me"] });
    },
    onError: (err: Error) => {
      setError(err.message);
    },
  });

  // Trigger setup on first render.
  if (step === "loading" && !setupMutation.isPending && !error) {
    setupMutation.mutate();
  }

  if (step === "loading" && setupMutation.isPending) {
    return (
      <div className="flex min-h-screen items-center justify-center bg-neutral-950 text-neutral-400">
        Setting up MFA…
      </div>
    );
  }

  if (error && step === "loading") {
    return (
      <div className="flex min-h-screen items-center justify-center bg-neutral-950">
        <div className="w-full max-w-sm rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow">
          <p className="text-sm text-red-400">{error}</p>
          <button
            onClick={() => navigate("/users")}
            className="mt-4 rounded bg-neutral-800 px-3 py-1.5 text-sm text-neutral-200 hover:bg-neutral-700"
          >
            Back to users
          </button>
        </div>
      </div>
    );
  }

  if (step === "scan") {
    return (
      <div className="flex min-h-screen items-center justify-center bg-neutral-950">
        <div className="w-full max-w-sm rounded-lg border border-neutral-800 bg-neutral-900 p-6 shadow">
          <h1 className="mb-2 text-lg font-semibold text-neutral-100">Set up MFA</h1>
          <p className="mb-4 text-sm text-neutral-400">
            Scan this URI with your authenticator app:
          </p>
          <div className="mb-4 rounded border border-neutral-700 bg-neutral-800 p-3">
            <code className="break-all text-xs text-neutral-300">{otpauthURI}</code>
          </div>
          <p className="mb-2 text-xs text-neutral-400">
            After scanning, enter the 6-digit code to verify:
          </p>
          <form
            onSubmit={(e) => {
              e.preventDefault();
              if (!code) return;
              setError(null);
              enableMutation.mutate({ code });
            }}
            className="space-y-3"
          >
            <label className="block">
              <input
                type="text"
                required
                autoFocus
                autoComplete="one-time-code"
                value={code}
                onChange={(e) => setCode(e.target.value)}
                placeholder="123456"
                className="w-full rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-sm text-neutral-100"
              />
            </label>
            {error ? <p className="text-xs text-red-400">{error}</p> : null}
            <button
              type="submit"
              disabled={enableMutation.isPending || !code}
              className="w-full rounded bg-brand px-3 py-1.5 text-sm font-medium text-white hover:bg-brand-dark disabled:opacity-50"
            >
              {enableMutation.isPending ? "Verifying…" : "Enable MFA"}
            </button>
          </form>
          <button
            onClick={() => navigate("/users")}
            className="mt-3 w-full text-xs text-neutral-400 hover:text-neutral-200"
          >
            Cancel
          </button>
        </div>
      </div>
    );
  }

  // step === "done" — show recovery codes
  return (
    <div className="flex min-h-screen items-center justify-center bg-neutral-950">
      <div className="w-full max-w-sm rounded-lg border border-emerald-800 bg-neutral-900 p-6 shadow">
        <h1 className="mb-2 text-lg font-semibold text-emerald-300">MFA enabled</h1>
        <p className="mb-4 text-sm text-neutral-400">
          Save these recovery codes in a safe place. They will not be shown again.
        </p>
        <div className="mb-4 rounded border border-neutral-700 bg-neutral-800 p-3">
          <div className="grid grid-cols-2 gap-1">
            {recoveryCodes.map((c) => (
              <code key={c} className="text-xs text-neutral-300">{c}</code>
            ))}
          </div>
        </div>
        <button
          onClick={() => navigate("/users")}
          className="w-full rounded bg-brand px-3 py-1.5 text-sm font-medium text-white hover:bg-brand-dark"
        >
          Done
        </button>
      </div>
    </div>
  );
}
