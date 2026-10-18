// Stale-SPA verdict for the demo's web/dist. Run: make test-demo
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import {
  mkdirSync,
  mkdtempSync,
  readFileSync,
  rmSync,
  symlinkSync,
  utimesSync,
  writeFileSync,
} from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { afterEach, beforeEach, describe, it } from "node:test";
import { STUB_MARKER, STUB_MAX_BYTES, webDistStaleReason } from "./webdist.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const PREBUILD = path.join(ROOT, ".github/ci-prebuild.sh");

const OLD = new Date("2026-01-01T00:00:00Z");
const BUILT = new Date("2026-02-01T00:00:00Z");
const NEW = new Date("2026-03-01T00:00:00Z");

let web;

/** Sets a path's atime and mtime. */
function touch(rel, time) {
  utimesSync(path.join(web, rel), time, time);
}

function write(rel, content = "x", time = OLD) {
  const p = path.join(web, rel);
  mkdirSync(path.dirname(p), { recursive: true });
  writeFileSync(p, content);
  touch(rel, time);
}

/** A current real build: sources older than dist/index.html. */
function realBuild() {
  write("src/main.tsx");
  write("src/pages/Graph.tsx");
  write("index.html");
  write("package.json");
  write("package-lock.json");
  write("vite.config.ts");
  write("public/favicon.svg");
  write("dist/assets/index.js");
  // Over ci-prebuild's 200-byte stub threshold, like a real Vite index.html.
  write("dist/index.html", `<!doctype html><html lang="en"><head>${" ".repeat(400)}`, BUILT);
  // Creating the files bumped their directories; date those before the build too.
  for (const dir of ["src/pages", "src", "public", "dist"]) touch(dir, OLD);
  touch(".", OLD);
}

beforeEach(() => {
  web = mkdtempSync(path.join(tmpdir(), "branchdam-webdist-test-"));
});
afterEach(() => rmSync(web, { recursive: true, force: true }));

describe("webDistStaleReason", () => {
  it("accepts a build newer than every input", () => {
    realBuild();
    assert.equal(webDistStaleReason(web), null);
  });

  it("rebuilds a missing dist", () => {
    write("src/main.tsx");
    assert.equal(webDistStaleReason(web), "web/dist is missing");
  });

  it("rebuilds a dist without assets", () => {
    realBuild();
    rmSync(path.join(web, "dist/assets"), { recursive: true });
    assert.equal(webDistStaleReason(web), "web/dist is missing");
  });

  it("rebuilds the ci-prebuild stub even when it is newer than the sources", () => {
    realBuild();
    write("dist/index.html", `${STUB_MARKER}<p>stub</p>`, NEW);
    assert.equal(webDistStaleReason(web), "web/dist is the build stub");
  });

  it("treats any tiny index.html as the stub, like ci-prebuild's is_stub()", () => {
    realBuild();
    write("dist/index.html", "<!doctype html><p>x</p>", NEW);
    assert.equal(webDistStaleReason(web), "web/dist is the build stub");
  });

  it("keeps STUB_MARKER in step with .github/ci-prebuild.sh", () => {
    const script = readFileSync(PREBUILD, "utf8");
    assert.ok(script.includes(`STUB_MARKER='${STUB_MARKER}'`));
  });

  it("keeps STUB_MAX_BYTES in step with is_stub()'s size threshold", () => {
    const script = readFileSync(PREBUILD, "utf8");
    const m = script.match(/\[ "\$size" -lt (\d+) \]/);
    assert.ok(m, "is_stub() size test not found in ci-prebuild.sh");
    assert.equal(Number(m[1]), STUB_MAX_BYTES);
    realBuild();
    write("dist/index.html", `<!doctype html>${" ".repeat(STUB_MAX_BYTES - 16)}`, NEW);
    assert.equal(webDistStaleReason(web), "web/dist is the build stub");
    write("dist/index.html", `<!doctype html>${" ".repeat(STUB_MAX_BYTES - 15)}`, BUILT);
    assert.equal(webDistStaleReason(web), null);
  });

  it("classifies the placeholder ci-prebuild.sh actually writes as the stub", () => {
    // A throwaway repo root outside any git checkout: with no index.html the
    // script writes its placeholder there and exits.
    const repo = mkdtempSync(path.join(tmpdir(), "branchdam-prebuild-test-"));
    try {
      mkdirSync(path.join(repo, "web/dist/assets"), { recursive: true });
      execFileSync("bash", [PREBUILD], {
        cwd: repo,
        env: { PATH: process.env.PATH, GIT_CEILING_DIRECTORIES: path.dirname(repo) },
      });
      const html = readFileSync(path.join(repo, "web/dist/index.html"), "utf8");
      assert.ok(html.length < STUB_MAX_BYTES);
      assert.equal(webDistStaleReason(path.join(repo, "web")), "web/dist is the build stub");
    } finally {
      rmSync(repo, { recursive: true, force: true });
    }
  });

  it("honours web-ensure's stale marker", () => {
    realBuild();
    write("dist/.branchdam-stale", "", NEW);
    assert.equal(webDistStaleReason(web), "web/dist is marked stale");
  });

  it("rebuilds when a nested source file changed after the build", () => {
    realBuild();
    write("src/pages/Graph.tsx", "changed", NEW);
    assert.equal(webDistStaleReason(web), "web/src is newer than web/dist");
  });

  it("rebuilds when a source file was deleted after the build", () => {
    realBuild();
    rmSync(path.join(web, "src/pages/Graph.tsx"));
    touch("src/pages", NEW);
    assert.equal(webDistStaleReason(web), "web/src is newer than web/dist");
  });

  it("rebuilds when a web/public asset changed after the build", () => {
    realBuild();
    write("public/favicon.svg", "changed", NEW);
    assert.equal(webDistStaleReason(web), "web/public is newer than web/dist");
  });

  it("rebuilds when a whole web/ entry was removed after the build", () => {
    realBuild();
    rmSync(path.join(web, "public"), { recursive: true });
    touch(".", NEW);
    assert.equal(webDistStaleReason(web), "web/ changed after web/dist was built");
  });

  it("rebuilds instead of crashing on a dangling web/.env symlink", () => {
    realBuild();
    symlinkSync(path.join(web, "no-such-target"), path.join(web, ".env"));
    touch(".", OLD);
    assert.equal(webDistStaleReason(web), "web/.env is a dangling link or was removed");
  });

  it("ignores tsc's tsbuildinfo output", () => {
    realBuild();
    write("tsconfig.app.tsbuildinfo", "{}", NEW);
    touch(".", OLD);
    assert.equal(webDistStaleReason(web), null);
  });

  for (const name of [
    "index.html",
    "package.json",
    "package-lock.json",
    "vite.config.ts",
    "tsconfig.json",
    "tsconfig.app.json",
    ".env",
    ".env.production",
  ])
    it(`rebuilds when web/${name} changed after the build`, () => {
      realBuild();
      write(name, "changed", NEW);
      assert.equal(webDistStaleReason(web), `web/${name} is newer than web/dist`);
    });
});
