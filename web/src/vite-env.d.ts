/// <reference types="vite/client" />

// Inlined at build time by vite.config.ts's `define`. Matches the
// BRANCHDAM_BUILD_ID env var the backend Dockerfile uses for
// `-X main.version` -- operators compare the two from the Settings
// page to spot a stale local embed of an old SPA into a new binary.
declare const __BRANCHDAM_BUILD__: string;
