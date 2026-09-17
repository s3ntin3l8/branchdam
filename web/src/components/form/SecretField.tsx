import { useState } from "react";
import { INPUT_CLASS } from "./inputStyles";

interface SecretFieldProps {
  hasValue: boolean;
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  // When true, renders a "Generate" button that fills the draft with a
  // fresh cryptographically random value instead of requiring the
  // operator to paste one in. Mirrors SettingsField.generatable.
  generatable?: boolean;
}

// generateSecret returns a 256-bit cryptographically random URL-safe
// string -- the same shape as internal/pairing.mintAPIKey (32 random bytes,
// base64url, padding stripped) so a generated shared-agent-secret and a
// device-pairing key look alike.
function generateSecret(): string {
  const bytes = new Uint8Array(32);
  crypto.getRandomValues(bytes);
  let binary = "";
  for (const b of bytes) binary += String.fromCharCode(b);
  return btoa(binary).replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

// GET /api/v1/settings never returns a secret's value (see
// SettingsFieldDTO.HasValue in internal/httpapi/settings.go) -- there is no
// existing plaintext to show or pre-fill. `value` here is always a fresh
// draft the operator is typing to REPLACE the stored secret; leaving it
// empty and pressing Save is a no-op (SettingsFieldEditor only enables Save
// once the draft is non-empty), not a way to clear the secret. Clearing
// happens through "Revert to config" like any other overridden field.
export function SecretField({ hasValue, value, onChange, disabled, generatable }: SecretFieldProps) {
  const [reveal, setReveal] = useState(false);

  return (
    <div className="flex gap-2">
      <input
        type={reveal ? "text" : "password"}
        value={value}
        onChange={(e) => onChange(e.target.value)}
        disabled={disabled}
        placeholder={hasValue ? "Set -- enter a new value to replace" : "Not set"}
        className={INPUT_CLASS}
      />
      {generatable && (
        <button
          type="button"
          onClick={() => {
            onChange(generateSecret());
            setReveal(true);
          }}
          disabled={disabled}
          className="shrink-0 rounded border border-neutral-800 px-2 text-xs text-neutral-400 hover:text-neutral-100 disabled:opacity-50"
        >
          Generate
        </button>
      )}
      <button
        type="button"
        onClick={() => setReveal((r) => !r)}
        disabled={disabled}
        className="shrink-0 rounded border border-neutral-800 px-2 text-xs text-neutral-400 hover:text-neutral-100 disabled:opacity-50"
      >
        {reveal ? "Hide" : "Show"}
      </button>
    </div>
  );
}
