// returnPathFrom picks the in-app path Layout stashed in location state
// ({ from }) when it redirected a signed-out visitor to /login, falling
// back to "/". Only a same-origin absolute path is accepted -- "//host"
// and "/\host" are protocol-relative to a browser, and LoginPage also
// feeds this to window.location.assign for SSO -- and never /login itself.
export function returnPathFrom(state: unknown): string {
  const from = (state as { from?: unknown } | null)?.from;
  if (typeof from !== "string" || !from.startsWith("/")) return "/";
  if (from[1] === "/" || from[1] === "\\") return "/";
  if (from === "/login" || from.startsWith("/login?") || from.startsWith("/login#")) return "/";
  return from;
}
