import { StrictMode, useState } from "react";
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

function Harness() {
  const queryClient = useState(
    () => new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } }),
  )[0];
  const router = useState(() =>
    createMemoryRouter(
      [
        { path: "/mfa/setup", element: <MfaSetupPage /> },
        { path: "/account", element: <div>account page</div> },
      ],
      { initialEntries: ["/mfa/setup"] },
    ),
  )[0];
  return (
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
}

function renderPage() {
  render(<Harness />);
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

    await userEvent.type(screen.getByLabelText("Then enter the 6-digit code it shows"), "654321");
    await userEvent.click(screen.getByRole("button", { name: "Enable MFA" }));

    await waitFor(() => expect(api.mfaEnable).toHaveBeenCalled());
    expect(vi.mocked(api.mfaEnable).mock.calls[0][0]).toEqual({ code: "654321" });
    expect(await screen.findByRole("heading", { name: "MFA enabled" })).toBeInTheDocument();
    expect(screen.getByText("aaaa-1111")).toBeInTheDocument();
    expect(screen.getByText("bbbb-2222")).toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: "Done" }));
    expect(await screen.findByText("account page")).toBeInTheDocument();
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

  it("never submits a whitespace-only code", async () => {
    vi.mocked(api.mfaSetup).mockResolvedValue({ otpauthURI: "otpauth://totp/x", asciiQR: "" });
    renderPage();

    await screen.findByText("otpauth://totp/x");
    const input = screen.getByPlaceholderText("123456");
    await userEvent.type(input, "   ");
    expect(screen.getByRole("button", { name: "Enable MFA" })).toBeDisabled();
    await userEvent.type(input, "{Enter}");
    expect(api.mfaEnable).not.toHaveBeenCalled();
  });

  it("shows a setup failure with a way back", async () => {
    vi.mocked(api.mfaSetup).mockRejectedValue(new Error("MFA is not available"));
    renderPage();

    expect(await screen.findByText("MFA is not available")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("link", { name: "Back to account" }));
    expect(await screen.findByText("account page")).toBeInTheDocument();
  });

  it("shows the QR code, hides the key until asked, and starts setup only once", async () => {
    vi.mocked(api.mfaSetup).mockResolvedValue({
      otpauthURI: "otpauth://totp/branchDAM:bob?secret=JBSWY3DPEHPK3PXP&issuer=branchDAM",
      asciiQR: "",
      qrSvg: '<svg xmlns="http://www.w3.org/2000/svg"></svg>',
    });
    render(
      <StrictMode>
        <Harness />
      </StrictMode>,
    );

    const img = await screen.findByRole("img", { name: "MFA QR code" });
    expect(img.getAttribute("src")).toMatch(/^data:image\/svg\+xml/);
    expect(screen.queryByTestId("mfa-secret")).not.toBeInTheDocument();

    await userEvent.click(screen.getByRole("button", { name: /enter a key instead/i }));
    expect(screen.getByTestId("mfa-secret")).toHaveTextContent("JBSW Y3DP EHPK 3PXP");

    // Each /mfa/setup call mints a new secret, so StrictMode's double
    // render and double effect must still produce exactly one call.
    expect(api.mfaSetup).toHaveBeenCalledTimes(1);
  });
});
