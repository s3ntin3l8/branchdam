/// <reference types="vite/client" />

// Inlined at build time by vite.config.ts's `define` (resolveBuildId in
// resolve-build-id.ts): BRANCHDAM_BUILD_ID (release tag), else the
// checkout's git sha when .git is present, else COMMIT_SHA (the commit
// docker-publish checked out — used in the image, where .git is absent),
// else "dev-docker". Operators compare it with the backend's
// `-X main.version` (Config.version) on the Settings page to spot a
// stale embed.
declare const __BRANCHDAM_BUILD__: string;
