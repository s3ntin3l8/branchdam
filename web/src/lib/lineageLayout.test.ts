import { describe, expect, it } from "vitest";
import { lineageRanks, lineageRows, ROW_GAP_Y } from "./lineageLayout";

const e = (sourceNodeId: number, targetNodeId: number) => ({ sourceNodeId, targetNodeId });

describe("lineageRanks (#595)", () => {
  it("puts a child one column right of its deepest parent, so same-level edges disappear", () => {
    // 1 -> 2 -> 4 and 1 -> 3, 3 -> 4: a hop-distance layout puts 2 and 3 in one
    // column (3 -> 4 would then fan back), longest-path puts 4 right of both.
    const ranks = lineageRanks([1, 2, 3, 4], [e(1, 2), e(1, 3), e(2, 4), e(3, 4)]);
    expect([1, 2, 3, 4].map((id) => ranks.get(id))).toEqual([0, 1, 1, 2]);
  });

  it("separates two siblings joined by an edge (the demo hero's JPG -> edit.jpg case)", () => {
    // raw 1 produces both the JPG 2 and the edit 3, and the JPG is the edit's parent.
    const ranks = lineageRanks([1, 2, 3], [e(1, 2), e(1, 3), e(2, 3)]);
    expect(ranks.get(3)!).toBeGreaterThan(ranks.get(2)!);
  });

  it("points every edge forward for any DAG", () => {
    const edges = [e(5, 1), e(5, 2), e(1, 3), e(2, 3), e(3, 4), e(5, 4), e(2, 6)];
    const ranks = lineageRanks([1, 2, 3, 4, 5, 6], edges);
    edges.forEach((edge) => expect(ranks.get(edge.targetNodeId)!).toBeGreaterThan(ranks.get(edge.sourceNodeId)!));
  });

  it("places unconnected nodes in the first column", () => {
    expect(lineageRanks([1, 2], []).get(2)).toBe(0);
  });

  it("survives a cycle and ignores edges to unknown nodes", () => {
    const ranks = lineageRanks([1, 2], [e(1, 2), e(2, 1), e(1, 99), e(7, 7)]);
    expect(ranks.size).toBe(2);
    expect(Number.isFinite(ranks.get(1))).toBe(true);
  });
});

describe("lineageRows", () => {
  it("centres each column and spaces its nodes by ROW_GAP_Y", () => {
    const ranks = lineageRanks([1, 2, 3], [e(1, 2), e(1, 3)]);
    const rows = lineageRows(ranks, [e(1, 2), e(1, 3)]);
    expect(rows.get(1)).toBe(0);
    expect(rows.get(3)! - rows.get(2)!).toBe(ROW_GAP_Y);
    expect(rows.get(2)! + rows.get(3)!).toBe(0);
  });

  it("orders a column by its parents' height, to keep edges from crossing", () => {
    // 10 sits above 20 (by id); 21's parent is 10 and 11's is 20, so 21 must be
    // placed above 11 even though 11 has the smaller id.
    const edges = [e(10, 21), e(20, 11)];
    const ranks = lineageRanks([10, 20, 11, 21], edges);
    const rows = lineageRows(ranks, edges);
    expect(rows.get(21)!).toBeLessThan(rows.get(11)!);
  });
});
