import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import PasswordResetPage from "./PasswordResetPage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    confirmPasswordReset: vi.fn(),
  },
}));

function renderWithClient(initialEntries: string[]) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={initialEntries}>
        <PasswordResetPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("PasswordResetPage", () => {
  it("shows an error and a back-to-login link when the URL has no token", () => {
    renderWithClient(["/password-reset"]);

    expect(screen.getByRole("heading", { name: "Invalid reset link" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Back to sign in" })).toHaveAttribute("href", "/login");
    // No form rendered when the token is missing.
    expect(screen.queryByLabelText(/New password/)).not.toBeInTheDocument();
  });

  it("renders the form with both password fields when a token is present", () => {
    renderWithClient(["/password-reset?token=opaque-token-abc"]);

    expect(screen.getByRole("heading", { name: "Set a new password" })).toBeInTheDocument();
    expect(screen.getByLabelText(/New password/)).toBeInTheDocument();
    expect(screen.getByLabelText(/Confirm new password/)).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Update password" })).toBeDisabled();
  });

  it("submits token+newPassword to api.confirmPasswordReset on success", async () => {
    vi.mocked(api.confirmPasswordReset).mockResolvedValue({ ok: true });
    const user = userEvent.setup();
    renderWithClient(["/password-reset?token=opaque-token-abc"]);

    await user.type(screen.getByLabelText(/^New password/), "new-secret-1");
    await user.type(screen.getByLabelText(/Confirm new password/), "new-secret-1");
    await user.click(screen.getByRole("button", { name: "Update password" }));

    await waitFor(() => {
      expect(api.confirmPasswordReset).toHaveBeenCalled();
    });
    const firstCall = vi.mocked(api.confirmPasswordReset).mock.calls[0];
    expect(firstCall[0]).toEqual({ token: "opaque-token-abc", newPassword: "new-secret-1" });
  });

  it("renders the success panel after a successful submit", async () => {
    vi.mocked(api.confirmPasswordReset).mockResolvedValue({ ok: true });
    const user = userEvent.setup();
    renderWithClient(["/password-reset?token=opaque-token-abc"]);

    await user.type(screen.getByLabelText(/^New password/), "new-secret-1");
    await user.type(screen.getByLabelText(/Confirm new password/), "new-secret-1");
    await user.click(screen.getByRole("button", { name: "Update password" }));

    await waitFor(() => {
      expect(screen.getByRole("heading", { name: "Password updated" })).toBeInTheDocument();
    });
    expect(screen.getByRole("link", { name: "Sign in" })).toHaveAttribute("href", "/login");
  });

  it("renders the error message from the server on failure", async () => {
    vi.mocked(api.confirmPasswordReset).mockRejectedValue(
      Object.assign(new Error("token not found, used, or expired"), { status: 404 })
    );
    const user = userEvent.setup();
    renderWithClient(["/password-reset?token=opaque-token-abc"]);

    await user.type(screen.getByLabelText(/^New password/), "new-secret-1");
    await user.type(screen.getByLabelText(/Confirm new password/), "new-secret-1");
    await user.click(screen.getByRole("button", { name: "Update password" }));

    await waitFor(() => {
      expect(screen.getByTestId("confirm-error")).toHaveTextContent("token not found, used, or expired");
    });
    // Form remains visible -- user can retry with a new token (after
    // requesting a new link from /login).
    expect(screen.getByLabelText(/^New password/)).toBeInTheDocument();
  });

  it("disables submit while passwords do not match", async () => {
    const user = userEvent.setup();
    renderWithClient(["/password-reset?token=opaque-token-abc"]);

    await user.type(screen.getByLabelText(/^New password/), "new-secret-1");
    await user.type(screen.getByLabelText(/Confirm new password/), "different-1");
    expect(screen.getByText("Passwords do not match")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Update password" })).toBeDisabled();
  });

  it("disables submit when the password is too short", async () => {
    const user = userEvent.setup();
    renderWithClient(["/password-reset?token=opaque-token-abc"]);

    await user.type(screen.getByLabelText(/^New password/), "short");
    await user.type(screen.getByLabelText(/Confirm new password/), "short");
    expect(screen.getByText("Password must be at least 8 characters")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Update password" })).toBeDisabled();
  });
});
