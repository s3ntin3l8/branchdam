import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ThemeProvider } from "./ThemeProvider";
import { useThemeContext } from "../../hooks/themeContext";
import { THEME_STORAGE_KEY } from "../../hooks/useTheme";

function Probe() {
  const { mode, effective, setMode } = useThemeContext();
  return (
    <div>
      <span data-testid="mode">{mode}</span>
      <span data-testid="effective">{effective}</span>
      <button type="button" onClick={() => setMode("light")}>
        light
      </button>
    </div>
  );
}

beforeEach(() => {
  localStorage.clear();
  document.documentElement.removeAttribute("data-theme");
  // System prefers dark.
  vi.stubGlobal(
    "matchMedia",
    vi.fn(() => ({
      matches: true,
      addEventListener: vi.fn(),
      removeEventListener: vi.fn(),
    })),
  );
});

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("ThemeProvider", () => {
  it("provides the stored mode and applies the resolved theme to <html>", () => {
    render(
      <ThemeProvider>
        <Probe />
      </ThemeProvider>,
    );
    expect(screen.getByTestId("mode")).toHaveTextContent("system");
    expect(screen.getByTestId("effective")).toHaveTextContent("dark");
    expect(document.documentElement).toHaveAttribute("data-theme", "dark");
  });

  it("shares one state with every consumer and persists changes", async () => {
    render(
      <ThemeProvider>
        <Probe />
        <Probe />
      </ThemeProvider>,
    );
    await userEvent.click(screen.getAllByRole("button", { name: "light" })[0]);

    for (const el of screen.getAllByTestId("mode")) expect(el).toHaveTextContent("light");
    expect(document.documentElement).toHaveAttribute("data-theme", "light");
    expect(localStorage.getItem(THEME_STORAGE_KEY)).toBe(JSON.stringify("light"));
  });

  it("useThemeContext throws outside a provider", () => {
    const spy = vi.spyOn(console, "error").mockImplementation(() => {});
    expect(() => render(<Probe />)).toThrow(/within a ThemeProvider/);
    spy.mockRestore();
  });
});
