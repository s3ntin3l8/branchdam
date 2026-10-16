import { describe, expect, it } from "vitest";
import { edgeVisual } from "./edgeStyle";

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
