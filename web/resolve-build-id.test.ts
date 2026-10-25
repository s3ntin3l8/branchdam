import { describe, expect, it } from "vitest";
import { resolveBuildId } from "./resolve-build-id.ts";

const checkout = () => "abc1234-dirty";
const noGit = () => null;

describe("resolveBuildId", () => {
  it.each([
    {
      name: "release tag overrides COMMIT_SHA and the checkout",
      env: { BRANCHDAM_BUILD_ID: "v1.2.3", COMMIT_SHA: "fullsha" },
      gitHead: checkout,
      want: "v1.2.3",
    },
    {
      name: "a checkout outranks a stray COMMIT_SHA",
      env: { COMMIT_SHA: "not-the-checkout" },
      gitHead: checkout,
      want: "abc1234-dirty",
    },
    {
      name: "COMMIT_SHA is used only when git is unavailable",
      env: { BRANCHDAM_BUILD_ID: "  ", COMMIT_SHA: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb" },
      gitHead: noGit,
      want: "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
    },
    {
      name: "neither env nor git falls back to dev-docker",
      env: { BRANCHDAM_BUILD_ID: "", COMMIT_SHA: "   " },
      gitHead: noGit,
      want: "dev-docker",
    },
  ])("$name", ({ env, gitHead, want }) => {
    expect(resolveBuildId(env, gitHead)).toBe(want);
  });
});
