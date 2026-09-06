import { describe, expect, it, beforeEach, vi, afterEach } from "vitest";
import { act, renderHook } from "@testing-library/react";
import { THEME_STORAGE_KEY, useThemeState } from "./useTheme";

/*
 * Unit tests for the useThemeState() hook. The hook is the only thing that
 * writes localStorage and applies the data-theme attribute to <html>, so
 * these tests cover both the persistence path and the side-effect path in
 * one place -- no need to duplicate them in the switcher / settings tests.
 *
 * matchMedia is mocked because jsdom doesn't implement it; the mock is
 * controllable per-test so we can drive system-mode resolution without
 * touching a real OS-level preference.
 */

type Listener = (event: { matches: boolean }) => void;

class MatchMediaStub {
  matches: boolean;
  media: string;
  listeners: Listener[] = [];
  constructor(media: string) {
    this.media = media;
    this.matches = false;
  }
  addEventListener(_: string, listener: Listener) {
    this.listeners.push(listener);
  }
  removeEventListener(_: string, listener: Listener) {
    this.listeners = this.listeners.filter((l) => l !== listener);
  }
  // Old API surface that some browsers still use; useThemeState falls back
  // to it when addEventListener isn't present.
  addListener(listener: Listener) {
    this.listeners.push(listener);
  }
  removeListener(listener: Listener) {
    this.listeners = this.listeners.filter((l) => l !== listener);
  }
  dispatch(matches: boolean) {
    this.matches = matches;
    for (const l of this.listeners) l({ matches });
  }
}

function installMatchMedia(initialMatches: boolean): MatchMediaStub {
  const stub = new MatchMediaStub("(prefers-color-scheme: dark)");
  stub.matches = initialMatches;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  (window as any).matchMedia = vi.fn().mockImplementation(() => stub);
  return stub;
}

function clearStorage(): void {
  try {
    localStorage.clear();
  } catch {
    /* jsdom storage may be unavailable in some configurations */
  }
}

