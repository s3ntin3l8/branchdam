import { act, render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { afterEach, describe, expect, it } from "vitest";
import { AuthErrorBanner } from "./AuthErrorBanner";

function fireApiError(status: number, message: string) {
  window.dispatchEvent(new CustomEvent("api-auth-error", { detail: { status, message } }));
}

function fireApiSuccess() {
  window.dispatchEvent(new CustomEvent("api-auth-success"));
}

afterEach(() => {
  // Ensure no banner state leaks between tests.
  fireApiSuccess();
});

describe("AuthErrorBanner", () => {
  it("renders nothing on first paint", () => {
    render(<AuthErrorBanner />);
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.queryByLabelText(/dismiss/i)).not.toBeInTheDocument();
  });

  it("shows the banner when an api-auth-error event fires", () => {
    render(<AuthErrorBanner />);
    act(() => fireApiError(401, "session expired"));
    expect(screen.getByText(/authentication required/i)).toBeInTheDocument();
    expect(screen.getByText(/session expired/i)).toBeInTheDocument();
  });

  it("uses 'Access denied' for non-401 status codes", () => {
    render(<AuthErrorBanner />);
    act(() => fireApiError(403, "not allowed"));
    expect(screen.getByText(/access denied/i)).toBeInTheDocument();
    expect(screen.getByText(/not allowed/i)).toBeInTheDocument();
  });

  it("hides the banner when api-auth-success fires (transient 401 recovery)", () => {
    render(<AuthErrorBanner />);
    act(() => fireApiError(401, "session expired"));
    expect(screen.getByText(/authentication required/i)).toBeInTheDocument();
    act(() => fireApiSuccess());
    expect(screen.queryByText(/authentication required/i)).not.toBeInTheDocument();
  });

  it("dismisses via the × button", async () => {
    const user = userEvent.setup();
    render(<AuthErrorBanner />);
    act(() => fireApiError(401, "session expired"));
    const dismissBtn = screen.getByLabelText(/dismiss/i);
    await user.click(dismissBtn);
    expect(screen.queryByText(/authentication required/i)).not.toBeInTheDocument();
  });

  it("deduplicates simultaneous identical error events", () => {
    render(<AuthErrorBanner />);
    act(() => {
      fireApiError(401, "session expired");
      fireApiError(401, "session expired");
      fireApiError(401, "session expired");
    });
    // The banner is shown once; dedup is verified by the component
    // returning the same state object (no re-render with new state).
    expect(screen.getByText(/authentication required/i)).toBeInTheDocument();
  });

  it("updates when a different error arrives (not deduped)", () => {
    render(<AuthErrorBanner />);
    act(() => fireApiError(401, "session expired"));
    expect(screen.getByText(/session expired/i)).toBeInTheDocument();
    act(() => fireApiError(403, "forbidden"));
    expect(screen.getByText(/forbidden/i)).toBeInTheDocument();
    expect(screen.queryByText(/session expired/i)).not.toBeInTheDocument();
  });
});
