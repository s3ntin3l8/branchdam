import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";
import MfaSetupPage from "./MfaSetupPage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    mfaSetup: vi.fn(),
    mfaEnable: vi.fn(),
  },
}));

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const router = createMemoryRouter(
    [
      { path: "/mfa/setup", element: <MfaSetupPage /> },
      { path: "/users", element: <div>users page</div> },
    ],
    { initialEntries: ["/mfa/setup"] },
  );
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(api.mfaSetup).mockReset();
  vi.mocked(api.mfaEnable).mockReset();
});

describe("MfaSetupPage", () => {
  it("enrolls: shows the otpauth URI, verifies a code, then shows the recovery codes", async () => {
    vi.mocked(api.mfaSetup).mockResolvedValue({ otpauthURI: "otpauth://totp/branchDAM:alice?secret=ABC", asciiQR: "" });
    vi.mocked(api.mfaEnable).mockResolvedValueOnce({
      ok: true,
      recoveryCodes: ["aaaa-1111", "bbbb-2222"],
      notice: "",
    });
    renderPage();

    expect(await screen.findByText("otpauth://totp/branchDAM:alice?secret=ABC")).toBeInTheDocument();
    expect(api.mfaSetup).toHaveBeenCalled();

    await userEvent.type(screen.getByPlaceholderText("123456"), "654321");
    await userEvent.click(screen.getByRole("button", { name: "Enable MFA" }));

    await waitFor(() => expect(api.mfaEnable).toHaveBeenCalled());
    expect(vi.mocked(api.mfaEnable).mock.calls[0][0]).toEqual({ code: "654321" });
    expect(await screen.findByRole("heading", { name: "MFA enabled" })).toBeInTheDocument();
    expect(screen.getByText("aaaa-1111")).toBeInTheDocument();
    expect(screen.getByText("bbbb-2222")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(await screen.findByText("users page")).toBeInTheDocument();
  });

  it("keeps the code form up and shows the error when the code is rejected", async () => {
    vi.mocked(api.mfaSetup).mockResolvedValue({ otpauthURI: "otpauth://totp/x", asciiQR: "" });
    vi.mocked(api.mfaEnable).mockRejectedValueOnce(new Error("invalid code"));
    renderPage();

    await screen.findByText("otpauth://totp/x");
    await userEvent.type(screen.getByPlaceholderText("123456"), "000000");
    await userEvent.click(screen.getByRole("button", { name: "Enable MFA" }));

    expect(await screen.findByText("invalid code")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Enable MFA" })).toBeInTheDocument();
  });

  it("shows a setup failure with a way back", async () => {
    vi.mocked(api.mfaSetup).mockRejectedValue(new Error("MFA is not available"));
    renderPage();

    expect(await screen.findByText("MFA is not available")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Back to users" }));
    expect(await screen.findByText("users page")).toBeInTheDocument();
  });
});
