import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryRouter, RouterProvider } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { routeConfig } from "./router";

// The asset list fires a dozen queries of its own; these tests are about the
// shell around it, so a stub keeps the fetch fake below small.
vi.mock("./pages/AssetListPage", () => ({
  default: () => <div>asset list page</div>,
}));

describe("routeConfig", () => {
  it("registers /login and /password-reset as top-level routes outside the Layout shell", () => {
    const topLevel = routeConfig.map((route) => route.path);
    expect(topLevel).toEqual(expect.arrayContaining(["/login", "/password-reset", "/"]));

    const layoutRoute = routeConfig.find((route) => route.path === "/");
    const childPaths = layoutRoute?.children?.map((route) => route.path) ?? [];
    expect(childPaths).not.toContain("login");
    expect(childPaths).not.toContain("password-reset");
    expect(childPaths).toContain("mfa/setup");
    expect(childPaths).toContain("account");

    const passwordResetRoute = routeConfig.find((route) => route.path === "/password-reset");
    expect(passwordResetRoute?.element).toBeDefined();
  });

  it("ends the Layout's children with a catch-all route", () => {
    const layoutRoute = routeConfig.find((route) => route.path === "/");
    const last = layoutRoute?.children?.[layoutRoute.children.length - 1];
    expect(last?.path).toBe("*");
  });
});

// ---- Mounted routing: the real routeConfig in a memory router ----

class FakeEventSource {
  static instances: FakeEventSource[] = [];
  url: string;
  readyState = 0;
  constructor(url: string) {
    this.url = url;
    FakeEventSource.instances.push(this);
  }
  addEventListener() {}
  removeEventListener() {}
  close() {
    this.readyState = 2;
  }
}

type Mode = "local" | "both" | "forward";

interface FakeServer {
  signedIn: boolean;
  mode: Mode;
  calls: string[];
}

function json(status: number, body: unknown) {
  return Promise.resolve({
    ok: status >= 200 && status < 300,
    status,
    statusText: "test",
    json: async () => body,
  });
}

// installFakeServer mimics the server's auth gate: in local/both mode an
// anonymous request has no Principal and every gated route answers 403
// "authentication required"; in forward mode /me answers 200 for an
// anonymous browser with authenticated:false.
function installFakeServer(server: FakeServer) {
  vi.stubGlobal(
    "fetch",
    vi.fn((input: string, init?: RequestInit) => {
      const path = input.split("?")[0];
      const method = init?.method ?? "GET";
      server.calls.push(`${method} ${path}`);
      if (path === "/api/v1/setup/status") {
        return json(200, { readyForSetup: false, mode: server.mode });
      }
      if (path === "/api/v1/login" && method === "POST") {
        server.signedIn = true;
        return json(200, { ok: true });
      }
      if (!server.signedIn) {
        if (server.mode === "forward" && path === "/api/v1/me") {
          return json(200, { kind: "user", authenticated: false, isAdmin: false });
        }
        if (server.mode !== "forward") {
          return json(403, { detail: "authentication required" });
        }
      }
      if (path === "/api/v1/me") {
        return json(200, { kind: "user", authenticated: true, isAdmin: true, isLocal: true, localUserId: 1 });
      }
      if (path === "/api/v1/assets") {
        return json(200, { assets: [], total: 0 });
      }
      return json(200, {});
    }),
  );
}

function renderAt(path: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const router = createMemoryRouter(routeConfig, { initialEntries: [path] });
  render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
  return router;
}

describe("auth shell routing", () => {
  const originalEventSource = globalThis.EventSource;
  let server: FakeServer;

  beforeEach(() => {
    FakeEventSource.instances = [];
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    globalThis.EventSource = FakeEventSource as any;
    server = { signedIn: false, mode: "local", calls: [] };
    installFakeServer(server);
  });

  afterEach(() => {
    globalThis.EventSource = originalEventSource;
    vi.unstubAllGlobals();
  });

  it("redirects a signed-out visitor to /login and returns them to the page they asked for", async () => {
    const router = renderAt("/assets?unlinked=1");

    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/login");
    // The login page renders without the app shell, and no live-update
    // stream is opened for a signed-out session.
    expect(screen.queryByRole("link", { name: "Audit Queue" })).not.toBeInTheDocument();
    expect(FakeEventSource.instances).toHaveLength(0);

    const user = userEvent.setup();
    await user.type(screen.getByLabelText("Username"), "alice");
    await user.type(screen.getByLabelText("Password"), "correct horse");
    await user.click(screen.getByRole("button", { name: "Sign in" }));

    expect(await screen.findByText("asset list page")).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/assets");
    expect(router.state.location.search).toBe("?unlinked=1");
    expect(screen.getByRole("link", { name: "Audit Queue" })).toBeInTheDocument();
    // Signing in opens the live-update stream without a reload.
    await waitFor(() => expect(FakeEventSource.instances).toHaveLength(1));
    expect(FakeEventSource.instances[0].url).toBe("/api/v1/events");
  });

  it("redirects in 'both' mode too", async () => {
    server.mode = "both";
    const router = renderAt("/");
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/login");
  });

  it("keeps the shell for an anonymous forward-auth visitor (no local login page)", async () => {
    server.mode = "forward";
    const router = renderAt("/");
    await waitFor(() => expect(server.calls).toContain("GET /api/v1/setup/status"));
    expect(await screen.findByRole("link", { name: "Audit Queue" })).toBeInTheDocument();
    expect(screen.getByText("asset list page")).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/");
    expect(FakeEventSource.instances).toHaveLength(0);
  });

  it("renders /login without the sidebar or a /me probe", async () => {
    renderAt("/login");
    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Audit Queue" })).not.toBeInTheDocument();
    expect(server.calls).not.toContain("GET /api/v1/me");
  });

  it("renders /password-reset without the sidebar or a /me probe", async () => {
    renderAt("/password-reset");
    expect(await screen.findByRole("heading", { name: "Invalid reset link" })).toBeInTheDocument();
    expect(screen.queryByRole("link", { name: "Audit Queue" })).not.toBeInTheDocument();
    expect(server.calls).not.toContain("GET /api/v1/me");
  });

  it("shows a 'Page not found' page with a link home for an unknown path", async () => {
    server.signedIn = true;
    renderAt("/no/such/page");
    expect(await screen.findByRole("heading", { name: "Page not found" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Go to assets" })).toHaveAttribute("href", "/");
    // Still inside the shell.
    expect(screen.getByRole("link", { name: "Audit Queue" })).toBeInTheDocument();
  });

  it("renders the lazily loaded /account page inside the shell", async () => {
    server.signedIn = true;
    renderAt("/account");
    expect(await screen.findByRole("heading", { name: "Account security" })).toBeInTheDocument();
    expect(screen.getByRole("link", { name: "Audit Queue" })).toBeInTheDocument();
  });

  it("sends a session that expires mid-use to /login", async () => {
    server.signedIn = true;
    const router = renderAt("/");
    expect(await screen.findByText("asset list page")).toBeInTheDocument();

    // The session cookie expires; the next API failure re-checks /me.
    server.signedIn = false;
    window.dispatchEvent(
      new CustomEvent("api-auth-error", {
        detail: { status: 403, message: "authentication required", path: "/api/v1/assets" },
      }),
    );

    expect(await screen.findByRole("heading", { name: "Sign in" })).toBeInTheDocument();
    expect(router.state.location.pathname).toBe("/login");
    expect(router.state.location.state).toEqual({ from: "/" });
  });
});
