import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { fireEvent, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { SettingsLayout, type SettingsCategory } from "./SettingsLayout";

const TEST_CATEGORIES: SettingsCategory[] = [
  { id: "server", label: "Server & Storage" },
  { id: "workers", label: "Workers & Indexing" },
  { id: "appearance", label: "Appearance" },
];

describe("SettingsLayout", () => {
  const originalScrollIntoView = HTMLElement.prototype.scrollIntoView;

  beforeEach(() => {
    HTMLElement.prototype.scrollIntoView = vi.fn();
    window.location.hash = "";
  });

  afterEach(() => {
    HTMLElement.prototype.scrollIntoView = originalScrollIntoView;
    window.location.hash = "";
    vi.restoreAllMocks();
  });

  it("renders all category links in navigation and sets first category active by default", () => {
    render(
      <SettingsLayout categories={TEST_CATEGORIES}>
        <section id="server" data-settings-section="server">
          <h2>Server Section</h2>
        </section>
        <section id="workers" data-settings-section="workers">
          <h2>Workers Section</h2>
        </section>
        <section id="appearance" data-settings-section="appearance">
          <h2>Appearance Section</h2>
        </section>
      </SettingsLayout>
    );

    const serverLink = screen.getByRole("link", { name: "Server & Storage" });
    const workersLink = screen.getByRole("link", { name: "Workers & Indexing" });
    const appearanceLink = screen.getByRole("link", { name: "Appearance" });

    expect(serverLink).toBeInTheDocument();
    expect(workersLink).toBeInTheDocument();
    expect(appearanceLink).toBeInTheDocument();

    expect(serverLink).toHaveClass("bg-neutral-800", "text-neutral-100");
    expect(appearanceLink).not.toHaveClass("bg-neutral-800");
  });

  it("highlights the clicked category, calls scrollIntoView, and updates URL hash via history.replaceState", async () => {
    const user = userEvent.setup();
    const replaceStateSpy = vi.spyOn(window.history, "replaceState");

    render(
      <SettingsLayout categories={TEST_CATEGORIES}>
        <section id="server" data-settings-section="server">
          <h2>Server Section</h2>
        </section>
        <section id="workers" data-settings-section="workers">
          <h2>Workers Section</h2>
        </section>
        <section id="appearance" data-settings-section="appearance">
          <h2>Appearance Section</h2>
        </section>
      </SettingsLayout>
    );

    const appearanceLink = screen.getByRole("link", { name: "Appearance" });
    await user.click(appearanceLink);

    expect(appearanceLink).toHaveClass("bg-neutral-800", "text-neutral-100");
    expect(HTMLElement.prototype.scrollIntoView).toHaveBeenCalledWith({
      behavior: "smooth",
      block: "start",
    });
    expect(replaceStateSpy).toHaveBeenCalledWith(null, "", "#appearance");
  });

  it("activates the last category when scrolled to the bottom of an HTMLElement scroll container", () => {
    const { container } = render(
      <div
        data-testid="scroll-container"
        style={{ height: "500px", overflowY: "auto" }}
      >
        <SettingsLayout categories={TEST_CATEGORIES}>
          <section id="server" data-settings-section="server">
            <h2>Server Section</h2>
          </section>
          <section id="workers" data-settings-section="workers">
            <h2>Workers Section</h2>
          </section>
          <section id="appearance" data-settings-section="appearance">
            <h2>Appearance Section</h2>
          </section>
        </SettingsLayout>
      </div>
    );

    const scrollContainer = container.querySelector('[data-testid="scroll-container"]') as HTMLElement;
    expect(scrollContainer).not.toBeNull();

    // Simulate scroll container at bottom
    Object.defineProperty(scrollContainer, "scrollHeight", { value: 1000, configurable: true });
    Object.defineProperty(scrollContainer, "clientHeight", { value: 500, configurable: true });
    Object.defineProperty(scrollContainer, "scrollTop", { value: 490, configurable: true, writable: true }); // 1000 - 490 - 500 = 10 <= 24

    // Fire scroll event on container
    fireEvent.scroll(scrollContainer);

    const appearanceLink = screen.getByRole("link", { name: "Appearance" });
    expect(appearanceLink).toHaveClass("bg-neutral-800", "text-neutral-100");
  });

  it("activates the last category when scrolled to the bottom of the window", () => {
    render(
      <SettingsLayout categories={TEST_CATEGORIES}>
        <section id="server" data-settings-section="server">
          <h2>Server Section</h2>
        </section>
        <section id="workers" data-settings-section="workers">
          <h2>Workers Section</h2>
        </section>
        <section id="appearance" data-settings-section="appearance">
          <h2>Appearance Section</h2>
        </section>
      </SettingsLayout>
    );

    Object.defineProperty(window, "innerHeight", { value: 600, configurable: true });
    Object.defineProperty(window, "scrollY", { value: 400, configurable: true, writable: true });
    Object.defineProperty(document.documentElement, "scrollHeight", { value: 1000, configurable: true }); // 600 + 400 = 1000 >= 1000 - 24

    fireEvent.scroll(window);

    const appearanceLink = screen.getByRole("link", { name: "Appearance" });
    expect(appearanceLink).toHaveClass("bg-neutral-800", "text-neutral-100");
  });

  it("activates category matching initial window.location.hash on mount", () => {
    window.location.hash = "#appearance";

    render(
      <SettingsLayout categories={TEST_CATEGORIES}>
        <section id="server" data-settings-section="server">
          <h2>Server Section</h2>
        </section>
        <section id="workers" data-settings-section="workers">
          <h2>Workers Section</h2>
        </section>
        <section id="appearance" data-settings-section="appearance">
          <h2>Appearance Section</h2>
        </section>
      </SettingsLayout>
    );

    const appearanceLink = screen.getByRole("link", { name: "Appearance" });
    expect(appearanceLink).toHaveClass("bg-neutral-800", "text-neutral-100");
    expect(HTMLElement.prototype.scrollIntoView).toHaveBeenCalledWith({
      behavior: "smooth",
      block: "start",
    });
  });

  it("resets the scroll lock on effect cleanup so subsequent scrolls are not blocked", async () => {
    const user = userEvent.setup();
    const { rerender, container } = render(
      <div data-testid="scroll-container" style={{ height: "500px", overflowY: "auto" }}>
        <SettingsLayout categories={TEST_CATEGORIES}>
          <section id="server" data-settings-section="server">
            <h2>Server Section</h2>
          </section>
          <section id="workers" data-settings-section="workers">
            <h2>Workers Section</h2>
          </section>
          <section id="appearance" data-settings-section="appearance">
            <h2>Appearance Section</h2>
          </section>
        </SettingsLayout>
      </div>
    );

    // Click to start programmatic scroll lock
    const appearanceLink = screen.getByRole("link", { name: "Appearance" });
    await user.click(appearanceLink);

    // Rerender with new categories array identity (triggers effect teardown/cleanup)
    const newCategories = [...TEST_CATEGORIES];
    rerender(
      <div data-testid="scroll-container" style={{ height: "500px", overflowY: "auto" }}>
        <SettingsLayout categories={newCategories}>
          <section id="server" data-settings-section="server">
            <h2>Server Section</h2>
          </section>
          <section id="workers" data-settings-section="workers">
            <h2>Workers Section</h2>
          </section>
          <section id="appearance" data-settings-section="appearance">
            <h2>Appearance Section</h2>
          </section>
        </SettingsLayout>
      </div>
    );

    const scrollContainer = container.querySelector('[data-testid="scroll-container"]') as HTMLElement;
    Object.defineProperty(scrollContainer, "scrollHeight", { value: 1000, configurable: true });
    Object.defineProperty(scrollContainer, "clientHeight", { value: 500, configurable: true });
    Object.defineProperty(scrollContainer, "scrollTop", { value: 490, configurable: true, writable: true });

    // Scrolling to bottom should work immediately and not be blocked by stale lock
    fireEvent.scroll(scrollContainer);

    expect(screen.getByRole("link", { name: "Appearance" })).toHaveClass("bg-neutral-800", "text-neutral-100");
  });

  it("unlocks programmatic scroll immediately on user wheel or keyboard interaction", async () => {
    const user = userEvent.setup();
    const { container } = render(
      <div data-testid="scroll-container" style={{ height: "500px", overflowY: "auto" }}>
        <SettingsLayout categories={TEST_CATEGORIES}>
          <section id="server" data-settings-section="server">
            <h2>Server Section</h2>
          </section>
          <section id="workers" data-settings-section="workers">
            <h2>Workers Section</h2>
          </section>
          <section id="appearance" data-settings-section="appearance">
            <h2>Appearance Section</h2>
          </section>
        </SettingsLayout>
      </div>
    );

    // Click to start programmatic scroll lock
    const appearanceLink = screen.getByRole("link", { name: "Appearance" });
    await user.click(appearanceLink);

    // User spins mouse wheel
    fireEvent.wheel(window);

    const scrollContainer = container.querySelector('[data-testid="scroll-container"]') as HTMLElement;
    Object.defineProperty(scrollContainer, "scrollHeight", { value: 1000, configurable: true });
    Object.defineProperty(scrollContainer, "clientHeight", { value: 500, configurable: true });
    Object.defineProperty(scrollContainer, "scrollTop", { value: 490, configurable: true, writable: true });

    // Scroll event immediately processes
    fireEvent.scroll(scrollContainer);

    expect(screen.getByRole("link", { name: "Appearance" })).toHaveClass("bg-neutral-800", "text-neutral-100");
  });
});
