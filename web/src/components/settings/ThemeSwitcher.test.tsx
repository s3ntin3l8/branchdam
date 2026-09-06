import { describe, expect, it, beforeEach, vi } from "vitest";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { ThemeSwitcher } from "./ThemeSwitcher";

/*
 * Component-level tests for the segmented theme control. The hook
 * (`useTheme.test.tsx`) already covers localStorage persistence and the
 * data-theme side effect; here we assert the visible UI surface -- the
 * three options, their aria-pressed states, and that clicks invoke the
 * hook with the right value.
 */

beforeEach(() => {
  try {
    localStorage.clear();
  } catch {
    /* jsdom storage may be unavailable in some configurations */
  }
  document.documentElement.removeAttribute("data-theme");
  // matchMedia isn't implemented in jsdom; the hook handles that path,
  // but ThemeSwitcher's render calls it through useTheme(), so install a
  // minimal stub.
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  (window as any).matchMedia = vi.fn().mockImplementation((query: string) => ({
    matches: false,
    media: query,
    addEventListener: () => {},
    removeEventListener: () => {},
    addListener: () => {},
    removeListener: () => {},
  }));
});

describe("ThemeSwitcher", () => {
  it("renders System / Light / Dark options inside a labelled group", () => {
    render(<ThemeSwitcher />);

    const group = screen.getByRole("group", { name: /color theme/i });
    expect(group).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^System theme/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Light theme/i })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /^Dark theme/i })).toBeInTheDocument();
  });

  it("marks the currently-active option with aria-pressed=true", () => {
    render(<ThemeSwitcher />);

    // Default mode is `system` (no stored preference)
    expect(screen.getByRole("button", { name: /^System theme/i })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: /^Light theme/i })).toHaveAttribute("aria-pressed", "false");
    expect(screen.getByRole("button", { name: /^Dark theme/i })).toHaveAttribute("aria-pressed", "false");
  });

  it("clicking Light marks Light as active and persists", async () => {
    const user = userEvent.setup();
    render(<ThemeSwitcher />);

    await user.click(screen.getByRole("button", { name: /^Light theme/i }));

    expect(screen.getByRole("button", { name: /^Light theme/i })).toHaveAttribute("aria-pressed", "true");
    expect(screen.getByRole("button", { name: /^System theme/i })).toHaveAttribute("aria-pressed", "false");
    expect(JSON.parse(localStorage.getItem("branchdam.theme") ?? "null")).toBe("light");
  });

  it("clicking Dark marks Dark as active and persists", async () => {
    const user = userEvent.setup();
    render(<ThemeSwitcher />);

    await user.click(screen.getByRole("button", { name: /^Dark theme/i }));

    expect(screen.getByRole("button", { name: /^Dark theme/i })).toHaveAttribute("aria-pressed", "true");
    expect(JSON.parse(localStorage.getItem("branchdam.theme") ?? "null")).toBe("dark");
  });
});
