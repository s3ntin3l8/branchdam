import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { ErrorBoundary } from "./ErrorBoundary";

function ProblemChild({ shouldThrow }: { shouldThrow: boolean }) {
  if (shouldThrow) {
    throw new Error("Simulated component render failure");
  }
  return <div>Normal Content</div>;
}

describe("ErrorBoundary", () => {
  it("renders children when no error occurs", () => {
    render(
      <ErrorBoundary>
        <ProblemChild shouldThrow={false} />
      </ErrorBoundary>
    );

    expect(screen.getByText("Normal Content")).toBeInTheDocument();
  });

  it("renders fallback UI when a child component throws", () => {
    const consoleSpy = vi.spyOn(console, "error").mockImplementation(() => {});

    render(
      <ErrorBoundary>
        <ProblemChild shouldThrow={true} />
      </ErrorBoundary>
    );

    expect(screen.getByText("Something went wrong")).toBeInTheDocument();
    expect(screen.getByText("Simulated component render failure")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Try Again" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Reload Page" })).toBeInTheDocument();

    consoleSpy.mockRestore();
  });

  it("resets state when Try Again is clicked", async () => {
    const consoleSpy = vi.spyOn(console, "error").mockImplementation(() => {});
    const onReset = vi.fn();

    const { rerender } = render(
      <ErrorBoundary onReset={onReset}>
        <ProblemChild shouldThrow={true} />
      </ErrorBoundary>
    );

    expect(screen.getByText("Something went wrong")).toBeInTheDocument();

    rerender(
      <ErrorBoundary onReset={onReset}>
        <ProblemChild shouldThrow={false} />
      </ErrorBoundary>
    );

    await userEvent.click(screen.getByRole("button", { name: "Try Again" }));

    expect(onReset).toHaveBeenCalledTimes(1);
    expect(screen.getByText("Normal Content")).toBeInTheDocument();

    consoleSpy.mockRestore();
  });
});
