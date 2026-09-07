import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it } from "vitest";
import type { ReactNode } from "react";
import type { Me } from "../api/types";
import { UserMenu } from "./UserMenu";

const baseMe: Me = {
  kind: "user",
  name: "Alice Liddell",
  email: "alice@example.com",
  groups: ["dam-admins", "dam-users"],
  authenticated: true,
  isAdmin: true,
};

// UserMenu now hosts a useMutation (api.logout) for local-auth sign-out,
// so tests need a QueryClient. The wrapper is intentionally lightweight --
// no QueryClientProvider hooks are exercised here, only the trigger-click
// surface that opens the popover.
function withQueryClient(node: ReactNode) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return <QueryClientProvider client={qc}>{node}</QueryClientProvider>;
}

describe("UserMenu", () => {
  it("renders the trigger with monogram initials and the user's name", () => {
    render(withQueryClient(<UserMenu me={baseMe} />));
    expect(screen.getByText("AL")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /alice liddell/i })).toBeInTheDocument();
  });

  it("does not render the popover before the trigger is clicked", () => {
    render(withQueryClient(<UserMenu me={baseMe} />));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.queryByText(/sign out/i)).not.toBeInTheDocument();
  });

  it("opens the popover when the trigger is clicked", async () => {
    const user = userEvent.setup();
    render(withQueryClient(<UserMenu me={baseMe} />));
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));
    expect(screen.getByRole("dialog", { name: /account menu/i })).toBeInTheDocument();
  });

  it("shows email, groups, and admin badge when all are present", async () => {
    const user = userEvent.setup();
    render(withQueryClient(<UserMenu me={baseMe} />));
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));

    expect(screen.getByText("alice@example.com")).toBeInTheDocument();
    expect(screen.getByText("dam-admins")).toBeInTheDocument();
    expect(screen.getByText("dam-users")).toBeInTheDocument();
    expect(screen.getByText(/^admin$/i)).toBeInTheDocument();
  });

  it("hides the admin badge when the user is not admin", async () => {
    const user = userEvent.setup();
    render(withQueryClient(<UserMenu me={{ ...baseMe, isAdmin: false }} />));
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));

    expect(screen.queryByText(/^admin$/i)).not.toBeInTheDocument();
    expect(screen.getByText("alice@example.com")).toBeInTheDocument();
  });

  it("hides the groups row when no groups are present", async () => {
    const user = userEvent.setup();
    render(withQueryClient(<UserMenu me={{ ...baseMe, groups: [] }} />));
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));

    expect(screen.queryByText("dam-admins")).not.toBeInTheDocument();
    expect(screen.queryByText("dam-users")).not.toBeInTheDocument();
  });

  it("hides the email line when no email is present", async () => {
    const user = userEvent.setup();
    const me: Me = { ...baseMe, email: undefined };
    render(withQueryClient(<UserMenu me={me} />));
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));

    expect(screen.queryByText(/@example.com/)).not.toBeInTheDocument();
  });

  it("links Sign out to the Authentik outpost sign-out path when not local", async () => {
    const user = userEvent.setup();
    render(withQueryClient(<UserMenu me={baseMe} />));
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));

    const link = screen.getByRole("link", { name: /sign out/i });
    expect(link).toBeInTheDocument();
    expect(link.getAttribute("href")).toBe("/outpost.goauthentik.io/sign_out");
  });

  it("renders Sign out as a button when the session is local", async () => {
    const user = userEvent.setup();
    const localMe: Me = { ...baseMe, isLocal: true, localUserId: 42 };
    render(withQueryClient(<UserMenu me={localMe} />));
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));

    const button = screen.getByRole("button", { name: /sign out/i });
    expect(button).toBeInTheDocument();
    expect(button.tagName).toBe("BUTTON");
  });

  it("closes the popover when clicking outside", async () => {
    const user = userEvent.setup();
    render(
      withQueryClient(
        <div>
          <button type="button">outside</button>
          <UserMenu me={baseMe} />
        </div>
      )
    );
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /outside/i }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("closes the popover when Escape is pressed", async () => {
    const user = userEvent.setup();
    render(withQueryClient(<UserMenu me={baseMe} />));
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("toggles the popover closed when the trigger is clicked twice", async () => {
    const user = userEvent.setup();
    render(withQueryClient(<UserMenu me={baseMe} />));
    const trigger = screen.getByRole("button", { name: /alice liddell/i });

    await user.click(trigger);
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await user.click(trigger);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});
