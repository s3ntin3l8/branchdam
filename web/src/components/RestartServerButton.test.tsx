import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { RestartServerCard } from "./RestartServerButton";

const mutate = vi.fn();

vi.mock("../hooks/queries", () => ({
  useRestartServer: () => ({
    mutate,
    isPending: false,
    isError: false,
    error: null,
  }),
}));

describe("RestartServerCard", () => {
  beforeEach(() => {
    vi.useFakeTimers();
    mutate.mockReset();
  });

  afterEach(() => {
    vi.useRealTimers();
  });

  it("clears the restart notice timer on unmount", () => {
    const { unmount } = render(<RestartServerCard />);

    fireEvent.click(screen.getByRole("button", { name: "Restart server" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart" }));

    expect(mutate).toHaveBeenCalledTimes(1);
    const options = mutate.mock.calls[0][1];
    act(() => options.onSuccess());
    expect(screen.getByText(/reconnect automatically/i)).toBeInTheDocument();

    unmount();

    expect(vi.getTimerCount()).toBe(0);
  });

  it("replaces the previous restart notice timer after another successful restart", () => {
    render(<RestartServerCard />);

    fireEvent.click(screen.getByRole("button", { name: "Restart server" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart" }));
    act(() => mutate.mock.calls[0][1].onSuccess());
    expect(vi.getTimerCount()).toBe(1);

    fireEvent.click(screen.getByRole("button", { name: "Restart server" }));
    fireEvent.click(screen.getByRole("button", { name: "Restart" }));
    act(() => mutate.mock.calls[1][1].onSuccess());

    expect(vi.getTimerCount()).toBe(1);
  });
});
