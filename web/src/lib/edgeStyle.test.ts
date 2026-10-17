import { describe, expect, it } from "vitest";
import { edgeVisual, pointAlong, routePath } from "./edgeStyle";

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

  it("interpolates along x across the segments", () => {
    expect(pointAlong(pts, 0)).toEqual({ x: 0, y: 0 });
    expect(pointAlong(pts, 1)).toEqual({ x: 300, y: 50 });
    expect(pointAlong(pts, 1 / 3)).toEqual({ x: 100, y: 50 }); // a waypoint exactly
    expect(pointAlong(pts, 1 / 6).y).toBeCloseTo(25);
  });

  it("matches the straight source->target line when there are no waypoints", () => {
    expect(pointAlong([{ x: 0, y: 0 }, { x: 200, y: 100 }], 0.5)).toEqual({ x: 100, y: 50 });
  });
});
