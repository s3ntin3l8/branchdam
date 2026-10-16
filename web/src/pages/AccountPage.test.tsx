import { render, screen, waitFor, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";
import AccountPage from "./AccountPage";
import { api } from "../api/client";
import type { Me, Pat } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    me: vi.fn(),
    mfaDisable: vi.fn(),
    listPats: vi.fn(),
    createPat: vi.fn(),
    revokePat: vi.fn(),
  },
  ApiError: class ApiError extends Error {
    status: number;
    constructor(status: number, message: string) {
      super(message);
      this.status = status;
    }
  },
}));

const localUser: Me = {
  kind: "user",
  name: "bob",
  authenticated: true,
  isAdmin: false,
  isLocal: true,
  localUserId: 7,
};

function pat(overrides: Partial<Pat> = {}): Pat {
  return {
    id: 1,
    name: "backup script",
    scopes: ["admin"],
    createdAt: 1700000000,
    hashedKeyPrefix: "abcd1234",
    ...overrides,
  };
}

function renderPage() {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  render(
    <QueryClientProvider client={qc}>
      <MemoryRouter>
        <AccountPage />
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.mocked(api.me).mockReset();
  vi.mocked(api.mfaDisable).mockReset();
  vi.mocked(api.listPats).mockReset();
  vi.mocked(api.createPat).mockReset();
  vi.mocked(api.revokePat).mockReset();
});

describe("AccountPage: two-factor authentication", () => {
  it("offers a non-admin local user MFA set-up", async () => {
    vi.mocked(api.me).mockResolvedValue(localUser);
    renderPage();

    const link = await screen.findByRole("link", { name: "Set up two-factor authentication" });
    expect(link.getAttribute("href")).toBe("/mfa/setup");
    // PATs are admin-only: a non-admin never lists them (no 403 noise).
    expect(screen.getByText("Personal access tokens are available to administrators.")).toBeInTheDocument();
    expect(api.listPats).not.toHaveBeenCalled();
  });

  it("lets a user with MFA turn it off with password and code", async () => {
    vi.mocked(api.me).mockResolvedValueOnce({ ...localUser, mfaEnabled: true });
    vi.mocked(api.me).mockResolvedValue({ ...localUser, mfaEnabled: false });
    vi.mocked(api.mfaDisable).mockResolvedValue({ ok: true });
    renderPage();

    expect(await screen.findByText("On")).toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Turn off two-factor authentication" }));
    const dialog = screen.getByRole("dialog", { name: "Turn off two-factor authentication" });
    await userEvent.type(within(dialog).getByLabelText("Password"), "hunter2");
    await userEvent.type(within(dialog).getByLabelText("Authenticator code"), " 123456 ");
    await userEvent.click(within(dialog).getByRole("button", { name: "Turn off" }));

    await waitFor(() => expect(api.mfaDisable).toHaveBeenCalled());
    expect(vi.mocked(api.mfaDisable).mock.calls[0][0]).toEqual({ password: "hunter2", code: "123456" });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(await screen.findByRole("link", { name: "Set up two-factor authentication" })).toBeInTheDocument();
  });

  it("keeps the disable dialog open and shows the error on a wrong code", async () => {
    vi.mocked(api.me).mockResolvedValue({ ...localUser, mfaEnabled: true });
    vi.mocked(api.mfaDisable).mockRejectedValue(new Error("disable MFA: invalid TOTP code"));
    renderPage();

    await userEvent.click(await screen.findByRole("button", { name: "Turn off two-factor authentication" }));
    const dialog = screen.getByRole("dialog");
    await userEvent.type(within(dialog).getByLabelText("Password"), "pw");
    await userEvent.type(within(dialog).getByLabelText("Authenticator code"), "000000");
    await userEvent.click(within(dialog).getByRole("button", { name: "Turn off" }));

    expect(await within(dialog).findByRole("alert")).toHaveTextContent("invalid TOTP code");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
  });

  it("sends a forward-auth user to their identity provider", async () => {
    vi.mocked(api.me).mockResolvedValue({ kind: "user", name: "eve", authenticated: true, isAdmin: false });
    renderPage();

    expect(await screen.findByText(/managed by your identity provider/)).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Set up two-factor authentication" })).not.toBeInTheDocument();
  });
});

describe("AccountPage: loading", () => {
  it("shows a failed /me with a retry instead of loading forever", async () => {
    vi.mocked(api.me).mockRejectedValueOnce(new Error("server down"));
    vi.mocked(api.me).mockResolvedValue(localUser);
    renderPage();

    expect(await screen.findByRole("alert")).toHaveTextContent("Failed to load your account: server down");
    expect(screen.queryByText("Loading account…")).not.toBeInTheDocument();
    await userEvent.click(screen.getByRole("button", { name: "Retry" }));
    expect(await screen.findByRole("heading", { name: "Account security" })).toBeInTheDocument();
  });
});

