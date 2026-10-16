// Isolation guard for the demo instance. Run: make test-demo
import assert from "node:assert/strict";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import net from "node:net";
import { tmpdir } from "node:os";
import path from "node:path";
import { after, describe, it } from "node:test";
import { assertDemo, assertPortsFree, demoConfigProblems, demoEnv, demoEnvProblems } from "./env.mjs";
import { spawn } from "node:child_process";
import { assetsOf, hasExited, renderConfig, retryOnConflict, stopChild, waitFor } from "./instance.mjs";

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
    "a plural key list": { IMMICH_API_KEYS: "k1,k2" }, // pragma: allowlist secret
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
    "a privileged port": (c) => (c.listenAddr = "127.0.0.1:22"),
    "a non-numeric port": (c) => (c.listenAddr = "127.0.0.1:x"),
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

describe("retryOnConflict", () => {
  const httpError = (status) => Object.assign(new Error(`-> ${status}`), { status });

  it("retries while the scan is still running (409)", async () => {
    let calls = 0;
    const result = await retryOnConflict(
      async () => {
        if (++calls < 3) throw httpError(409);
        return "started";
      },
      { delayMs: 1 },
    );
    assert.equal(result, "started");
    assert.equal(calls, 3);
  });

  it("gives up after the last attempt", async () => {
    await assert.rejects(
      retryOnConflict(async () => Promise.reject(httpError(409)), { attempts: 2, delayMs: 1 }),
      /409/,
    );
  });

  it("does not retry other errors", async () => {
    let calls = 0;
    await assert.rejects(
      retryOnConflict(async () => {
        calls++;
        throw httpError(500);
      }),
      /500/,
    );
    assert.equal(calls, 1);
  });
});

describe("renderConfig", () => {
  it("quotes every path, so YAML specials in it can't change the value", () => {
    const tricky = inDemo("my: root #1");
    const cfg = { ...safeConfig(), storageLocations: [{ name: "a", rootPath: tricky, tier: "PROJECTS", readOnly: false }] };
    const out = renderConfig(cfg);
    assert.ok(out.includes(`rootPath: ${JSON.stringify(tricky)}`));
    assert.ok(out.includes(`path: ${JSON.stringify(cfg.database.path)}`));
    assert.ok(!out.includes(`: ${tricky}`), "no unquoted path");
  });
});

describe("assetsOf", () => {
  it("accepts every response shape the API has used", () => {
    const list = [{ id: 1 }];
    assert.deepEqual(assetsOf({ assets: list }), list);
    assert.deepEqual(assetsOf({ items: list }), list);
    assert.deepEqual(assetsOf(list), list);
    assert.deepEqual(assetsOf({}), []);
  });
});

describe("stopChild", () => {
  const exitOf = (child) => new Promise((r) => child.once("exit", r));

  it("stops a running child", async () => {
    const child = spawn("sleep", ["30"]);
    await new Promise((r) => child.once("spawn", r));
    await stopChild(child);
    assert.ok(hasExited(child));
  });

  it("returns at once for a child killed by a signal (exitCode stays null)", async () => {
    const child = spawn("sleep", ["30"]);
    const exited = exitOf(child);
    child.kill("SIGKILL");
    await exited;
    assert.equal(child.exitCode, null);
    assert.ok(hasExited(child));
    await stopChild(child); // would hang on a second "exit" listener before
  });

  it("returns at once for a child that never started", async () => {
    const child = spawn(path.join(demoDir, "no-such-binary"));
    const error = await new Promise((r) => child.once("error", r));
    assert.equal(error.code, "ENOENT");
    await stopChild(child);
  });
});

describe("waitFor", () => {
  it("fails fast when the fail callback reports a fatal condition", async () => {
    const started = Date.now();
    await assert.rejects(
      waitFor(async () => false, "never", 60_000, { fail: () => new Error("the server exited (SIGKILL)") }),
      /server exited/,
    );
    assert.ok(Date.now() - started < 1_000);
  });
});
