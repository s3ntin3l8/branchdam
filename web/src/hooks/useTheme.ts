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

interface UseTheme {
  mode: ThemeMode;
  effective: ThemeResolved;
  setMode: (mode: ThemeMode) => void;
}

/*
 * Hook for reading and updating the user's color-theme preference.
 *
 * `mode` is what the user picked (`system|light|dark`); `effective` is the
 * concrete theme currently applied to <html data-theme> -- `system` mode
 * resolves to `effective: "dark"` or `"light"` depending on the OS-level
 * prefers-color-scheme media query. The two split apart because SettingsPage
 * needs to show both: the chosen mode AND what's actually being rendered
 * (e.g. "Currently: Light (following system)").
 *
 * Storage key is THEME_STORAGE_KEY ("branchdam.theme"). The matching inline
 * script in index.html reads the same key before React mounts to avoid the
 * first-paint flash of the wrong theme.
 */
export function useTheme(): UseTheme {
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
