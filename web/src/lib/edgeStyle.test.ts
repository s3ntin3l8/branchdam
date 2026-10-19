import { describe, expect, it } from "vitest";
import { clampLabelX, edgeVisual, estimateLabelWidth, labelAnchor, placeLabel, pointAlong, routePath } from "./edgeStyle";

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

  it("anchors a routed edge's label early on its last hop, from the spread t", () => {
    const wp = [{ x: 460, y: 22 }];
    const target = { x: 920, y: 22 };
    const first = labelAnchor(source, wp, target, 0.3);
    const last = labelAnchor(source, wp, target, 0.6);
    expect(first.x).toBeCloseTo(460 + 0.15 * 460);
    expect(last.x).toBeCloseTo(460 + 0.35 * 460);
  });

  it("keeps edges that share a route apart by labelling each one's own last hop", () => {
    const shared = [{ x: 460, y: 22 }];
    const a = labelAnchor(source, shared, { x: 920, y: -88 }, 0.5);
    const b = labelAnchor(source, shared, { x: 920, y: 132 }, 0.5);
    expect(Math.abs(a.y - b.y)).toBeGreaterThan(25); // more than a label is tall
  });
});

describe("label clearance", () => {
  it("keeps a label inside the gap between the two columns it joins", () => {
    // The demo hero case: a 34-character label centred at t = 0.3 of a
    // 300-unit hop used to start under the source node.
    const w = estimateLabelWidth("DERIVED_FROM (0.85) - needs review");
    const x = clampLabelX(90, w, 0, 300);
    expect(x - w / 2).toBeGreaterThan(0);
    expect(x + w / 2).toBeLessThan(300);
    // A label that already fits stays where labelSpread put it.
    expect(clampLabelX(150, 40, 0, 300)).toBe(150);
  });

  it("centres a label that is wider than the gap", () => {
    expect(clampLabelX(10, 500, 0, 300)).toBe(150);
  });

  it("keeps the moved label on its line", () => {
    const source = { x: 0, y: 0 };
    const target = { x: 300, y: 100 };
    const label = "DERIVED_FROM (0.85) - needs review";
    const at = placeLabel(source, [], target, label, { labelT: 0.3, labelDy: 0 });
    expect(at.x - estimateLabelWidth(label) / 2).toBeGreaterThan(source.x);
    expect(at.y).toBeCloseTo(pointAlong([source, target], at.x / 300).y);
    expect(placeLabel(source, [], target, label, { labelT: 0.3, labelDy: 12 }).y).toBeCloseTo(at.y + 12);
  });

  it("keeps a routed edge's label clear of the column its last waypoint sits in", () => {
    // Last waypoint at a column centre (x = 80 for a 160-wide column).
    const label = "PROXY_OF (0.60) - needs review";
    const at = placeLabel({ x: -300, y: 0 }, [{ x: 80, y: 50 }], { x: 460, y: 0 }, label, { labelT: 0.3, slotHalfWidth: 80 });
    expect(at.x - estimateLabelWidth(label) / 2).toBeGreaterThan(160);
    expect(at.x + estimateLabelWidth(label) / 2).toBeLessThan(460);
  });
});
