import { describe, expect, it } from "vitest";
import { edgeVisual, labelAnchor, pointAlong, routePath } from "./edgeStyle";

describe("edgeVisual", () => {
  const base = { relationshipType: "PROXY_OF" as const, confidence: 0.5 };

  it("leaves accepted edges solid", () => {
    for (const reviewState of ["CONFIRMED", "AUTO_ACCEPTED"] as const) {
      const v = edgeVisual({ ...base, reviewState });
      expect(v.style.strokeDasharray).toBeUndefined();
      expect(v.label).toBe("PROXY_OF (0.50)");
    }
  });

  it("dashes and labels an unreviewed edge", () => {
    const v = edgeVisual({ ...base, reviewState: "NEEDS_REVIEW" }, 2);
    expect(v.style.strokeDasharray).toBe("6 4");
    expect(v.style.strokeWidth).toBe(2);
    expect(v.label).toBe("PROXY_OF (0.50) - needs review");
  });

  it("fades a rejected edge", () => {
    const v = edgeVisual({ ...base, reviewState: "REJECTED" });
    expect(v.style.opacity).toBeLessThan(1);
    expect(v.label).toMatch(/rejected/);
  });
});

describe("routePath / pointAlong (#598)", () => {
  const pts = [{ x: 0, y: 0 }, { x: 100, y: 50 }, { x: 300, y: 50 }];

  it("passes through every point with horizontal tangents", () => {
    expect(routePath(pts)).toBe("M 0 0 C 50 0, 50 50, 100 50 C 200 50, 200 50, 300 50");
  });

  it("hits the endpoints and waypoints exactly", () => {
    expect(pointAlong(pts, 0).y).toBeCloseTo(0);
    expect(pointAlong(pts, 1).y).toBeCloseTo(50);
    const wp = pointAlong(pts, 1 / 3);
    expect(wp.x).toBeCloseTo(100);
    expect(wp.y).toBeCloseTo(50);
  });

  it("stays on the drawn curve, not the chord", () => {
    // Quarter of the way across the first hop: on routePath's cubic the label
    // is still near the start height, well short of the chord's 12.5.
    const p = pointAlong(pts, 1 / 12);
    expect(p.x).toBeCloseTo(25);
    expect(p.y).toBeLessThan(12.5 - 3);
  });

  it("puts a two-point edge's label at the midpoint of its S-curve", () => {
    const mid = pointAlong([{ x: 0, y: 0 }, { x: 200, y: 100 }], 0.5);
    expect(mid.x).toBe(100);
    expect(mid.y).toBeCloseTo(50);
  });
});

describe("labelAnchor (#601)", () => {
  const source = { x: 0, y: 0 };

  it("labels a plain edge across its whole span", () => {
    const target = { x: 200, y: 100 };
    expect(labelAnchor(source, [], target, 0.5)).toEqual(pointAlong([source, target], 0.5));
  });

  it("keeps edges that share a route apart by labelling each one's own last hop", () => {
    const shared = [{ x: 460, y: 22 }];
    const a = labelAnchor(source, shared, { x: 920, y: -88 }, 0.5);
    const b = labelAnchor(source, shared, { x: 920, y: 132 }, 0.5);
    expect(Math.abs(a.y - b.y)).toBeGreaterThan(50);
  });
});
