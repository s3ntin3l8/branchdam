/**
 * What the demo browser and scripts send in place of an auth proxy: the server runs in forward-auth
 * mode, so these headers are the login. Shared by instance.mjs (its own API calls) and
 * visual/hooks.mjs (the visual-kit browser).
 */
export const DEMO_HEADERS = {
  "X-Authentik-Username": "demo",
  "X-Authentik-Email": "demo@example.com",
  "X-Authentik-Groups": "branchdam-admins",
};
