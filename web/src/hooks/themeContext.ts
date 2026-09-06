import { createContext, useContext } from "react";
import type { ThemeMode, ThemeResolved } from "./useTheme";

interface ThemeContextValue {
  mode: ThemeMode;
  effective: ThemeResolved;
  setMode: (mode: ThemeMode) => void;
}

/*
 * Single source of truth for the active color theme. useTheme() (which owns
 * the matchMedia subscription, localStorage write, and <html data-theme>
 * side effect) is called exactly once at the ThemeProvider level, and every
 * consumer reads through this context.
 *
 * The earlier shape -- two independent useTheme() call sites -- would have
 * created two matchMedia listeners, two storage write paths, and two
 * <html> writes per change. Centralizing via context means there is one
 * subscription, one storage writer, and one DOM side effect for the whole
 * tree. Consumers (the settings switcher, the appearance section, anything
 * else that needs to read the theme) just read context.
 */

export const ThemeContext = createContext<ThemeContextValue | null>(null);

export function useThemeContext(): ThemeContextValue {
  const value = useContext(ThemeContext);
  if (!value) {
    throw new Error("useThemeContext must be used within a ThemeProvider");
  }
  return value;
}
