// The demo's fixed clock. Run: make test-demo
import assert from "node:assert/strict";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { DatabaseSync } from "node:sqlite";
import { after, describe, it } from "node:test";
import { fixDemoNodeUuids, fixDemoTimestamps, fixedNodeUuid, parseDemoNow } from "./clock.mjs";

const dir = mkdtempSync(path.join(tmpdir(), "branchdam-clock-test-"));
after(() => rmSync(dir, { recursive: true, force: true }));

describe("parseDemoNow", () => {
  it("reads an ISO date-time, UTC unless it says otherwise", () => {
    const expected = Date.UTC(2026, 9, 15, 10, 0, 0);
    assert.equal(parseDemoNow("2026-10-15T10:00:00Z").getTime(), expected);
    assert.equal(parseDemoNow("2026-10-15T10:00:00").getTime(), expected);
    assert.equal(parseDemoNow("2026-10-15 10:00").getTime(), expected);
    assert.equal(parseDemoNow("2026-10-15T12:00:00+02:00").getTime(), expected);
  });

  it("refuses what is not a date-time", () => {
    // A bare date would read as 00:00 UTC, a day off whatever browser clock the scenes pair it with.
    for (const bad of ["2026-10-15", "20261015T100000", "yesterday", "", "10:00", "2026-13-45T10:00:00Z"])
      assert.throws(() => parseDemoNow(bad), /DEMO_NOW/, bad);
  });
});

