import { render, screen, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import InlineNotice from "./InlineNotice";

describe("InlineNotice", () => {
  let scrollIntoViewSpy: ReturnType<typeof vi.fn<(options?: ScrollIntoViewOptions | boolean) => void>>;

  beforeEach(() => {
    vi.useFakeTimers();
    scrollIntoViewSpy = vi.fn<(options?: ScrollIntoViewOptions | boolean) => void>();
    Element.prototype.scrollIntoView = scrollIntoViewSpy;
  });

  afterEach(() => {
    vi.useRealTimers();
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    delete (Element.prototype as any).scrollIntoView;
  });

  it("renders success message with polite status role", () => {
    render(
      <InlineNotice tone="success" message="Revoked 1 active session." onDismiss={vi.fn()} />,
    );

    // role="status" implies aria-live="polite"; no explicit attribute.
    expect(screen.getByRole("status")).toHaveTextContent("Revoked 1 active session.");
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("renders error message with assertive alert role", () => {
    render(<InlineNotice tone="error" message="Failed to re-enable bob: boom" onDismiss={vi.fn()} />);

    // role="alert" implies aria-live="assertive"; no explicit attribute.
    expect(screen.getByRole("alert")).toHaveTextContent("Failed to re-enable bob: boom");
    expect(screen.queryByRole("status")).not.toBeInTheDocument();
  });

  it("dismisses on click of close button", async () => {
    vi.useRealTimers();
    const onDismiss = vi.fn();
    render(<InlineNotice tone="error" message="Failed." onDismiss={onDismiss} />);

    await userEvent.click(screen.getByRole("button", { name: /dismiss notice/i }));
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it("auto-dismisses success after the default 8s timeout", () => {
    const onDismiss = vi.fn();
    render(<InlineNotice tone="success" message="Done." onDismiss={onDismiss} />);

    act(() => {
      vi.advanceTimersByTime(7999);
    });
    expect(onDismiss).not.toHaveBeenCalled();

    act(() => {
      vi.advanceTimersByTime(1);
    });
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it("error tone is sticky by default (autoDismissMs defaults to 0)", () => {
    const onDismiss = vi.fn();
    render(<InlineNotice tone="error" message="Failed." onDismiss={onDismiss} />);

    act(() => {
      vi.advanceTimersByTime(60000);
    });
    expect(onDismiss).not.toHaveBeenCalled();
  });

  it("does not auto-dismiss when autoDismissMs is explicitly 0", () => {
    const onDismiss = vi.fn();
    render(
      <InlineNotice tone="success" message="Sticky success." onDismiss={onDismiss} autoDismissMs={0} />,
    );

    act(() => {
      vi.advanceTimersByTime(60000);
    });
    expect(onDismiss).not.toHaveBeenCalled();
  });

  it("does not reset the auto-dismiss timer on parent re-renders with new callbacks", () => {
    const onDismiss1 = vi.fn();
    const { rerender } = render(
      <InlineNotice tone="success" message="Done." onDismiss={onDismiss1} />,
    );

    act(() => {
      vi.advanceTimersByTime(6000);
    });
    expect(onDismiss1).not.toHaveBeenCalled();

    const onDismiss2 = vi.fn();
    rerender(<InlineNotice tone="success" message="Done." onDismiss={onDismiss2} />);

    act(() => {
      vi.advanceTimersByTime(2000);
    });
    expect(onDismiss2).toHaveBeenCalledTimes(1);
    expect(onDismiss1).not.toHaveBeenCalled();
  });

  it("does not restart the countdown when message is swapped in place (key contract)", () => {
    // Timer starts on mount only; callers re-showing a notice must change
    // the React key to remount. Swapping props alone keeps the deadline.
    const onDismiss = vi.fn();
    const { rerender } = render(
      <InlineNotice tone="success" message="First message" onDismiss={onDismiss} />,
    );

    act(() => {
      vi.advanceTimersByTime(7000);
    });
    expect(onDismiss).not.toHaveBeenCalled();

    rerender(<InlineNotice tone="success" message="Second message" onDismiss={onDismiss} />);

    act(() => {
      vi.advanceTimersByTime(1000);
    });
    // Original 8s deadline from first mount, not a fresh 8s from the swap.
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it("restarts the countdown when remounted via a new key", () => {
    const onDismiss = vi.fn();
    const { rerender } = render(
      <InlineNotice key={1} tone="success" message="Done." onDismiss={onDismiss} />,
    );

    act(() => {
      vi.advanceTimersByTime(7000);
    });
    expect(onDismiss).not.toHaveBeenCalled();

    rerender(<InlineNotice key={2} tone="success" message="Done." onDismiss={onDismiss} />);

    // Fresh 8s window from remount; old deadline would fire 1s later.
    act(() => {
      vi.advanceTimersByTime(1000);
    });
    expect(onDismiss).not.toHaveBeenCalled();

    act(() => {
      vi.advanceTimersByTime(7000);
    });
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });

  it("scrolls itself into view on mount", () => {
    render(<InlineNotice tone="success" message="Done." onDismiss={vi.fn()} />);

    expect(scrollIntoViewSpy).toHaveBeenCalledTimes(1);
    expect(scrollIntoViewSpy).toHaveBeenCalledWith({ behavior: "smooth", block: "nearest" });
  });

  it("scrolls itself into view again when the message swaps", () => {
    const { rerender } = render(
      <InlineNotice tone="success" message="First" onDismiss={vi.fn()} />,
    );
    expect(scrollIntoViewSpy).toHaveBeenCalledTimes(1);

    rerender(<InlineNotice tone="success" message="Second" onDismiss={vi.fn()} />);
    expect(scrollIntoViewSpy).toHaveBeenCalledTimes(2);
    expect(scrollIntoViewSpy).toHaveBeenLastCalledWith({ behavior: "smooth", block: "nearest" });
  });
});
