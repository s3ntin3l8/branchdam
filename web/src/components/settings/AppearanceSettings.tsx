import { useTheme } from "../../hooks/useTheme";
import { ThemeSwitcher } from "./ThemeSwitcher";

/*
 * Client-only settings section for the user's color-theme preference.
 *
 * Does NOT route through useSettings()/usePutSettings() -- appearance is a
 * per-browser UI preference stored in localStorage, not a server-side
 * config value, so it lives outside the SettingsField pipeline entirely.
 * Keeping it client-side also means it works for read-only users (e.g. an
 * operator signed in via Authentik but without admin groups) who get a 403
 * on GET /api/v1/settings.
 *
 * Renders the ThemeSwitcher plus a live "currently applied" hint so users
 * in `system` mode can see what the OS preference currently resolves to.
 */
export function AppearanceSettings() {
  const { mode, effective } = useTheme();

  let currentHint: string;
  if (mode === "system") {
    currentHint = effective === "dark"
      ? "Currently: Dark (following system)"
      : "Currently: Light (following system)";
  } else if (mode === "light") {
    currentHint = "Currently: Light (manual)";
  } else {
    currentHint = "Currently: Dark (manual)";
  }

  return (
    <div className="rounded-lg border border-neutral-800 bg-neutral-900/50 p-4 mb-4">
      <h3 className="mb-2 text-sm font-semibold uppercase tracking-wider text-neutral-400">Theme</h3>
      <p className="mb-3 text-sm text-neutral-200">
        Choose how the interface should look. Light, dark, or follow your operating system.
      </p>
      <ThemeSwitcher />
      <p className="mt-3 text-xs text-neutral-500" data-theme-current-hint>
        {currentHint}
      </p>
    </div>
  );
}
