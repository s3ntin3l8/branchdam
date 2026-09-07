import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import LoginPage from "./LoginPage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    setupStatus: vi.fn(),
    setupAdmin: vi.fn(),
    login: vi.fn(),
    requestPasswordReset: vi.fn(),
  },
}));

function renderWithClient() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>
        <LoginPage />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

describe("LoginPage", () => {
  it("renders the sign-in form for a local-mode deployment with users present", async () => {
    vi.mocked(api.setupStatus).mockResolvedValue({
      readyForSetup: false,
      mode: "local",
    });
    renderWithClient();
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    });
    expect(screen.getByLabelText("Username")).toBeInTheDocument();
    expect(screen.getByLabelText("Password")).toBeInTheDocument();
    // Forgot password? link is present in local/both mode
    expect(screen.getByRole("button", { name: "Forgot password?" })).toBeInTheDocument();
    // No SSO button in local-only mode
    expect(screen.queryByRole("button", { name: "Sign in with SSO" })).not.toBeInTheDocument();
  });

  it("renders the SSO button in 'both' mode", async () => {
    vi.mocked(api.setupStatus).mockResolvedValue({
      readyForSetup: false,
      mode: "both",
    });
    renderWithClient();
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Sign in with SSO" })).toBeInTheDocument();
    });
  });

  it("expands the reset form when 'Forgot password?' is clicked", async () => {
    vi.mocked(api.setupStatus).mockResolvedValue({
      readyForSetup: false,
      mode: "local",
    });
    const user = userEvent.setup();
    renderWithClient();
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Forgot password?" })).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: "Forgot password?" }));
    expect(screen.getByTestId("reset-form")).toBeInTheDocument();
    expect(screen.getByPlaceholderText("you@example.com")).toBeInTheDocument();
  });

  it("calls api.requestPasswordReset when the form is submitted", async () => {
    vi.mocked(api.setupStatus).mockResolvedValue({
      readyForSetup: false,
      mode: "local",
    });
    vi.mocked(api.requestPasswordReset).mockResolvedValue({ ok: true });
    const user = userEvent.setup();
    renderWithClient();
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Forgot password?" })).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: "Forgot password?" }));
    await user.type(screen.getByPlaceholderText("you@example.com"), "alice@example.com");
    await user.click(screen.getByRole("button", { name: "Send reset link" }));
    await waitFor(() => {
      expect(api.requestPasswordReset).toHaveBeenCalledWith({ email: "alice@example.com" });
    });
  });

  it("shows a confirmation panel after a successful request", async () => {
    vi.mocked(api.setupStatus).mockResolvedValue({
      readyForSetup: false,
      mode: "local",
    });
    vi.mocked(api.requestPasswordReset).mockResolvedValue({ ok: true });
    const user = userEvent.setup();
    renderWithClient();
    await waitFor(() => {
      expect(screen.getByRole("button", { name: "Forgot password?" })).toBeInTheDocument();
    });
    await user.click(screen.getByRole("button", { name: "Forgot password?" }));
    await user.type(screen.getByPlaceholderText("you@example.com"), "alice@example.com");
    await user.click(screen.getByRole("button", { name: "Send reset link" }));
    await waitFor(() => {
      expect(screen.getByTestId("reset-sent")).toBeInTheDocument();
    });
    expect(screen.getByText(/If an account exists/)).toBeInTheDocument();
  });

  it("renders the setup form when no users exist", async () => {
    vi.mocked(api.setupStatus).mockResolvedValue({
      readyForSetup: true,
      mode: "local",
    });
    renderWithClient();
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: "Create the first admin" })).toBeInTheDocument();
    });
  });
});