describe("fixDemoTimestamps", () => {
  const NOW = parseDemoNow("2026-10-15T10:00:00Z");
  const FIXED = NOW.getTime() / 1000 - 3600;
  const START = 1_900_000_000; // the demo's start; the "recent" values below are after it

  function database(name) {
    const file = path.join(dir, name);
    const db = new DatabaseSync(file);
    db.exec(`
      CREATE TABLE media_nodes (id INTEGER PRIMARY KEY, node_uuid TEXT NOT NULL UNIQUE, first_seen_at INTEGER NOT NULL,
                                created_at INTEGER NOT NULL, captured_at INTEGER, file_name TEXT,
                                thumb_state TEXT NOT NULL DEFAULT 'READY', thumb_attempts INTEGER NOT NULL DEFAULT 0);
      CREATE TABLE users (id INTEGER PRIMARY KEY, created_at INTEGER NOT NULL, disabled_at INTEGER,
                          CHECK (disabled_at IS NULL OR disabled_at >= created_at));
      CREATE TABLE scan_jobs (id INTEGER PRIMARY KEY, started_at INTEGER NOT NULL, finished_at INTEGER,
                              updated_at INTEGER NOT NULL, files_seen INTEGER);
      CREATE TABLE node_creators (node_uuid TEXT PRIMARY KEY REFERENCES media_nodes(node_uuid), agent_id TEXT);
      CREATE TABLE node_uuid_aliases (alias_uuid TEXT PRIMARY KEY,
                                      node_uuid TEXT NOT NULL REFERENCES media_nodes(node_uuid),
                                      CHECK (alias_uuid <> node_uuid));
      CREATE TABLE plain (id INTEGER PRIMARY KEY, name TEXT);
    `);
    return { file, db };
  }

  it("moves what the run wrote and leaves the synthetic media's own times alone", async () => {
    const { file, db } = database("nodes.db");
    db.exec(
      `INSERT INTO media_nodes (id, node_uuid, first_seen_at, created_at, captured_at, file_name) VALUES (1, 'random-1', ${START + 5}, ${START + 7}, 1700000000, 'a.arw')`,
    );
    db.close();

    await fixDemoTimestamps(file, NOW, START);

    const check = new DatabaseSync(file);
    const row = check.prepare("SELECT * FROM media_nodes").get();
    check.close();
    assert.equal(row.first_seen_at, FIXED);
    assert.equal(row.created_at, FIXED);
    assert.equal(row.captured_at, 1700000000, "an old time (the media's EXIF) is not the run's");
    assert.equal(row.file_name, "a.arw");
  });

  it("gives every node a fixed UUIDv7 in id order, whatever it was generated as", async () => {
    const run = async (name, uuids) => {
      const { file, db } = database(name);
      uuids.forEach((u, i) =>
        db.exec(
          `INSERT INTO media_nodes (id, node_uuid, first_seen_at, created_at, captured_at, file_name) VALUES (${i + 1}, '${u}', ${START}, ${START}, NULL, 'f')`,
        ),
      );
      db.close();
      await fixDemoNodeUuids(file, NOW);
      const check = new DatabaseSync(file);
      const rows = check
        .prepare("SELECT node_uuid FROM media_nodes ORDER BY id")
        .all()
        .map((r) => r.node_uuid);
      check.close();
      return rows;
    };
    const first = await run("uuid-a.db", [
      "01a1bd9d-6f16-739b-87ef-c5181dfc0747",
      "01a1bda0-2088-7ab9-acd8-a3d529e79192",
    ]);
    const second = await run("uuid-b.db", [
      "01a1bf00-1111-7000-8000-000000000001",
      "01a1bf01-2222-7fff-bfff-ffffffffffff",
    ]);
    assert.deepEqual(first, second);
    assert.equal(new Set(first).size, 2);
    for (const uuid of first)
      assert.match(uuid, /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
  });

  it("sends the thumbnails, cached under the old UUIDs, back to be rendered again", async () => {
    const { file, db } = database("thumbs.db");
    db.exec(`INSERT INTO media_nodes (id, node_uuid, first_seen_at, created_at, thumb_state, thumb_attempts) VALUES
      (1, 'a', ${START}, ${START}, 'READY', 1), (2, 'b', ${START}, ${START}, 'UNSUPPORTED', 3)`);
    db.close();

    assert.equal(await fixDemoNodeUuids(file, NOW), 2);

    const check = new DatabaseSync(file);
    const rows = check.prepare("SELECT thumb_state, thumb_attempts FROM media_nodes ORDER BY id").all();
    check.close();
    assert.deepEqual(
      rows.map((r) => [r.thumb_state, r.thumb_attempts]),
      [
        ["PENDING", 0],
        ["UNSUPPORTED", 3],
      ],
      "only a READY thumbnail has a cached file to lose",
    );
  });

  it("moves the rows that reference a node's UUID along with it", async () => {
    const { file, db } = database("children.db");
    db.exec(`INSERT INTO media_nodes (id, node_uuid, first_seen_at, created_at) VALUES (1, 'old-a', ${START}, ${START}), (2, 'old-b', ${START}, ${START});
      INSERT INTO node_creators VALUES ('old-b', 'agent-1');
      INSERT INTO node_uuid_aliases VALUES ('dup-of-a', 'old-a')`);
    db.close();

    await fixDemoNodeUuids(file, NOW);

    const check = new DatabaseSync(file);
    const uuid = check.prepare("SELECT node_uuid FROM media_nodes WHERE id = 2").get().node_uuid;
    const creator = check.prepare("SELECT * FROM node_creators").get();
    const alias = check.prepare("SELECT * FROM node_uuid_aliases").get();
    const first = check.prepare("SELECT node_uuid FROM media_nodes WHERE id = 1").get().node_uuid;
    check.close();
    assert.equal(creator.node_uuid, uuid);
    assert.equal(alias.node_uuid, first);
    assert.equal(
      alias.alias_uuid,
      "dup-of-a",
      "the alias itself is another node's name for it, not ours to change",
    );
  });

  it("leaves every UUID as it was when a reference cannot follow", async () => {
    const { file, db } = database("rollback.db");
    // An alias already named like the node's fixed UUID: once the node takes it, the alias's CHECK refuses.
    const taken = fixedNodeUuid((NOW.getTime() / 1000 - 3600) * 1000, 0);
    db.exec(`INSERT INTO media_nodes (id, node_uuid, first_seen_at, created_at) VALUES (1, 'old-a', ${START}, ${START});
      INSERT INTO node_uuid_aliases VALUES ('${taken}', 'old-a')`);
    db.close();

    await assert.rejects(fixDemoNodeUuids(file, NOW));

    const check = new DatabaseSync(file);
    const uuid = check.prepare("SELECT node_uuid FROM media_nodes").get().node_uuid;
    check.close();
    assert.equal(uuid, "old-a");
  });

  it("builds distinct UUIDv7s from the fixed instant", () => {
    const ms = Date.UTC(2026, 9, 15, 9, 0, 0);
    assert.notEqual(fixedNodeUuid(ms, 0), fixedNodeUuid(ms, 1));
    assert.notEqual(fixedNodeUuid(ms, 4095), fixedNodeUuid(ms, 4096));
    assert.match(fixedNodeUuid(ms, 70000), /^[0-9a-f]{8}-[0-9a-f]{4}-7[0-9a-f]{3}-8000-[0-9a-f]{12}$/);
  });

  it("rewrites a table in one statement, so a CHECK between two of its times holds", async () => {
    const { file, db } = database("users.db");
    db.exec(`INSERT INTO users VALUES (1, ${START + 1}, ${START + 9})`);
    db.close();

    await fixDemoTimestamps(file, NOW, START);

    const check = new DatabaseSync(file);
    const row = check.prepare("SELECT * FROM users").get();
    check.close();
    assert.equal(row.created_at, FIXED);
    assert.equal(row.disabled_at, FIXED);
  });

  it("keeps the scan jobs in order, a few seconds long, and a running job unfinished", async () => {
    const { file, db } = database("jobs.db");
    db.exec(`INSERT INTO scan_jobs VALUES
      (1, ${START + 3}, ${START + 40}, ${START + 40}, 10),
      (2, ${START + 41}, ${START + 90}, ${START + 90}, 20),
      (3, ${START + 91}, NULL, ${START + 95}, 30)`);
    db.close();

    await fixDemoTimestamps(file, NOW, START);

    const check = new DatabaseSync(file);
    const jobs = check.prepare("SELECT * FROM scan_jobs ORDER BY id").all();
    check.close();
    assert.deepEqual(
      jobs.map((j) => [j.started_at, j.finished_at, j.updated_at]),
      [
        [FIXED, FIXED + 4, FIXED + 4],
        [FIXED + 30, FIXED + 34, FIXED + 34],
        [FIXED + 60, null, FIXED + 60],
      ],
    );
    assert.deepEqual(
      jobs.map((j) => j.files_seen),
      [10, 20, 30],
    );
  });

  it("gives the same data whatever the run's own timings were", async () => {
    const run = async (name, offsets) => {
      const { file, db } = database(name);
      offsets.forEach((o, i) =>
        db.exec(
          `INSERT INTO scan_jobs VALUES (${i + 1}, ${START + o}, ${START + o + 2}, ${START + o + 2}, 1)`,
        ),
      );
      db.close();
      await fixDemoTimestamps(file, NOW, START);
      const check = new DatabaseSync(file);
      const rows = check
        .prepare("SELECT started_at, finished_at, updated_at FROM scan_jobs ORDER BY id")
        .all();
      check.close();
      return JSON.stringify(rows);
    };
    assert.equal(await run("fast.db", [1, 2, 3]), await run("slow.db", [4, 90, 400]));
  });

  it("copes with tables that have no timestamps and with an empty database", async () => {
    const { file, db } = database("empty.db");
    db.close();
    await assert.doesNotReject(fixDemoTimestamps(file, NOW, START));
  });
});
