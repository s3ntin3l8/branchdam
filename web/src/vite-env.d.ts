/// <reference types="vite/client" />

// Inlined at build time by vite.config.ts's `define` (resolveBuildId):
// BRANCHDAM_BUILD_ID (release tag) || COMMIT_SHA (commit docker-publish
// checked out) || a host git sha || "dev-docker". Operators compare it
// with the backend's `-X main.version` (Config.version) on the Settings
// page to spot a stale embed.
declare const __BRANCHDAM_BUILD__: string;
