/// <reference types="vitest/config" />
import { execSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import path, { join } from "node:path";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// resolveBuildId is stamped into __BRANCHDAM_BUILD__ and web/dist/BUILD_ID.
// Precedence, so a release still shows its tag and a PR/edge image shows
// the commit it was actually built from (s3ntin3l8/.github#97, #611):
//
//   BRANCHDAM_BUILD_ID  explicit override. release.yml passes the tag.
//   COMMIT_SHA          injected by docker-publish from `git rev-parse HEAD`
//                       of the checkout — not `${{ github.sha }}`, which is
//                       only the commit that triggered the caller.
//   dev-docker          neither set. A local `docker build` lands here
//                       (.git is dockerignored, so there is no SHA to read).
//
// On a host checkout with neither env var set we still prefer
// `git rev-parse --short HEAD`, plus `-dirty` when the worktree is dirty.
// Vite applies `define` to the dev server too, and Settings compares the
// inlined id with the embedded BUILD_ID after stripping that suffix — a
// dirty `make dev-web` must not look like a stale embed. dev-docker is the
// fallback when git isn't available (image build, tarball).
function resolveBuildId(): string {
  const override = process.env.BRANCHDAM_BUILD_ID?.trim();
  if (override) return override;
  const commit = process.env.COMMIT_SHA?.trim();
  if (commit) return commit;
  try {
    const sha = execSync("git rev-parse --short HEAD", {
      stdio: ["ignore", "pipe", "ignore"],
    })
      .toString()
      .trim();
    if (!sha) throw new Error("empty");
    let dirty = "";
    try {
      const porcelain = execSync("git status --porcelain", {
        stdio: ["ignore", "pipe", "ignore"],
      })
        .toString()
        .trim();
      if (porcelain) dirty = "-dirty";
    } catch {
      // status failing is fine -- leave it un-dirty (e.g. tarball
      // checkout without .git).
    }
    return `${sha}${dirty}`;
  } catch {
    return "dev-docker";
  }
}

// branchdamBuildStamp writes a one-line BUILD_ID into the emitted
// outDir (web/dist/BUILD_ID) so the Go server can read it from the
// embedded FS at startup and surface it via /api/v1/config.spaBuildId.
// `apply: "build"` excludes the dev server and vitest -- nothing to
// stamp there. `closeBundle` runs after every asset has been emitted,
// so we write on the real on-disk outDir rather than Vite's in-memory
// bundle map.
//
// The outDir comes from `configResolved(cfg)` and is resolved against
// `config.root` -- vite 8.x's resolveConfig returns a RELATIVE
// build.outDir (Hermes round-2 reproducer); `join("dist", "BUILD_ID")`
// alone only works because config.root happens to equal cwd. A future
// --root override or a config that pins root elsewhere would silently
// fail-open: BUILD_ID written outside the embedded FS, /api/v1/config
// .spaBuildId returns "". path.resolve(root, outDir) gives the
// guarantee the comment used to claim.
//
// `outDir` starts as null and is set in configResolved; closeBundle
// throws if it is still null. Hermes round-3 wanted a fail-loud
// fallback in case a future configResolved hook skips us, instead of
// silently writing to a default cwd-resolved path.
function branchdamBuildStamp(buildId: string): Plugin {
  let outDir: string | null = null;
  return {
    name: "branchdam-build-stamp",
    apply: "build",
    configResolved(config) {
      outDir = path.resolve(config.root, config.build.outDir);
    },
    closeBundle() {
      if (outDir === null) {
        throw new Error(
          "branchdam-build-stamp: configResolved did not run; cannot determine outDir",
        );
      }
      writeFileSync(join(outDir, "BUILD_ID"), `${buildId}\n`, { encoding: "utf8" });
    },
  };
}

const buildId = resolveBuildId();

export default defineConfig({
  define: {
    // JSON.stringify keeps this a literal string in the bundle; the
    // SPA reads it via `declare const __BRANCHDAM_BUILD__: string`
    // in src/vite-env.d.ts. Operators compare this against the
    // backend's `-X main.version` (exposed as Config.version) to
    // spot a stale local embed.
    __BRANCHDAM_BUILD__: JSON.stringify(buildId),
  },
  plugins: [react(), tailwindcss(), branchdamBuildStamp(buildId)],
  server: {
    // Bind on all interfaces so the dev server is reachable on a
    // remote/headless host. Override the port with VITE_PORT if 5173 is taken.
    host: true,
    port: Number(process.env.VITE_PORT ?? 5173),
    proxy: {
      // Dev: Vite serves the SPA, the Go server (make dev) serves the API.
      // Override with BRANCHDAM_API_URL when the backend runs elsewhere.
      "/api": process.env.BRANCHDAM_API_URL ?? "http://127.0.0.1:8080",
    },
  },
  build: {
    outDir: "dist",
  },
  test: {
    environment: "jsdom",
    env: { NODE_ENV: "development" },
    globals: true,
    setupFiles: ["src/test/setup.ts"],
    include: ["src/**/*.test.ts", "src/**/*.test.tsx"],
    coverage: {
      provider: "v8",
      // json-summary feeds the CI coverage gate; lcov is what Codecov
      // ingests; text prints a local summary.
      reporter: ["text", "json-summary", "lcov"],
      reportsDirectory: "./coverage",
      include: ["src/**/*.{ts,tsx}"],
      exclude: ["src/**/*.test.{ts,tsx}", "src/test/**", "src/main.tsx", "src/**/*.d.ts", "src/api/types.ts"],
    },
  },
});
