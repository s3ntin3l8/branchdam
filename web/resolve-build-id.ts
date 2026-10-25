import { execSync } from "node:child_process";

// gitShortHead is the host-checkout id: short SHA, plus `-dirty` when the
// worktree has uncommitted changes. Null when git isn't there (the image
// build — .git is dockerignored — or a tarball). Settings strips the
// suffix before comparing, so a dirty `make dev-web` is not a stale embed.
function gitShortHead(): string | null {
  try {
    const sha = execSync("git rev-parse --short HEAD", {
      stdio: ["ignore", "pipe", "ignore"],
    })
      .toString()
      .trim();
    if (!sha) return null;
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
    return null;
  }
}

// resolveBuildId is stamped into __BRANCHDAM_BUILD__ and web/dist/BUILD_ID.
//
//   BRANCHDAM_BUILD_ID  explicit override. release.yml passes the tag.
//   git short SHA       the checkout, when .git is present. Consulted
//                       before COMMIT_SHA so a stray exported COMMIT_SHA
//                       on a host cannot outrank `git rev-parse`.
//   COMMIT_SHA          injected by docker-publish from `git rev-parse HEAD`
//                       of the checkout. This is the trustworthy value only
//                       once .git is gone (the image). Not `${{ github.sha }}`.
//   dev-docker          none of the above. A local `docker build` lands here.
export function resolveBuildId(
  env: NodeJS.ProcessEnv = process.env,
  gitHead: () => string | null = gitShortHead,
): string {
  const override = env.BRANCHDAM_BUILD_ID?.trim();
  if (override) return override;
  const fromGit = gitHead();
  if (fromGit) return fromGit;
  const commit = env.COMMIT_SHA?.trim();
  if (commit) return commit;
  return "dev-docker";
}
