// Decides whether the demo has to rebuild web/dist before embedding it.
// Mirrors the Makefile's web-ensure verdict, so `make demo` and the screenshot
// pipelines never serve a stale SPA after the checkout moves forward, nor the
// placeholder .github/ci-prebuild.sh writes for `make test`/`make build`.
import { existsSync, readFileSync, readdirSync, statSync } from "node:fs";
import path from "node:path";

/** Prefix of the placeholder index.html written by .github/ci-prebuild.sh. */
export const STUB_MARKER = "<!doctype html><title>branchDAM</title>";
/** ci-prebuild.sh's is_stub() also treats any index.html this small as a stub. */
const STUB_MAX_BYTES = 200;

/** Build inputs next to web/src whose changes also invalidate the bundle. */
const INPUT_FILES = ["index.html", "package.json", "package-lock.json", "vite.config.ts"];

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
 * .branchdam-stale marker, or older than web/src, web/public or another
 * build input.
 */
export function webDistStaleReason(webRoot) {
  const index = path.join(webRoot, "dist", "index.html");
  if (!existsSync(index) || !existsSync(path.join(webRoot, "dist", "assets")))
    return "web/dist is missing";
  if (
    statSync(index).size < STUB_MAX_BYTES ||
    readFileSync(index, "utf8").startsWith(STUB_MARKER)
  )
    return "web/dist is the build stub";
  if (existsSync(path.join(webRoot, "dist", ".branchdam-stale"))) return "web/dist is marked stale";
  const built = statSync(index).mtimeMs;
  // web/public is copied into dist verbatim (Vite's publicDir).
  for (const dir of ["src", "public"]) {
    const p = path.join(webRoot, dir);
    if (existsSync(p) && newestMtime(p) > built) return `web/${dir} is newer than web/dist`;
  }
  for (const name of INPUT_FILES) {
    const p = path.join(webRoot, name);
    if (existsSync(p) && statSync(p).mtimeMs > built) return `web/${name} is newer than web/dist`;
  }
  return null;
}