beforeEach(() => {
  clearStorage();
  document.documentElement.removeAttribute("data-theme");
  document.documentElement.style.colorScheme = "";
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("useThemeState", () => {
  it("defaults to system mode when storage is empty", () => {
    installMatchMedia(false);
    const { result } = renderHook(() => useThemeState());

    expect(result.current.mode).toBe("system");
    expect(result.current.effective).toBe("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("defaults to dark effective when system prefers dark and mode is system", () => {
    installMatchMedia(true);
    const { result } = renderHook(() => useThemeState());

    expect(result.current.mode).toBe("system");
    expect(result.current.effective).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("reads an existing persisted mode on mount", () => {
    installMatchMedia(false);
    localStorage.setItem(THEME_STORAGE_KEY, JSON.stringify("light"));

    const { result } = renderHook(() => useThemeState());

    expect(result.current.mode).toBe("light");
    expect(result.current.effective).toBe("light");
  });

  it("ignores malformed storage values and falls back to system", () => {
    installMatchMedia(false);
    localStorage.setItem(THEME_STORAGE_KEY, "not json");

    const { result } = renderHook(() => useThemeState());

    expect(result.current.mode).toBe("system");
  });

  it("ignores storage values that aren't a valid ThemeMode and falls back to system", () => {
    installMatchMedia(false);
    localStorage.setItem(THEME_STORAGE_KEY, JSON.stringify("hotpink"));

    const { result } = renderHook(() => useThemeState());

    expect(result.current.mode).toBe("system");
  });

  it("setMode('light') applies data-theme=light and persists", () => {
    installMatchMedia(true);
    const { result } = renderHook(() => useThemeState());

    act(() => result.current.setMode("light"));

    expect(result.current.mode).toBe("light");
    expect(result.current.effective).toBe("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
    expect(document.documentElement.style.colorScheme).toBe("light");
    expect(JSON.parse(localStorage.getItem(THEME_STORAGE_KEY) ?? "null")).toBe("light");
  });

  it("setMode('dark') applies data-theme=dark and persists", () => {
    installMatchMedia(false);
    const { result } = renderHook(() => useThemeState());

    act(() => result.current.setMode("dark"));

    expect(result.current.mode).toBe("dark");
    expect(result.current.effective).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
    expect(JSON.parse(localStorage.getItem(THEME_STORAGE_KEY) ?? "null")).toBe("dark");
  });

  it("setMode('system') re-resolves to OS preference", () => {
    const mql = installMatchMedia(true);
    const { result } = renderHook(() => useThemeState());
    expect(result.current.effective).toBe("dark");

    // First lock to light, then go back to system with OS=light.
    act(() => result.current.setMode("light"));
    act(() => mql.dispatch(false));
    act(() => result.current.setMode("system"));

    expect(result.current.mode).toBe("system");
    expect(result.current.effective).toBe("light");
    expect(document.documentElement.getAttribute("data-theme")).toBe("light");
  });

  it("tracks the OS preference live while in system mode", () => {
    const mql = installMatchMedia(false);
    const { result } = renderHook(() => useThemeState());
    expect(result.current.effective).toBe("light");

    act(() => mql.dispatch(true));
    expect(result.current.mode).toBe("system");
    expect(result.current.effective).toBe("dark");
    expect(document.documentElement.getAttribute("data-theme")).toBe("dark");
  });

  it("does not follow OS preference while a manual mode is set", () => {
    const mql = installMatchMedia(false);
    const { result } = renderHook(() => useThemeState());

    act(() => result.current.setMode("light"));
    expect(result.current.effective).toBe("light");

    act(() => mql.dispatch(true));
    expect(result.current.mode).toBe("light");
    expect(result.current.effective).toBe("light");
  });

  it("falls back to system on invalid storage and resolves effective from OS preference", () => {
    installMatchMedia(true);
    localStorage.setItem(THEME_STORAGE_KEY, JSON.stringify("hotpink"));

    const { result } = renderHook(() => useThemeState());

    expect(result.current.mode).toBe("system");
    expect(result.current.effective).toBe("dark");
  });

  it("removes the matchMedia listener on unmount", () => {
    const mql = installMatchMedia(false);
    const { unmount } = renderHook(() => useThemeState());
    expect(mql.listeners.length).toBe(1);

    unmount();
    expect(mql.listeners.length).toBe(0);
  });

  it("removes the storage listener on unmount", () => {
    const removeSpy = vi.spyOn(window, "removeEventListener");
    const { unmount } = renderHook(() => useThemeState());
    unmount();

    const removed = removeSpy.mock.calls.some(([type]) => type === "storage");
    expect(removed).toBe(true);
  });

  it("updates mode when another tab writes the storage key", () => {
    installMatchMedia(false);
    const { result } = renderHook(() => useThemeState());
    expect(result.current.mode).toBe("system");

    // Simulate another tab writing the key. Note: the storage event only
    // fires in OTHER tabs; the originating tab doesn't receive its own
    // event, which is why the local write path is a separate code path.
    // jsdom rejects our in-memory localStorage instance when passed as
    // storageArea (fails the IDL Storage check), so we omit it -- the
    // listener reads `event.key` only and never touches `storageArea`.
    act(() => {
      localStorage.setItem(THEME_STORAGE_KEY, JSON.stringify("dark"));
      window.dispatchEvent(
        new StorageEvent("storage", {
          key: THEME_STORAGE_KEY,
          newValue: JSON.stringify("dark"),
        }),
      );
    });

    expect(result.current.mode).toBe("dark");
    expect(result.current.effective).toBe("dark");
  });

  it("ignores storage events for other keys", () => {
    installMatchMedia(false);
    const { result } = renderHook(() => useThemeState());
    expect(result.current.mode).toBe("system");

    act(() => {
      window.dispatchEvent(
        new StorageEvent("storage", { key: "something-else", newValue: "irrelevant" }),
      );
    });

    expect(result.current.mode).toBe("system");
  });
});
