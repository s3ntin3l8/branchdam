import { render, screen } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import PendingResetsPanel from "./PendingResetsPanel";

// PendingResetsPanel is a stub in PR #409. The full implementation
// lands in PR #408 (admin user management), which will mount this
// on /admin/users and wire it to the password-reset list endpoints.
// These tests pin the stub's current behavior so #408's replacement
// can land cleanly without API churn.

describe("PendingResetsPanel (stub)", () => {
  it("renders a placeholder explaining the panel ships in #408", () => {
    render(<PendingResetsPanel />);
    expect(screen.getByTestId("pending-resets-panel-stub")).toBeInTheDocument();
    expect(screen.getByText(/Pending password resets/)).toBeInTheDocument();
    expect(screen.getByText(/PR #408/)).toBeInTheDocument();
    expect(screen.getByText(/slog\.WARN/)).toBeInTheDocument();
  });
});
