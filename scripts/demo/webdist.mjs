// Decides whether the demo has to rebuild web/dist before embedding it.
// Mirrors the Makefile's web-ensure verdict, so `make demo` and the screenshot
// pipelines never serve a stale SPA after the checkout moves forward, nor the
// placeholder .github/ci-prebuild.sh writes for `make test`/`make build`.
import { closeSync, existsSync, fstatSync, openSync, readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";

/** Prefix of the placeholder index.html written by .github/ci-prebuild.sh. */
export const STUB_MARKER = "<!doctype html><title>branchDAM</title>";
/** ci-prebuild.sh's is_stub() also treats any index.html this small as a stub. */
export const STUB_MAX_BYTES = 200;

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
 * as newer than the build. Infinity when an entry vanishes mid-walk (removed
 * between readdir and stat): the tree is changing, so it can't be current.
 */
export function newestMtime(dir, stat = statSync) {
  const own = stat(dir, { throwIfNoEntry: false });
  if (!own) return Infinity;
  let entries;
  try {
    entries = readdirSync(dir, { withFileTypes: true });
  } catch (err) {
    if (err.code === "ENOENT") return Infinity;
    throw err;
  }
  let newest = own.mtimeMs;
  for (const entry of entries) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) newest = Math.max(newest, newestMtime(p, stat));
    else if (entry.isFile()) {
      const st = stat(p, { throwIfNoEntry: false });
      if (!st) return Infinity;
      newest = Math.max(newest, st.mtimeMs);
    }
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
    // Same test as ci-prebuild.sh's is_stub(): small, or the marker within
    // the first 100 bytes (`head -c 100 | grep -qF`).
    if (st.size < STUB_MAX_BYTES || readFileSync(fd).subarray(0, 100).includes(STUB_MARKER))
      return "web/dist is the build stub";
    built = st.mtimeMs;
  } finally {
    closeSync(fd);
  }
  if (existsSync(path.join(webRoot, "dist", ".branchdam-stale"))) return "web/dist is marked stale";
  // web/public is copied into dist verbatim (Vite's publicDir).
  for (const dir of ["src", "public"]) {
    const p = path.join(webRoot, dir);
    if (!existsSync(p)) continue;
    const newest = newestMtime(p);
    if (newest === Infinity) return `web/${dir} changed while it was being checked`;
    if (newest > built) return `web/${dir} is newer than web/dist`;
  }
  const inputs = readdirSync(webRoot).filter((n) => INPUT_PATTERN.test(n) || INPUT_FILES.includes(n));
  for (const name of inputs.sort()) {
    // Listed but not stat-able: a dangling symlink (say web/.env), or removed
    // since readdir. Can't prove the bundle current, so rebuild, not crash.
    const st = statSync(path.join(webRoot, name), { throwIfNoEntry: false });
    if (!st) return `web/${name} is a dangling link or was removed`;
    if (st.mtimeMs > built) return `web/${name} is newer than web/dist`;
  }
  // web/ itself changes when a whole entry (say web/public) is removed.
  if (statSync(webRoot).mtimeMs > built) return "web/ changed after web/dist was built";
  return null;
}
