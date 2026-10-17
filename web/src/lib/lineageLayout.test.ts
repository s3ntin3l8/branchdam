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
    const { rows } = lineageRows(ranks, [e(1, 2), e(1, 3)]);
    expect(rows.get(1)).toBe(0);
    expect(rows.get(3)! - rows.get(2)!).toBe(ROW_GAP_Y);
    expect(rows.get(2)! + rows.get(3)!).toBe(0);
  });

  it("orders a column by its parents' height, to keep edges from crossing", () => {
    // 10 sits above 20 (by id); 21's parent is 10 and 11's is 20, so 21 must be
    // placed above 11 even though 11 has the smaller id.
    const edges = [e(10, 21), e(20, 11)];
    const ranks = lineageRanks([10, 20, 11, 21], edges);
    const { rows } = lineageRows(ranks, edges);
    expect(rows.get(21)!).toBeLessThan(rows.get(11)!);
  });
});

describe("lineageRows routes (#598)", () => {
  // 1 -> 2 -> 3 -> 4 is a spine; 1 -> 4 skips columns 1 and 2.
  const edges = [e(1, 2), e(2, 3), e(3, 4), e(1, 4)];
  const ranks = lineageRanks([1, 2, 3, 4], edges);

  it("gives an edge spanning several columns a slot in each column it crosses", () => {
    const { routes } = lineageRows(ranks, edges);
    expect(routes.get(3)!.map((s) => s.rank)).toEqual([1, 2]);
    expect(routes.has(0)).toBe(false); // adjacent columns: no route
  });

  it("keeps the slots clear of the real nodes in those columns", () => {
    const { rows, routes } = lineageRows(ranks, edges);
    const [first, second] = routes.get(3)!;
    expect(Math.abs(first.y - rows.get(2)!)).toBeGreaterThanOrEqual(ROW_GAP_Y);
    expect(Math.abs(second.y - rows.get(3)!)).toBeGreaterThanOrEqual(ROW_GAP_Y);
  });

  it("is deterministic", () => {
    expect(lineageRows(ranks, edges)).toEqual(lineageRows(ranks, edges));
  });
});

describe("shared route slots (#601)", () => {
  it("shares one slot between edges from the same source", () => {
    // 1 -> 2 -> 3 -> 4 spine; 1 -> 3 and 1 -> 4 both cross column 1 from node 1.
    const edges = [e(1, 2), e(2, 3), e(3, 4), e(1, 3), e(1, 4)];
    const ranks = lineageRanks([1, 2, 3, 4], edges);
    const { routes } = lineageRows(ranks, edges);
    expect(routes.get(3)![0]).toEqual(routes.get(4)![0]); // both cross column 1
    expect(routes.get(4)).toHaveLength(2); // the longer edge continues past the shared slot
  });

  it("gives different sources their own slot in a column they both cross", () => {
    const edges = [e(1, 2), e(2, 3), e(3, 4), e(1, 4), e(5, 4)];
    const ranks = lineageRanks([1, 2, 3, 4, 5], edges);
    const { routes } = lineageRows(ranks, edges);
    expect(routes.get(3)![0].y).not.toBe(routes.get(4)![0].y);
  });

  it("keeps a master linked to every version of a chain within 2x of its node-only height", () => {
    const ids = [1];
    const edges = [];
    for (let i = 2; i <= 12; i++) {
      ids.push(i);
      edges.push(e(i - 1, i));
      if (i > 2) edges.push(e(1, i));
    }
    const ranks = lineageRanks(ids, edges);
    const { rows, routes } = lineageRows(ranks, edges);
    const perRank = new Map<number, Set<number>>();
    const add = (rank: number, y: number) => perRank.set(rank, (perRank.get(rank) ?? new Set()).add(y));
    ranks.forEach((rank, id) => add(rank, rows.get(id)!));
    routes.forEach((slots) => slots.forEach((s) => add(s.rank, s.y)));
    // Node-only, every column has one row; unshared slots made it 10 in the worst column.
    expect(Math.max(...[...perRank.values()].map((ys) => ys.size))).toBeLessThanOrEqual(2);
  });

  it("never puts a route slot on a real node, over random DAGs", () => {
    let seed = 12345;
    const rand = () => (seed = (seed * 1664525 + 1013904223) % 4294967296) / 4294967296;
    for (let g = 0; g < 200; g++) {
      const n = 4 + Math.floor(rand() * 10);
      const ids = Array.from({ length: n }, (_, i) => i + 1);
      const edges = [];
      for (let a = 1; a <= n; a++) for (let b = a + 1; b <= n; b++) if (rand() < 0.3) edges.push(e(a, b));
      const ranks = lineageRanks(ids, edges);
      const { rows, routes } = lineageRows(ranks, edges);
      routes.forEach((slots) =>
        slots.forEach((s) => {
          ids.filter((id) => ranks.get(id) === s.rank).forEach((id) => expect(rows.get(id)).not.toBe(s.y));
        }),
      );
    }
  });
});
