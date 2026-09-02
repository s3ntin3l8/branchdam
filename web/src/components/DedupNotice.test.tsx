import { render, screen, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import { MemoryRouter } from "react-router";
import DedupNotice from "./DedupNotice";

describe("DedupNotice", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("renders dedup notice with message and link to asset", () => {
    const onDismiss = vi.fn();
    render(
      <MemoryRouter>
        <DedupNotice
          fileName="sample.raw"
          assetId={42}
          onDismiss={onDismiss}
        />
      </MemoryRouter>
    );

    expect(screen.getByText(/sample\.raw/)).toBeInTheDocument();
    expect(screen.getByText(/already in your library/i)).toBeInTheDocument();
    const link = screen.getByRole("link", { name: /view existing node/i });
    expect(link).toHaveAttribute("href", "/assets/42");
  });

  it("renders generic message if fileName is not provided", () => {
    const onDismiss = vi.fn();
    render(
      <MemoryRouter>
        <DedupNotice
          assetId={42}
          onDismiss={onDismiss}
        />
      </MemoryRouter>
    );

    expect(screen.getByText("This file is already in your library.")).toBeInTheDocument();
    const link = screen.getByRole("link", { name: /view existing node/i });
    expect(link).toHaveAttribute("href", "/assets/42");
  });

  it("dismisses on click of close button", async () => {
    vi.useRealTimers();
    const onDismiss = vi.fn();
    render(
      <MemoryRouter>
        <DedupNotice
          fileName="sample.raw"
          assetId={42}
          onDismiss={onDismiss}
        />
      </MemoryRouter>
    );

    const closeBtn = screen.getByRole("button", { name: /dismiss/i });
    await userEvent.click(closeBtn);
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it("auto-dismisses after specified timeout (default 10s)", () => {
    const onDismiss = vi.fn();
    render(
      <MemoryRouter>
        <DedupNotice
          fileName="sample.raw"
          assetId={42}
          onDismiss={onDismiss}
          autoDismissMs={10000}
        />
      </MemoryRouter>
    );

    expect(onDismiss).not.toHaveBeenCalled();
    act(() => {
      vi.advanceTimersByTime(9999);
    });
    expect(onDismiss).not.toHaveBeenCalled();

    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it("does not reset the auto-dismiss timer on parent re-renders with new callback references", () => {
    const onDismiss1 = vi.fn();
    const { rerender } = render(
      <MemoryRouter>
        <DedupNotice
          fileName="sample.raw"
          assetId={42}
          onDismiss={onDismiss1}
          autoDismissMs={10000}
        />
      </MemoryRouter>
    );

    // Advance 6 seconds
    act(() => {
      vi.advanceTimersByTime(6000);
    });
    expect(onDismiss1).not.toHaveBeenCalled();

    // Re-render with new callback
    const onDismiss2 = vi.fn();
    rerender(
      <MemoryRouter>
        <DedupNotice
          fileName="sample.raw"
          assetId={42}
          onDismiss={onDismiss2}
          autoDismissMs={10000}
        />
      </MemoryRouter>
    );

    // Advance remaining 4 seconds (total 10 seconds from initial mount)
    act(() => {
      vi.advanceTimersByTime(4000);
    });
    // The new callback should be called, without timer having reset back to 0
    expect(onDismiss2).toHaveBeenCalledTimes(1);
    expect(onDismiss1).not.toHaveBeenCalled();
  });
});
