import { render, screen, act } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, it, expect, vi, beforeEach, afterEach } from "vitest";
import InlineNotice from "./InlineNotice";

describe("InlineNotice", () => {
  beforeEach(() => {
    vi.useFakeTimers();
  });

  afterEach(() => {
    vi.useRealTimers();
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

  it("auto-dismisses after the default 8s timeout", () => {
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

  it("does not auto-dismiss when autoDismissMs is 0", () => {
    const onDismiss = vi.fn();
    render(
      <InlineNotice tone="error" message="Sticky." onDismiss={onDismiss} autoDismissMs={0} />,
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

  it("resets the auto-dismiss countdown when the message is swapped in place", () => {
    const onDismiss = vi.fn();
    const { rerender } = render(
      <InlineNotice tone="success" message="First message" onDismiss={onDismiss} />,
    );

    act(() => {
      vi.advanceTimersByTime(7950);
    });
    expect(onDismiss).not.toHaveBeenCalled();

    rerender(
      <InlineNotice tone="success" message="Second message" onDismiss={onDismiss} />,
    );

    // Old countdown would fire 50ms later; the new message must get a full window.
    act(() => {
      vi.advanceTimersByTime(50);
    });
    expect(onDismiss).not.toHaveBeenCalled();

    act(() => {
      vi.advanceTimersByTime(7950);
    });
    expect(onDismiss).toHaveBeenCalledTimes(1);
  });
});
