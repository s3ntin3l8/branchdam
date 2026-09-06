import { useThemeContext } from "../../hooks/themeContext";
import type { ThemeMode } from "../../hooks/useTheme";

interface ThemeSwitcherProps {
  className?: string;
}

/*
 * Three-button segmented control for the user's color-theme preference.
 * Renders System / Light / Dark, with the currently-selected option visually
 * distinguished and aria-pressed=true for assistive tech. State is owned by
 * the ThemeProvider; changing the selection here flows through the same
 * <html data-theme> write the provider owns.
 *
 * Reads via useThemeContext() rather than useTheme() so the
 * matchMedia/storage side effects stay centralized in the provider.
 *
 * Local preference only: writes to localStorage, never to the server. The
 * matching inline script in index.html reads the same storage key before
 * React mounts so first paint already reflects the choice.
 */
export function ThemeSwitcher({ className }: ThemeSwitcherProps) {
  const { mode, setMode } = useThemeContext();

  const options: Array<{ value: ThemeMode; label: string; hint: string }> = [
    { value: "system", label: "System", hint: "Follow your operating system" },
    { value: "light", label: "Light", hint: "Always use the light theme" },
    { value: "dark", label: "Dark", hint: "Always use the dark theme" },
  ];

  return (
    <div
      role="radiogroup"
      aria-label="Color theme"
      className={`inline-flex rounded-lg border border-neutral-800 bg-panel p-1 ${className ?? ""}`}
    >
      {options.map((opt) => {
        const active = mode === opt.value;
        return (
          <button
            key={opt.value}
            type="button"
            role="radio"
            aria-checked={active}
            aria-label={`${opt.label} theme (${opt.hint})`}
            onClick={() => setMode(opt.value)}
            data-active={active}
            data-theme-option={opt.value}
            className={`rounded px-3 py-1 text-xs font-medium transition-colors ${
              active
                ? "bg-neutral-700 text-neutral-100 shadow"
                : "text-neutral-400 hover:bg-neutral-800 hover:text-neutral-200"
            }`}
          >
            {opt.label}
          </button>
        );
      })}
    </div>
  );
}
