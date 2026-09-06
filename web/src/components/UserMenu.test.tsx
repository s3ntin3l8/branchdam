import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it } from "vitest";
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

describe("UserMenu", () => {
  it("renders the trigger with monogram initials and the user's name", () => {
    render(<UserMenu me={baseMe} />);
    // First letter of first and last whitespace-separated word.
    expect(screen.getByText("AL")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: /alice liddell/i })).toBeInTheDocument();
  });

  it("does not render the popover before the trigger is clicked", () => {
    render(<UserMenu me={baseMe} />);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(screen.queryByText(/sign out/i)).not.toBeInTheDocument();
  });

  it("opens the popover when the trigger is clicked", async () => {
    const user = userEvent.setup();
    render(<UserMenu me={baseMe} />);
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));
    expect(screen.getByRole("dialog", { name: /account menu/i })).toBeInTheDocument();
  });

  it("shows email, groups, and admin badge when all are present", async () => {
    const user = userEvent.setup();
    render(<UserMenu me={baseMe} />);
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));

    expect(screen.getByText("alice@example.com")).toBeInTheDocument();
    // Both group chips render in some order.
    expect(screen.getByText("dam-admins")).toBeInTheDocument();
    expect(screen.getByText("dam-users")).toBeInTheDocument();
    expect(screen.getByText(/^admin$/i)).toBeInTheDocument();
  });

  it("hides the admin badge when the user is not admin", async () => {
    const user = userEvent.setup();
    render(<UserMenu me={{ ...baseMe, isAdmin: false }} />);
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));

    expect(screen.queryByText(/^admin$/i)).not.toBeInTheDocument();
    // Email and groups still render -- only the badge is conditional.
    expect(screen.getByText("alice@example.com")).toBeInTheDocument();
  });

  it("hides the groups row when no groups are present", async () => {
    const user = userEvent.setup();
    render(<UserMenu me={{ ...baseMe, groups: [] }} />);
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));

    expect(screen.queryByText("dam-admins")).not.toBeInTheDocument();
    expect(screen.queryByText("dam-users")).not.toBeInTheDocument();
  });

  it("hides the email line when no email is present", async () => {
    const user = userEvent.setup();
    const me: Me = { ...baseMe, email: undefined };
    render(<UserMenu me={me} />);
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));

    expect(screen.queryByText(/@example.com/)).not.toBeInTheDocument();
  });

  it("links Sign out to the Authentik outpost sign-out path", async () => {
    const user = userEvent.setup();
    render(<UserMenu me={baseMe} />);
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));

    const link = screen.getByRole("link", { name: /sign out/i });
    expect(link).toBeInTheDocument();
    expect(link.getAttribute("href")).toBe("/outpost.goauthentik.io/sign_out");
  });

  it("closes the popover when clicking outside", async () => {
    const user = userEvent.setup();
    render(
      <div>
        <button type="button">outside</button>
        <UserMenu me={baseMe} />
      </div>
    );
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: /outside/i }));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("closes the popover when Escape is pressed", async () => {
    const user = userEvent.setup();
    render(<UserMenu me={baseMe} />);
    await user.click(screen.getByRole("button", { name: /alice liddell/i }));
    expect(screen.getByRole("dialog")).toBeInTheDocument();

    await user.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("toggles the popover closed when the trigger is clicked twice", async () => {
    const user = userEvent.setup();
    render(<UserMenu me={baseMe} />);
    const trigger = screen.getByRole("button", { name: /alice liddell/i });

    await user.click(trigger);
    expect(screen.getByRole("dialog")).toBeInTheDocument();
    await user.click(trigger);
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });
});
