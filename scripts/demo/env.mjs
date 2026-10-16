import { existsSync } from "node:fs";
import net from "node:net";
import os from "node:os";
import path from "node:path";

/**
 * Isolation for the throwaway demo instance (`make demo`, `make screenshots`).
 *
 * A developer checkout's `config.yaml` points at the developer's own database
 * and storage roots, and the shell may carry BRANCHDAM_SECRET_KEY,
 * ADMIN_BOOTSTRAP_PAT or Immich credentials. So the demo never reads
 * `config.yaml`: it writes its own config into a scratch directory, starts the
 * server with an environment built from scratch, and refuses to start if the
 * environment or the config could reach anything outside that directory.
 */

const PASSTHROUGH = ["PATH", "LANG", "LC_ALL", "TERM", "TZ"];
// Plural forms too (`*_KEYS`, `*_TOKENS`); MULLION_* is refused by prefix below.
const SECRET_KEY = /(SECRETS?|PASSWORDS?|_KEYS?|_TOKENS?|_PATS?)$|ENCRYPTION/;
const PATH_KEY = /(_DIR|_PATH|_FILE|_CONFIG)$/;
const PATH_ALLOW = new Set(["PLAYWRIGHT_BROWSERS_PATH", "SCREENSHOTS_OUT_DIR"]);
/** The default port of a real local branchDAM; a demo must never take it. */
const RESERVED_PORTS = new Set(["8080"]);

export function playwrightBrowsersPath(parent = process.env) {
  return parent.PLAYWRIGHT_BROWSERS_PATH ?? path.join(os.homedir(), ".cache", "ms-playwright");
}

/** A clean environment for a demo child process. */
export function demoEnv(demoDir, vars = {}, parent = process.env) {
  const env = {};
  for (const key of PASSTHROUGH) if (parent[key] !== undefined) env[key] = parent[key];
  return { ...env, HOME: path.join(demoDir, "home"), TMPDIR: path.join(demoDir, "tmp"), ...vars };
}

const inside = (dir, p) => {
  const rel = path.relative(dir, p);
  return rel === "" || (!rel.startsWith("..") && !path.isAbsolute(rel));
};

/** Problems with a demo child's environment (empty when it is safe). */
export function demoEnvProblems(env, demoDir) {
  const problems = [];
  for (const [key, value] of Object.entries(env)) {
    if (value === undefined) continue;
    if (key.startsWith("MULLION_")) problems.push(`${key} must not reach the demo`);
    if (SECRET_KEY.test(key)) problems.push(`${key} looks like a real secret; the demo never sets it`);
    if (PATH_KEY.test(key) && !PATH_ALLOW.has(key) && path.isAbsolute(value) && !inside(demoDir, value))
      problems.push(`${key}=${value} is outside the demo dir`);
  }
  for (const key of ["HOME", "TMPDIR", "BRANCHDAM_CONFIG"])
    if (!env[key] || !inside(demoDir, env[key])) problems.push(`${key} must point inside the demo dir`);
  return problems;
}

/** Problems with the generated server config (empty when it is safe). */
export function demoConfigProblems(cfg, demoDir) {
  const problems = [];
  const mustBeInside = (label, p) => {
    if (!p || !path.isAbsolute(p) || !inside(demoDir, p)) problems.push(`${label} must be inside the demo dir`);
  };
  const [host, port] = String(cfg.listenAddr ?? "").split(":");
  if (host !== "127.0.0.1") problems.push("listenAddr must bind 127.0.0.1");
  if (RESERVED_PORTS.has(port)) problems.push(`port ${port} is reserved for a real instance`);
  // DEMO_PORT comes from the developer's shell: only an unprivileged port is accepted.
  if (!/^\d+$/.test(port ?? "") || Number(port) < 1024 || Number(port) > 65535)
    problems.push(`port ${port} must be a number from 1024 to 65535`);
  mustBeInside("database.path", cfg.database?.path);
  mustBeInside("thumbnails.cacheDir", cfg.thumbnails?.cacheDir);
  mustBeInside("immich.exportPath", cfg.immich?.exportPath);
  if (cfg.immich?.apiUrl) problems.push("immich.apiUrl must be empty (no Immich in the demo)");
  if (cfg.auth?.mode !== "forward") problems.push("auth.mode must be forward (headers from the demo browser)");
  if (cfg.metadata?.autoInherit !== false)
    problems.push("metadata.autoInherit must be false (no writes into media files)");
  for (const loc of cfg.storageLocations ?? []) mustBeInside(`storage location ${loc.name}`, loc.rootPath);
  return problems;
}

export function assertDemo(env, cfg, demoDir, { cwd } = {}) {
  const problems = [...demoEnvProblems(env, demoDir), ...demoConfigProblems(cfg, demoDir)];
  if (cwd && existsSync(path.join(cwd, "config.yaml")))
    problems.push(`${cwd} contains a config.yaml the server could pick up`);
  if (problems.length > 0)
    throw new Error(`demo environment refused:\n  - ${problems.join("\n  - ")}`);
}

/** Rejects if something already listens on one of the ports (likely a real instance). */
export async function assertPortsFree(ports, hosts = ["127.0.0.1", "::1"]) {
  const listening = (port, host) =>
    new Promise((resolve) => {
      const socket = net.connect({ port: Number(port), host });
      socket.once("connect", () => {
        socket.destroy();
        resolve(true);
      });
      socket.once("error", () => resolve(false));
    });
  for (const port of ports)
    for (const host of hosts)
      if (await listening(port, host))
        throw new Error(`port ${port} is already in use on ${host}; refusing to start the demo`);
}
