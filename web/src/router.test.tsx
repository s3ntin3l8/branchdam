import { describe, expect, it } from "vitest";
import { routeConfig } from "./router";

// Pins the production route tree's registration of /password-reset.
// Hermes flagged (PR #460) that the route didn't exist at all in the
// production router at one point -- this test would have caught that,
// and guards against a future prune of the route tree silently
// regressing it back to a dead-end link.
describe("routeConfig", () => {
  it("registers /password-reset nested inside the Layout shell, same as /login", () => {
    const layoutRoute = routeConfig.find((route) => route.path === "/");
    expect(layoutRoute).toBeDefined();

    const passwordResetRoute = layoutRoute?.children?.find((route) => route.path === "password-reset");
    expect(passwordResetRoute).toBeDefined();
    expect(passwordResetRoute?.element).toBeDefined();

    const loginRoute = layoutRoute?.children?.find((route) => route.path === "login");
    expect(loginRoute).toBeDefined();
  });
});
