import { describe, expect, it, beforeEach, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ThemeContext } from "../../hooks/themeContext";
import { ThemeSwitcher } from "./ThemeSwitcher";
import type { ThemeMode, ThemeResolved } from "../../hooks/useTheme";

/*
 * Component-level tests for the segmented theme control. The hook
 * (`useTheme.test.tsx`) already covers localStorage persistence and the
 * data-theme side effect; here we assert the visible UI surface -- the
 * three options, their aria-pressed states, and that clicks invoke the
 * setMode callback wired through the provider.
 *
 * We wrap each render in a ThemeContext.Provider with a stub value rather
 * than calling the real useThemeState() -- the hook's own tests are the
 * right place to assert side effects; here we just want to verify the
 * switcher hands the right value to setMode and reflects the provider's
 * mode in its aria-pressed state.
 */

beforeEach(() => {
  try {
    localStorage.clear();
  } catch {
    /* jsdom storage may be unavailable in some configurations */
  }
  document.documentElement.removeAttribute("data-theme");
});

function renderWithContext(mode: ThemeMode, effective: ThemeResolved, setMode: (m: ThemeMode) => void) {
  return render(
    <ThemeContext.Provider value={{ mode, effective, setMode }}>
      <ThemeSwitcher />
    </ThemeContext.Provider>,
  );
}

describe("ThemeSwitcher", () => {
  it("renders System / Light / Dark options inside a labelled radiogroup", () => {
    renderWithContext("system", "dark", vi.fn());

    const group = screen.getByRole("radiogroup", { name: /color theme/i });
    expect(group).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /^System theme/i })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /^Light theme/i })).toBeInTheDocument();
    expect(screen.getByRole("radio", { name: /^Dark theme/i })).toBeInTheDocument();
  });

  it("marks the currently-active option with aria-checked=true", () => {
    renderWithContext("system", "dark", vi.fn());

    expect(screen.getByRole("radio", { name: /^System theme/i })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("radio", { name: /^Light theme/i })).toHaveAttribute("aria-checked", "false");
    expect(screen.getByRole("radio", { name: /^Dark theme/i })).toHaveAttribute("aria-checked", "false");
  });

  it("clicking each option invokes setMode with the right value", async () => {
    const user = userEvent.setup();
    const setMode = vi.fn();
    renderWithContext("system", "light", setMode);

    await user.click(screen.getByRole("radio", { name: /^Light theme/i }));
    expect(setMode).toHaveBeenLastCalledWith("light");

    await user.click(screen.getByRole("radio", { name: /^Dark theme/i }));
    expect(setMode).toHaveBeenLastCalledWith("dark");

    await user.click(screen.getByRole("radio", { name: /^System theme/i }));
    expect(setMode).toHaveBeenLastCalledWith("system");

    expect(setMode).toHaveBeenCalledTimes(3);
  });

  it("reflects the provider's mode across re-renders", () => {
    const setMode = vi.fn();
    const { rerender } = renderWithContext("system", "dark", setMode);
    expect(screen.getByRole("radio", { name: /^System theme/i })).toHaveAttribute("aria-checked", "true");

    rerender(
      <ThemeContext.Provider value={{ mode: "light", effective: "light", setMode }}>
        <ThemeSwitcher />
      </ThemeContext.Provider>,
    );
    expect(screen.getByRole("radio", { name: /^Light theme/i })).toHaveAttribute("aria-checked", "true");
    expect(screen.getByRole("radio", { name: /^System theme/i })).toHaveAttribute("aria-checked", "false");
  });
});
