// Decides whether the demo has to rebuild web/dist before embedding it.
// Mirrors the Makefile's web-ensure verdict, so `make demo` and the screenshot
// pipelines never serve a stale SPA after the checkout moves forward, nor the
// placeholder .github/ci-prebuild.sh writes for `make test`/`make build`.
import {
  closeSync,
  existsSync,
  fstatSync,
  lstatSync,
  openSync,
  readFileSync,
  readdirSync,
  statSync,
} from "node:fs";
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
 * stat(p) or undefined when p can't be stat'ed at all: gone (ENOENT, a
 * dangling symlink), replaced mid-walk (ENOTDIR) or unreadable (EACCES).
 */
function tryStat(stat, p) {
  try {
    return stat(p, { throwIfNoEntry: false });
  } catch {
    return undefined;
  }
}

/**
 * Newest mtime (ms) of dir, any file under it, or any subdirectory. Directory
 * mtimes count so a checkout that only deletes or renames a file still reads
 * as newer than the build. Infinity when any part of the walk can't be read
 * (an entry removed between readdir and stat, a directory swapped for a file,
 * no access): the bundle can't be proven current, so the caller rebuilds and
 * a real problem surfaces from `npm run build` rather than from this check.
 */
export function newestMtime(dir, stat = statSync) {
  const own = tryStat(stat, dir);
  if (!own) return Infinity;
  let entries;
  try {
    entries = readdirSync(dir, { withFileTypes: true });
  } catch {
    return Infinity;
  }
  let newest = own.mtimeMs;
  for (const entry of entries) {
    const p = path.join(dir, entry.name);
    if (entry.isDirectory()) newest = Math.max(newest, newestMtime(p, stat));
    else if (entry.isFile()) {
      const st = tryStat(stat, p);
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
    return "web/dist/index.html can't be read";
  }
  let built;
  try {
    const st = fstatSync(fd);
    let head;
    try {
      // A directory index.html opens fine read-only and fails here (EISDIR).
      head = readFileSync(fd).subarray(0, 100);
    } catch {
      return "web/dist/index.html can't be read";
    }
    // Same test as ci-prebuild.sh's is_stub(): small, or the marker within
    // the first 100 bytes (`head -c 100 | grep -qF`).
    if (st.size < STUB_MAX_BYTES || head.includes(STUB_MARKER)) return "web/dist is the build stub";
    built = st.mtimeMs;
  } finally {
    closeSync(fd);
  }
  if (existsSync(path.join(webRoot, "dist", ".branchdam-stale"))) return "web/dist is marked stale";
  // web/public is copied into dist verbatim (Vite's publicDir).
  for (const dir of ["src", "public"]) {
    const p = path.join(webRoot, dir);
    // lstat, not existsSync: a dangling web/src symlink must not read as
    // "no sources"; newestMtime then can't stat it and returns Infinity.
    if (!tryStat(lstatSync, p)) continue;
    const newest = newestMtime(p);
    if (newest === Infinity) return `web/${dir} changed or is unreadable`;
    if (newest > built) return `web/${dir} is newer than web/dist`;
  }
  const inputs = readdirSync(webRoot).filter((n) => INPUT_PATTERN.test(n) || INPUT_FILES.includes(n));
  for (const name of inputs.sort()) {
    // Listed but not stat-able: a dangling symlink (say web/.env), or removed
    // since readdir. Can't prove the bundle current, so rebuild, not crash.
    const st = tryStat(statSync, path.join(webRoot, name));
    if (!st) return `web/${name} can't be read (dangling link, removed or no access)`;
    if (st.mtimeMs > built) return `web/${name} is newer than web/dist`;
  }
  // web/ itself changes when a whole entry (say web/public) is removed.
  if (statSync(webRoot).mtimeMs > built) return "web/ changed after web/dist was built";
  return null;
}
