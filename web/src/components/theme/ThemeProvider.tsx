import type { ReactNode } from "react";
import { useTheme } from "../../hooks/useTheme";

/*
 * ThemeProvider exists so the Settings page (and any future theme-aware
 * feature) can subscribe to the same useTheme() state without each consumer
 * instantiating its own hook and re-binding localStorage/matchMedia handlers.
 *
 * It deliberately renders no markup -- the active theme is applied to
 * <html data-theme> as a side effect inside useTheme(), not via React's
 * tree, so there's nothing to mount here. The "provider" name is just the
 * conventional hook-call boundary.
 */
export function ThemeProvider({ children }: { children: ReactNode }) {
  useTheme();
  return <>{children}</>;
}
