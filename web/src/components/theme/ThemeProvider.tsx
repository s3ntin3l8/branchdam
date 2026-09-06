import { type ReactNode, useMemo } from "react";
import { useThemeState } from "../../hooks/useTheme";
import { ThemeContext } from "../../hooks/themeContext";

/*
 * ThemeProvider exists so the entire React tree reads theme state from a
 * single source: useThemeState() is called exactly once here (not in each
 * consumer), and the resulting value flows through ThemeContext. Without
 * this, every consumer that called useTheme() would create its own
 * matchMedia listener and its own DOM side effect on every re-render.
 *
 * The provider renders no markup -- the active theme is applied to
 * <html data-theme> as a side effect inside useThemeState(), not via
 * React's tree, so there's nothing to mount here. The "provider" name is
 * just the conventional hook-call boundary.
 */
export function ThemeProvider({ children }: { children: ReactNode }) {
  const value = useThemeState();
  // Memoize by the stable fields, not the wrapper object, so consumers
  // re-render only when the actual theme state changes -- not on every
  // provider re-run. `value` itself changes identity every render; using
    // it as a dep would defeat the memo.
  // eslint-disable-next-line react-hooks/exhaustive-deps
  const memo = useMemo(() => value, [value.mode, value.effective, value.setMode]);
  return <ThemeContext.Provider value={memo}>{children}</ThemeContext.Provider>;
}