describe("AccountPage: personal access tokens", () => {
  const admin: Me = { ...localUser, isAdmin: true };

  it("says the token list is partial when the server holds more than one page", async () => {
    vi.mocked(api.me).mockResolvedValue(admin);
    vi.mocked(api.listPats).mockResolvedValue({ pats: [pat({ id: 1, name: "newest" })], total: 201 });
    renderPage();

    await screen.findByText("newest");
    expect(screen.getByText(/Showing the 1 most recent of 201 tokens/)).toBeInTheDocument();
  });

  it("does not call a complete token list partial", async () => {
    vi.mocked(api.me).mockResolvedValue(admin);
    vi.mocked(api.listPats).mockResolvedValue({ pats: [pat({ id: 1, name: "only" })], total: 1 });
    renderPage();

    await screen.findByText("only");
    expect(screen.queryByText(/most recent of/)).not.toBeInTheDocument();
  });

  it("lists tokens with their status and only offers revoke for live ones", async () => {
    vi.mocked(api.me).mockResolvedValue(admin);
    vi.mocked(api.listPats).mockResolvedValue({
      pats: [
        pat({ id: 1, name: "live", lastUsedAt: 1700000500 }),
        pat({ id: 2, name: "gone", revokedAt: 1700000600 }),
        pat({ id: 3, name: "old", expiresAt: 1000 }),
      ],
      total: 3,
    });
    renderPage();

    const liveRow = (await screen.findByText("live")).closest("tr")!;
    expect(within(liveRow).getByText("Active")).toBeInTheDocument();
    expect(within(liveRow).getByRole("button", { name: "Revoke live" })).toBeInTheDocument();
    const goneRow = screen.getByText("gone").closest("tr")!;
    expect(within(goneRow).getByText("Revoked")).toBeInTheDocument();
    expect(within(goneRow).queryByRole("button")).not.toBeInTheDocument();
    expect(within(screen.getByText("old").closest("tr")!).getByText("Expired")).toBeInTheDocument();
  });

  it("creates a token and reveals it once, closable only with Done", async () => {
    vi.mocked(api.me).mockResolvedValue(admin);
    vi.mocked(api.listPats).mockResolvedValue({ pats: [], total: 0 });
    vi.mocked(api.createPat).mockResolvedValue({
      plaintext: "bdam_pat_secretvalue",
      id: 9,
      name: "ci",
      scopes: ["admin", "pairings:write"],
      createdAt: 1700000000,
      hashedKeyPrefix: "ffff0000",
    });
    renderPage();

    const form = await screen.findByRole("form", { name: "Create token" });
    await userEvent.type(within(form).getByLabelText("Name"), "ci");
    const pairingsScope = within(form).getByRole("checkbox", { name: "pairings:write" });
    expect(pairingsScope).toHaveAccessibleDescription("Companion pairings");
    await userEvent.click(pairingsScope);
    await userEvent.click(within(form).getByRole("button", { name: "Create token" }));

    await waitFor(() => expect(api.createPat).toHaveBeenCalled());
    expect(vi.mocked(api.createPat).mock.calls[0][0]).toEqual({ name: "ci", scopes: ["admin", "pairings:write"] });

    const dialog = await screen.findByRole("dialog", { name: 'Token "ci" created' });
    const tokenField = within(dialog).getByLabelText("New token");
    expect(tokenField).toHaveValue("bdam_pat_secretvalue");
    expect(tokenField).toHaveAccessibleDescription("Copy this token now. It won't be shown again.");
    expect(dialog).toHaveAttribute("aria-modal", "true");

    // A one-time secret must not vanish on Escape or a backdrop click.
    await userEvent.keyboard("{Escape}");
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await userEvent.click(dialog.parentElement!);
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    await userEvent.click(within(dialog).getByRole("button", { name: "Done" }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.queryByDisplayValue("bdam_pat_secretvalue")).not.toBeInTheDocument();
  });

  it("validates the form before calling the server", async () => {
    vi.mocked(api.me).mockResolvedValue(admin);
    vi.mocked(api.listPats).mockResolvedValue({ pats: [], total: 0 });
    renderPage();

    const form = await screen.findByRole("form", { name: "Create token" });
    await userEvent.click(within(form).getByRole("button", { name: "Create token" }));
    expect(within(form).getByRole("alert")).toHaveTextContent("Give the token a name.");

    await userEvent.type(within(form).getByLabelText("Name"), "x");
    await userEvent.click(within(form).getByRole("checkbox", { name: "admin" }));
    await userEvent.click(within(form).getByRole("button", { name: "Create token" }));
    expect(within(form).getByRole("alert")).toHaveTextContent("Pick at least one scope.");

    await userEvent.click(within(form).getByRole("checkbox", { name: "admin" }));
    await userEvent.type(within(form).getByLabelText("Expires (optional)"), "2001-01-01");
    await userEvent.click(within(form).getByRole("button", { name: "Create token" }));
    expect(within(form).getByRole("alert")).toHaveTextContent("The expiry date must be in the future.");

    expect(api.createPat).not.toHaveBeenCalled();
  });

  it("revokes a token after confirmation", async () => {
    vi.mocked(api.me).mockResolvedValue(admin);
    vi.mocked(api.listPats).mockResolvedValueOnce({ pats: [pat({ id: 4, name: "laptop" })], total: 1 });
    vi.mocked(api.listPats).mockResolvedValue({ pats: [pat({ id: 4, name: "laptop", revokedAt: 1700001000 })], total: 1 });
    vi.mocked(api.revokePat).mockResolvedValue({ ok: true });
    renderPage();

    await userEvent.click(await screen.findByRole("button", { name: "Revoke laptop" }));
    const dialog = screen.getByRole("dialog", { name: 'Revoke "laptop"?' });
    await userEvent.click(within(dialog).getByRole("button", { name: "Revoke" }));

    await waitFor(() => expect(api.revokePat).toHaveBeenCalledWith(4));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(await screen.findByText("Revoked")).toBeInTheDocument();
  });
});
