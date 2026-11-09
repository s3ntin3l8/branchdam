// The demo's fixed clock. Run: make test-demo
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { parseDemoNow, serverInstant } from "./clock.mjs";

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

describe("serverInstant", () => {
  it("is an hour before DEMO_NOW, as an RFC 3339 UTC instant the server accepts", () => {
    assert.equal(serverInstant(parseDemoNow("2026-10-15T10:00:00Z")), "2026-10-15T09:00:00Z");
    assert.equal(serverInstant(parseDemoNow("2026-10-15T12:00:30.700+02:00")), "2026-10-15T09:00:30Z");
    assert.match(serverInstant(parseDemoNow("2026-01-01T00:30:00Z")), /^2025-12-31T23:30:00Z$/);
  });
});
