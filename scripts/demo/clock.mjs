/**
 * The demo's fixed clock (`DEMO_NOW=2026-10-15T10:00:00Z make demo`).
 *
 * Pages show timestamps the server wrote: the jobs page its start and finish times, the audit queue
 * when a link was found. The Go server takes them from the real clock (`time.Now`, and SQLite's
 * `DEFAULT (unixepoch())`), which no fake-time shim reaches, so two runs on different days (or two
 * runs of a visual check) would render different text. Instead of putting a clock into the server,
 * the demo moves what the run wrote to fixed instants once it has finished indexing, before it
 * reports ready: the data is the same on every run, and the server is untouched.
 *
 * The node UUIDs the asset detail page shows are UUIDv7, which carry the real clock and random bits, so
 * they get the same treatment (`fixDemoNodeUuids`): fixed, valid UUIDv7 values in id order. The server
 * caches a thumbnail under its node's UUID, so changing the UUIDs sends every thumbnail back to be
 * rendered again; the demo waits for that to finish, and only then fixes the timestamps, because the
 * thumbnail worker stamps `updated_at` with the real clock.
 */

/** The seconds between a job's start and its finish in the fixed data: a few, not zero. */
const JOB_SECONDS = 4;
/** The gap between one job's start and the next one's. */
const JOB_GAP_SECONDS = 30;
/** What "one run ago" is: the fixed instant every other run-time timestamp collapses to. */
const AGO_SECONDS = 3600;

/**
 * `DEMO_NOW` as a Date. A value without an offset is UTC. A bare date is refused: it would read as
 * 00:00 UTC, a day off whatever `clock` the scenes pair it with.
 */
export function parseDemoNow(value) {
  const text = String(value).trim();
  const m = /^(\d{4}-\d{2}-\d{2})[T ](\d{2}:\d{2}(?::\d{2}(?:\.\d+)?)?)(Z|[+-]\d{2}:\d{2})?$/i.exec(text);
  if (!m)
    throw new Error(
      `DEMO_NOW=${JSON.stringify(text)} is not an ISO 8601 date-time (e.g. 2026-10-15T10:00:00Z)`,
    );
  const parsed = new Date(`${m[1]}T${m[2]}${m[3] ?? "Z"}`);
  if (Number.isNaN(parsed.getTime()))
    throw new Error(`DEMO_NOW=${JSON.stringify(text)} is not a real date-time`);
  return parsed;
}

/** A valid UUIDv7 for the `index`-th node: the fixed instant, the version and variant bits, then the index. */
export function fixedNodeUuid(fixedMs, index) {
  const ms = fixedMs.toString(16).padStart(12, "0");
  const randA = (Math.floor(index / 4096) % 4096).toString(16).padStart(3, "0");
  const randB = index.toString(16).padStart(12, "0").slice(-12);
  return `${ms.slice(0, 8)}-${ms.slice(8)}-7${randA}-8000-${randB}`;
}

const quote = (name) => `"${String(name).replaceAll('"', '""')}"`;

/**
 * Moves the timestamps the run wrote in the scratch database to fixed instants.
 *
 * Every INTEGER column named `*_at` whose value is at or after `sinceSeconds` (the demo's start)
 * becomes `demoNow - 1h`, so what the synthetic media carries (its fixed EXIF times, long before the
 * run) is left alone. The scan jobs are the exception: they keep their order, each starting
 * `JOB_GAP_SECONDS` after the one before and, when finished, ending `JOB_SECONDS` later, so the
 * jobs page reads as a short history rather than as every job at one instant.
 *
 * Each table is rewritten in one statement, so a CHECK between two of its timestamps
 * (`revoked_at >= created_at`) is only judged on the result. Returns how many tables it touched.
 */
export async function fixDemoTimestamps(dbPath, demoNow, sinceSeconds) {
  // Loaded here, so a plain `make demo` (no DEMO_NOW) runs on a Node without `node:sqlite`.
  const { DatabaseSync } = await import("node:sqlite");
  const fixed = Math.floor(demoNow.getTime() / 1000) - AGO_SECONDS;
  const db = new DatabaseSync(dbPath);
  try {
    // The server has the file open and may write while this runs.
    db.exec("PRAGMA busy_timeout = 10000");
    let touched = 0;
    const tables = db
      .prepare(
        "SELECT name FROM sqlite_master WHERE type = 'table' AND name NOT LIKE 'sqlite_%' ORDER BY name",
      )
      .all();
    for (const { name: table } of tables) {
      if (table === "scan_jobs") continue;
      const columns = db
        .prepare(`PRAGMA table_info(${quote(table)})`)
        .all()
        .filter((c) => /_at$/.test(c.name) && /INT/i.test(c.type))
        .map((c) => c.name);
      if (columns.length === 0) continue;
      const sets = columns.map((c) => `${quote(c)} = CASE WHEN ${quote(c)} >= ? THEN ? ELSE ${quote(c)} END`);
      db.prepare(`UPDATE ${quote(table)} SET ${sets.join(", ")}`).run(
        ...columns.flatMap(() => [sinceSeconds, fixed]),
      );
      touched++;
    }
    const hasJobs = tables.some((t) => t.name === "scan_jobs");
    if (hasJobs) {
      const jobs = db.prepare("SELECT id, finished_at FROM scan_jobs ORDER BY id").all();
      const update = db.prepare(
        "UPDATE scan_jobs SET started_at = ?, finished_at = ?, updated_at = ? WHERE id = ?",
      );
      jobs.forEach((job, i) => {
        const started = fixed + i * JOB_GAP_SECONDS;
        const finished = job.finished_at === null ? null : started + JOB_SECONDS;
        update.run(started, finished, finished ?? started, job.id);
      });
      touched++;
    }
    return touched;
  } finally {
    db.close();
  }
}

/**
 * Gives every node a fixed UUIDv7, in id order, and returns how many it changed. The thumbnails are cached
 * under the old UUIDs, so they are set back to PENDING for the server's worker to render again.
 */
export async function fixDemoNodeUuids(dbPath, demoNow) {
  const { DatabaseSync } = await import("node:sqlite");
  const fixedMs = (Math.floor(demoNow.getTime() / 1000) - AGO_SECONDS) * 1000;
  const db = new DatabaseSync(dbPath);
  try {
    db.exec("PRAGMA busy_timeout = 10000");
    const nodes = db.prepare("SELECT id FROM media_nodes ORDER BY id").all();
    const update = db.prepare("UPDATE media_nodes SET node_uuid = ? WHERE id = ?");
    nodes.forEach((node, i) => update.run(fixedNodeUuid(fixedMs, i), node.id));
    db.exec("UPDATE media_nodes SET thumb_state = 'PENDING', thumb_attempts = 0 WHERE thumb_state = 'READY'");
    return nodes.length;
  } finally {
    db.close();
  }
}
