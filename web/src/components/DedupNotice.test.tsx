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
          nodeUuid="u-123"
          onDismiss={onDismiss}
        />
      </MemoryRouter>
    );

    expect(screen.getByText("This file is already in your library.")).toBeInTheDocument();
    const link = screen.getByRole("link", { name: /view existing node/i });
    expect(link).toHaveAttribute("href", "/assets/u-123");
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
});
