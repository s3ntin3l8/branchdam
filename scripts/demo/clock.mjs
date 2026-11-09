/**
 * The demo's fixed clock (`DEMO_NOW=2026-10-15T10:00:00Z make demo`).
 *
 * Pages show timestamps the server wrote: the jobs page its start and finish times, the audit queue
 * when a link was found, the asset page its node UUIDs (UUIDv7: a clock reading plus random bits). The
 * server takes all of them from one clock (internal/clock), which the demo freezes through the config
 * key `clock.fixed`, so two runs on different days (or two runs of a visual check) render the same
 * text. The server refuses that key unless BRANCHDAM_DEMO=1, which only the demo sets.
 *
 * The server's clock is `DEMO_NOW` minus an hour, so the data the demo shows is "1h ago" next to the
 * browser clock the scenes pair with `DEMO_NOW` (visual/scenes.json `clock`).
 */

/** What "one run ago" is: how far before DEMO_NOW the server's frozen clock stands. */
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

/** The instant the server's clock is frozen at (`clock.fixed`, RFC 3339, whole seconds) for a `DEMO_NOW`. */
export function serverInstant(demoNow) {
  const seconds = Math.floor(demoNow.getTime() / 1000) - AGO_SECONDS;
  return new Date(seconds * 1000).toISOString().replace(".000Z", "Z");
}
