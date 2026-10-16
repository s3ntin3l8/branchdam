import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryRouter, RouterProvider } from "react-router";
import { describe, it, expect, vi, beforeEach } from "vitest";
import { Layout } from "./App";

vi.mock("./hooks/useEventStream", () => ({
  useEventStream: vi.fn(() => ({ disconnected: false })),
}));

vi.mock("./hooks/queries", () => ({
  useMe: vi.fn(),
  useUnlinkedCount: vi.fn(() => ({ data: 0 })),
}));

vi.mock("./api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("./api/client")>();
  return {
    ...actual,
    api: {
      logout: vi.fn().mockResolvedValue(undefined),
      mfaChallenge: vi.fn().mockResolvedValue({ ok: true }),
      setupStatus: vi.fn().mockResolvedValue({ readyForSetup: false, mode: "local" }),
    },
  };
});

import { useMe } from "./hooks/queries";
import { useEventStream } from "./hooks/useEventStream";

function renderLayout(meData: ReturnType<typeof useMe>["data"]) {
  const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  (useMe as ReturnType<typeof vi.fn>).mockReturnValue({ data: meData });

  const router = createMemoryRouter(
    [
      {
        path: "/",
        element: <Layout />,
        children: [
          { index: true, element: <div>child content</div> },
        ],
      },
    ],
    { initialEntries: ["/"] },
  );

  return render(
    <QueryClientProvider client={qc}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  vi.clearAllMocks();
});

describe("Layout MFA challenge gate", () => {
  it("renders MFA challenge form when me.mfaRequired is true", () => {
    renderLayout({
      kind: "user",
      authenticated: true,
      isAdmin: false,
      isLocal: true,
      localUserId: 1,
      mfaRequired: true,
      mfaVerified: false,
    });

    expect(screen.getByText(/two-factor authentication/i)).toBeInTheDocument();
    expect(screen.getByPlaceholderText("123456")).toBeInTheDocument();
  });

  it("does NOT render child content when mfaRequired is true", () => {
    renderLayout({
      kind: "user",
      authenticated: true,
      isAdmin: false,
      isLocal: true,
      localUserId: 1,
      mfaRequired: true,
      mfaVerified: false,
    });

    expect(screen.queryByText("child content")).not.toBeInTheDocument();
  });

  it("renders nav shell and child content when mfaRequired is false", () => {
    renderLayout({
      kind: "user",
      authenticated: true,
      isAdmin: false,
      isLocal: true,
      localUserId: 1,
      mfaRequired: false,
      mfaVerified: true,
    });

    expect(screen.queryByText(/two-factor authentication/i)).not.toBeInTheDocument();
    expect(screen.getByText("child content")).toBeInTheDocument();
  });

  it("renders nav shell when me has no MFA fields (non-local user)", () => {
    renderLayout({
      kind: "user",
      authenticated: true,
      isAdmin: true,
    });

    expect(screen.queryByText(/two-factor authentication/i)).not.toBeInTheDocument();
    expect(screen.getByText("child content")).toBeInTheDocument();
  });

  it("shows sign-out button on challenge view", () => {
    renderLayout({
      kind: "user",
      authenticated: true,
      isAdmin: false,
      isLocal: true,
      localUserId: 1,
      mfaRequired: true,
      mfaVerified: false,
    });

    expect(screen.getByRole("button", { name: /sign out/i })).toBeInTheDocument();
  });
});

describe("Layout live-update stream gate", () => {
  it("opens the stream for a signed-in session", () => {
    renderLayout({ kind: "user", authenticated: true, isAdmin: true });
    expect(useEventStream).toHaveBeenLastCalledWith({ enabled: true });
  });

  it("keeps it closed while an MFA challenge is pending", () => {
    renderLayout({
      kind: "user",
      authenticated: true,
      isAdmin: false,
      isLocal: true,
      localUserId: 1,
      mfaRequired: true,
      mfaVerified: false,
    });
    expect(useEventStream).toHaveBeenLastCalledWith({ enabled: false });
  });

  it("keeps it closed for an anonymous session", () => {
    renderLayout({ kind: "user", authenticated: false, isAdmin: false });
    expect(useEventStream).toHaveBeenLastCalledWith({ enabled: false });
  });
});

describe("Layout collapsible navigation", () => {
  function renderAt(path: string) {
    const qc = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    (useMe as ReturnType<typeof vi.fn>).mockReturnValue({
      data: { kind: "user", authenticated: true, isAdmin: true },
    });
    const router = createMemoryRouter(
      [
        {
          path: "/",
          element: <Layout />,
          children: [
            { index: true, element: <div>home content</div> },
            { path: "audit", element: <div>audit content</div> },
          ],
        },
      ],
      { initialEntries: [path] },
    );
    return render(
      <QueryClientProvider client={qc}>
        <RouterProvider router={router} />
      </QueryClientProvider>,
    );
  }

  it("toggles the sidebar drawer from the menu button", async () => {
    const user = (await import("@testing-library/user-event")).default.setup();
    renderAt("/");

    const toggle = screen.getByRole("button", { name: "Open navigation" });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    expect(toggle).toHaveAttribute("aria-controls", "app-nav");
    expect(screen.getByRole("navigation", { name: "Main" })).toHaveClass("hidden", "md:block");

    await user.click(toggle);
    expect(screen.getByRole("button", { name: "Close navigation" })).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("navigation", { name: "Main" })).toHaveClass("fixed");

    await user.keyboard("{Escape}");
    expect(screen.getByRole("button", { name: "Open navigation" })).toHaveAttribute("aria-expanded", "false");
  });

  it("closes the drawer after navigating", async () => {
    const user = (await import("@testing-library/user-event")).default.setup();
    renderAt("/");

    await user.click(screen.getByRole("button", { name: "Open navigation" }));
    await user.click(screen.getByRole("link", { name: "Audit Queue" }));

    expect(await screen.findByText("audit content")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Open navigation" })).toHaveAttribute("aria-expanded", "false");
  });
});
