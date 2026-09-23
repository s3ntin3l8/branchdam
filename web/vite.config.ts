/// <reference types="vitest/config" />
import { execSync } from "node:child_process";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { defineConfig, type Plugin } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

// resolveBuildId returns a short, human-readable identifier for this
// SPA build. CI / Docker set BRANCHDAM_BUILD_ID (matching the same env
// the backend Dockerfile uses for `-X main.version`); locally we fall
// back to `git rev-parse --short HEAD` with a `-dirty` marker so a
// developer running `npm run build` sees something more useful than
// "dev" -- and can spot uncommitted SPA changes from the Settings
// page. Final fallback: `dev-<UTC date>`, e.g. for tarball checkouts
// without .git.
function resolveBuildId(): string {
  const fromEnv = process.env.BRANCHDAM_BUILD_ID?.trim();
  if (fromEnv) return fromEnv;
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
    return `dev-${new Date().toISOString().slice(0, 10)}`;
  }
}

// branchdamBuildStamp writes a one-line BUILD_ID into the emitted
// outDir (web/dist/BUILD_ID) so the Go server can read it from the
// embedded FS at startup and surface it via /api/v1/config.spaBuildId.
// `apply: "build"` excludes the dev server and vitest -- nothing to
// stamp there. `closeBundle` runs after every asset has been emitted,
// so we write on the real on-disk outDir rather than Vite's in-memory
// bundle map.
function branchdamBuildStamp(buildId: string): Plugin {
  return {
    name: "branchdam-build-stamp",
    apply: "build",
    closeBundle() {
      const target = join(process.cwd(), "dist", "BUILD_ID");
      writeFileSync(target, `${buildId}\n`, { encoding: "utf8" });
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
