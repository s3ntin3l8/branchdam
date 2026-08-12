/// <reference types="vitest/config" />
import { defineConfig } from "vite";
import react from "@vitejs/plugin-react";
import tailwindcss from "@tailwindcss/vite";

export default defineConfig({
  plugins: [react(), tailwindcss()],
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
