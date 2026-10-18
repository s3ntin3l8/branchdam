// Decides whether the demo has to rebuild web/dist before embedding it.
// Mirrors the Makefile's web-ensure verdict, so `make demo` and the screenshot
// pipelines never serve a stale SPA after the checkout moves forward, nor the
// placeholder .github/ci-prebuild.sh writes for `make test`/`make build`.
import { closeSync, existsSync, fstatSync, openSync, readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";

/** Prefix of the placeholder index.html written by .github/ci-prebuild.sh. */
export const STUB_MARKER = "<!doctype html><title>branchDAM</title>";
/** ci-prebuild.sh's is_stub() also treats any index.html this small as a stub. */
const STUB_MAX_BYTES = 200;

/**
 * Build inputs next to web/src whose changes also invalidate the bundle:
 * `npm run build` is `tsc -b && vite build`, so tsconfigs and .env files count.
 * (tsc's *.tsbuildinfo output doesn't match.)
 */
const INPUT_FILES = ["index.html", "package.json", "package-lock.json", "vite.config.ts"];
const INPUT_PATTERN = /^(tsconfig(\..+)?\.json|\.env(\..+)?)$/;

/**
 * Newest mtime (ms) of dir, any file under it, or any subdirectory. Directory
 * mtimes count so a checkout that only deletes or renames a file still reads
 * as newer than the build.
 */
function newestMtime(dir) {
  let newest = statSync(dir).mtimeMs;
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) newest = Math.max(newest, newestMtime(p));
    else if (entry.isFile()) newest = Math.max(newest, statSync(p).mtimeMs);
  }
  return newest;
}

/**
 * Why web/dist under webRoot must be rebuilt, or null when it is current.
 * Stale means: missing, the ci-prebuild stub, flagged by web-ensure's
 * .branchdam-stale marker, or older than web/src, web/public, another build
 * input, or web/ itself.
 */
export function webDistStaleReason(webRoot) {
  const index = path.join(webRoot, "dist", "index.html");
  if (!existsSync(path.join(webRoot, "dist", "assets"))) return "web/dist is missing";
  // One open for the stub check and the build time, so both see the same file.
  let fd;
  try {
    fd = openSync(index, "r");
  } catch (err) {
    if (err.code === "ENOENT") return "web/dist is missing";
    throw err;
  }
  let built;
  try {
    const st = fstatSync(fd);
    if (st.size < STUB_MAX_BYTES || readFileSync(fd, "utf8").startsWith(STUB_MARKER))
      return "web/dist is the build stub";
    built = st.mtimeMs;
  } finally {
    closeSync(fd);
  }
  if (existsSync(path.join(webRoot, "dist", ".branchdam-stale"))) return "web/dist is marked stale";
  // web/public is copied into dist verbatim (Vite's publicDir).
  for (const dir of ["src", "public"]) {
    const p = path.join(webRoot, dir);
    if (existsSync(p) && newestMtime(p) > built) return `web/${dir} is newer than web/dist`;
  }
  const inputs = readdirSync(webRoot).filter((n) => INPUT_PATTERN.test(n) || INPUT_FILES.includes(n));
  for (const name of inputs.sort()) {
    if (statSync(path.join(webRoot, name)).mtimeMs > built) return `web/${name} is newer than web/dist`;
  }
  // web/ itself changes when a whole entry (say web/public) is removed.
  if (statSync(webRoot).mtimeMs > built) return "web/ changed after web/dist was built";
  return null;
}
