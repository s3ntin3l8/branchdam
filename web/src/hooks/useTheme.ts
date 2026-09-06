import { useCallback, useEffect, useState } from "react";

export type ThemeMode = "system" | "light" | "dark";
export type ThemeResolved = "light" | "dark";

export const THEME_STORAGE_KEY = "branchdam.theme";

function isThemeMode(value: unknown): value is ThemeMode {
  return value === "system" || value === "light" || value === "dark";
}

function readStoredMode(): ThemeMode {
  try {
    const raw = localStorage.getItem(THEME_STORAGE_KEY);
    if (raw === null) return "system";
    const parsed: unknown = JSON.parse(raw);
    return isThemeMode(parsed) ? parsed : "system";
  } catch {
    return "system";
  }
}

function writeStoredMode(mode: ThemeMode): void {
  try {
    localStorage.setItem(THEME_STORAGE_KEY, JSON.stringify(mode));
  } catch {
    /* storage unavailable (private mode, quota) -- changes are session-only */
  }
}

function systemPrefersDark(): boolean {
  return typeof window !== "undefined"
    && typeof window.matchMedia === "function"
    && window.matchMedia("(prefers-color-scheme: dark)").matches;
}

function applyTheme(resolved: ThemeResolved): void {
  if (typeof document === "undefined") return;
  document.documentElement.setAttribute("data-theme", resolved);
  document.documentElement.style.colorScheme = resolved;
}

interface UseThemeState {
  mode: ThemeMode;
  effective: ThemeResolved;
  setMode: (mode: ThemeMode) => void;
}

/*
 * Hook owning the theme state, the storage round-trip, and the DOM side
 * effect. Call this exactly once at the root of the tree (ThemeProvider);
 * everyone else should read via `useThemeContext()` so there's a single
 * matchMedia listener / storage writer / DOM mutation per change rather
 * than one per consumer.
 *
 * `mode` is what the user picked (`system|light|dark`); `effective` is the
 * concrete theme currently applied to <html data-theme> -- `system` mode
 * resolves to `effective: "dark"` or `"light"` depending on the OS-level
 * prefers-color-scheme media query. The two split apart because SettingsPage
 * needs to show both: the chosen mode AND what's actually being rendered
 * (e.g. "Currently: Light (following system)").
 *
 * Cross-tab sync: a `storage` event fires in other tabs when one tab writes
 * `branchdam.theme`, and we re-read the storage key from there so opening
 * the app in two tabs and changing the theme in one updates the other
 * without a manual refresh. The event doesn't fire in the originating tab,
 * so the local write path stays as-is.
 *
 * Storage key is THEME_STORAGE_KEY ("branchdam.theme"). The matching inline
 * script in index.html reads the same key before React mounts to avoid the
 * first-paint flash of the wrong theme.
 */
export function useThemeState(): UseThemeState {
  const [mode, setModeState] = useState<ThemeMode>(() => readStoredMode());
  const [systemDark, setSystemDark] = useState<boolean>(() => systemPrefersDark());

  // Subscribe to OS color-scheme changes so `system` mode tracks the OS live.
  useEffect(() => {
    if (typeof window === "undefined" || typeof window.matchMedia !== "function") return;
    const mql = window.matchMedia("(prefers-color-scheme: dark)");
    const handler = (event: MediaQueryListEvent) => setSystemDark(event.matches);
    // Older Safari uses addListener/removeListener instead of addEventListener.
    if (typeof mql.addEventListener === "function") {
      mql.addEventListener("change", handler);
      return () => mql.removeEventListener("change", handler);
    }
    mql.addListener(handler);
    return () => mql.removeListener(handler);
  }, []);

  // Cross-tab sync: another tab's localStorage write fires `storage` here
  // (the originating tab does NOT receive its own event, so this only
  // affects the other tabs -- which is what we want).
  useEffect(() => {
    if (typeof window === "undefined") return;
    const handler = (event: StorageEvent) => {
      if (event.key !== THEME_STORAGE_KEY) return;
      setModeState(readStoredMode());
    };
    window.addEventListener("storage", handler);
    return () => window.removeEventListener("storage", handler);
  }, []);

  const effective: ThemeResolved = mode === "light" || mode === "dark" ? mode : (systemDark ? "dark" : "light");

  useEffect(() => {
    applyTheme(effective);
  }, [effective]);

  const setMode = useCallback((next: ThemeMode) => {
    writeStoredMode(next);
    setModeState(next);
  }, []);

  return { mode, effective, setMode };
}
