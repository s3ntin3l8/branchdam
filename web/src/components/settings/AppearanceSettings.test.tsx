import { describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { AppearanceSettings } from "./AppearanceSettings";
import { ThemeContext } from "../../hooks/themeContext";
import type { ThemeMode, ThemeResolved } from "../../hooks/useTheme";

function renderWith(mode: ThemeMode, effective: ThemeResolved, setMode = vi.fn()) {
  render(
    <ThemeContext.Provider value={{ mode, effective, setMode }}>
      <AppearanceSettings />
    </ThemeContext.Provider>,
  );
  return setMode;
}

describe("AppearanceSettings", () => {
  it.each([
    ["system", "dark", "Currently: Dark (following system)"],
    ["system", "light", "Currently: Light (following system)"],
    ["light", "light", "Currently: Light (manual)"],
    ["dark", "dark", "Currently: Dark (manual)"],
  ] as const)("mode %s resolving to %s reads %j", (mode, effective, hint) => {
    renderWith(mode, effective);
    expect(screen.getByText(hint)).toBeInTheDocument();
  });

  it("changes the mode through the switcher", async () => {
    const setMode = renderWith("system", "dark");
    await userEvent.click(screen.getByRole("radio", { name: /^Light theme/i }));
    expect(setMode).toHaveBeenCalledWith("light");
  });
});
