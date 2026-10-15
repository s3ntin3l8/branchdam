// Isolation guard for the demo instance. Run: make test-demo
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import net from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { after, describe, it } from "node:test";
import { assertDemo, assertPortsFree, demoConfigProblems, demoEnv, demoEnvProblems } from "./env.mjs";

const demoDir = mkdtempSync(path.join(tmpdir(), "branchdam-demo-test-"));
after(() => rmSync(demoDir, { recursive: true, force: true }));

const inDemo = (...p) => path.join(demoDir, ...p);
const safeConfig = () => ({
  listenAddr: "127.0.0.1:3975",
  database: { path: inDemo("data", "branchdam.db") },
  thumbnails: { cacheDir: inDemo("data", "thumbs") },
  auth: { mode: "forward" },
  immich: { apiUrl: "", exportPath: inDemo("exports", "immich") },
  metadata: { autoInherit: false },
  storageLocations: [{ name: "archive", rootPath: inDemo("archive") }],
});
const safeEnv = () =>
  demoEnv(demoDir, { BRANCHDAM_CONFIG: inDemo("data", "config.yaml") }, { PATH: "/usr/bin" });

describe("demoEnv", () => {
  it("inherits only the allowlist", () => {
    const env = demoEnv(
      demoDir,
      { BRANCHDAM_CONFIG: inDemo("data", "config.yaml") },
      {
        PATH: "/usr/bin",
        BRANCHDAM_SECRET_KEY: "real", // pragma: allowlist secret
        ADMIN_BOOTSTRAP_PAT: "bdam_pat_real", // pragma: allowlist secret
        BRANCHDAM_CONFIG: "/home/me/branchDAM/config.yaml",
        MULLION_SOCKET_PATH: "/x",
      },
    );
    assert.equal(env.PATH, "/usr/bin");
    assert.equal(env.BRANCHDAM_SECRET_KEY, undefined);
    assert.equal(env.ADMIN_BOOTSTRAP_PAT, undefined);
    assert.equal(env.MULLION_SOCKET_PATH, undefined);
    assert.ok(env.BRANCHDAM_CONFIG.startsWith(demoDir));
    assert.deepEqual(demoEnvProblems(env, demoDir), []);
    assert.deepEqual(demoConfigProblems(safeConfig(), demoDir), []);
  });
});

describe("refuses a poisoned environment", () => {
  for (const [name, vars] of Object.entries({
    "the checkout's config": { BRANCHDAM_CONFIG: "/home/me/branchDAM/config.yaml" },
    "a secret key": { BRANCHDAM_SECRET_KEY: "k" }, // pragma: allowlist secret
    "a bootstrap token": { ADMIN_BOOTSTRAP_PAT: "bdam_pat_x" }, // pragma: allowlist secret
    "Mullion session variables": { MULLION_HOOK_TOKEN: "t" }, // pragma: allowlist secret
    "HOME outside": { HOME: "/home/me" },
  }))
    it(name, () => assert.ok(demoEnvProblems({ ...safeEnv(), ...vars }, demoDir).length > 0));
});

describe("refuses an unsafe config", () => {
  const cases = {
    "the developer database": (c) => (c.database.path = "/home/me/branchDAM/data/branchdam.db"),
    "a storage root outside": (c) => c.storageLocations.push({ name: "real", rootPath: "/mnt/photos" }),
    "an Immich URL": (c) => (c.immich.apiUrl = "https://photos.example.com"),
    "metadata write-back": (c) => (c.metadata.autoInherit = true),
    "binding all interfaces": (c) => (c.listenAddr = ":3975"),
    "the default port": (c) => (c.listenAddr = "127.0.0.1:8080"),
    "local auth": (c) => (c.auth.mode = "local"),
  };
  for (const [name, poison] of Object.entries(cases))
    it(name, () => {
      const cfg = safeConfig();
      poison(cfg);
      assert.ok(demoConfigProblems(cfg, demoDir).length > 0);
      assert.throws(() => assertDemo(safeEnv(), cfg, demoDir), /refused/);
    });

  it("a working directory holding a config.yaml", () => {
    const cwd = mkdtempSync(path.join(demoDir, "cwd-"));
    writeFileSync(path.join(cwd, "config.yaml"), "listenAddr: \":8080\"\n");
    assert.throws(() => assertDemo(safeEnv(), safeConfig(), demoDir, { cwd }), /config\.yaml/);
  });
});

describe("assertPortsFree", () => {
  it("rejects a port that is already listening", async () => {
    const server = net.createServer().listen(0, "127.0.0.1");
    await new Promise((r) => server.once("listening", r));
    await assert.rejects(assertPortsFree([server.address().port]), /already in use/);
    server.close();
  });
});
