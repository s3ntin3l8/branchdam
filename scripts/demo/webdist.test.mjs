// Stale-SPA verdict for the demo's web/dist. Run: make test-demo
import assert from "node:assert/strict";
import { mkdirSync, mkdtempSync, rmSync, utimesSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { afterEach, beforeEach, describe, it } from "node:test";
import { STUB_MARKER, webDistStaleReason } from "./webdist.mjs";

const OLD = new Date("2026-01-01T00:00:00Z");
const BUILT = new Date("2026-02-01T00:00:00Z");
const NEW = new Date("2026-03-01T00:00:00Z");

let web;

function write(rel, content = "x", time = OLD) {
  const p = path.join(web, rel);
  mkdirSync(path.dirname(p), { recursive: true });
  writeFileSync(p, content);
  utimesSync(p, time, time);
}

/** A current real build: sources older than dist/index.html. */
function realBuild() {
  write("src/main.tsx");
  write("src/pages/Graph.tsx");
  write("index.html");
  write("package.json");
  write("package-lock.json");
  write("vite.config.ts");
  write("dist/assets/index.js");
  write("dist/index.html", '<!doctype html><html lang="en"><head>', BUILT);
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

  for (const name of ["index.html", "package.json", "package-lock.json", "vite.config.ts"])
    it(`rebuilds when web/${name} changed after the build`, () => {
      realBuild();
      write(name, "changed", NEW);
      assert.equal(webDistStaleReason(web), `web/${name} is newer than web/dist`);
    });
});
