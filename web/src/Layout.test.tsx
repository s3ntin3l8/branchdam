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

vi.mock("./api/client", () => ({
  api: {
    logout: vi.fn().mockResolvedValue(undefined),
    mfaChallenge: vi.fn().mockResolvedValue({ ok: true }),
  },
}));

import { useMe } from "./hooks/queries";

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
