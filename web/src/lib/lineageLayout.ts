/** Edge endpoints are all the layout needs; the API's Edge type satisfies it. */
export interface LayoutEdge {
  sourceNodeId: number;
  targetNodeId: number;
}

/** Vertical gap between nodes in one column. */
export const ROW_GAP_Y = 110;

/**
 * Assigns each node a column (rank) so every edge points left to right: a node
 * sits one column right of its deepest parent (longest-path layering). Ranking
 * from edge direction, rather than by hop distance from the focused asset,
 * means two nodes joined by an edge never share a column, so no edge has to
 * loop back around its own nodes.
 *
 * Lineage is a DAG (the graph package refuses cycles), but a back edge in
 * malformed data is ignored rather than recursing forever.
 */
export function lineageRanks(nodeIds: number[], edges: LayoutEdge[]): Map<number, number> {
  const known = new Set(nodeIds);
  const parents = new Map<number, number[]>();
  edges.forEach((e) => {
    if (e.sourceNodeId === e.targetNodeId || !known.has(e.sourceNodeId) || !known.has(e.targetNodeId)) return;
    if (!parents.has(e.targetNodeId)) parents.set(e.targetNodeId, []);
    parents.get(e.targetNodeId)!.push(e.sourceNodeId);
  });

  const ranks = new Map<number, number>();
  const visiting = new Set<number>();
  const visit = (id: number): number => {
    const done = ranks.get(id);
    if (done !== undefined) return done;
    if (visiting.has(id)) return -1; // back edge: contributes nothing
    visiting.add(id);
    let rank = 0;
    for (const p of parents.get(id) ?? []) {
      const parentRank = visit(p);
      if (parentRank >= 0) rank = Math.max(rank, parentRank + 1);
    }
    visiting.delete(id);
    ranks.set(id, rank);
    return rank;
  };
  [...nodeIds].sort((a, b) => a - b).forEach(visit);
  return ranks;
}

/**
 * Vertical position of every node: columns are stacked and centred, with each
 * node ordered by the mean height of its parents (then id, for a stable order
 * across refetches) so edges between neighbouring columns run roughly flat
 * instead of crossing.
 */
export function lineageRows(ranks: Map<number, number>, edges: LayoutEdge[]): Map<number, number> {
  const parents = new Map<number, number[]>();
  edges.forEach((e) => {
    if (!ranks.has(e.sourceNodeId) || !ranks.has(e.targetNodeId)) return;
    if (!parents.has(e.targetNodeId)) parents.set(e.targetNodeId, []);
    parents.get(e.targetNodeId)!.push(e.sourceNodeId);
  });

  const columns = new Map<number, number[]>();
  ranks.forEach((rank, id) => {
    if (!columns.has(rank)) columns.set(rank, []);
    columns.get(rank)!.push(id);
  });

  const y = new Map<number, number>();
  [...columns.keys()]
    .sort((a, b) => a - b)
    .forEach((rank) => {
      const ids = columns.get(rank)!;
      const barycentre = (id: number): number => {
        const placed = (parents.get(id) ?? []).filter((p) => y.has(p));
        return placed.length === 0 ? 0 : placed.reduce((sum, p) => sum + y.get(p)!, 0) / placed.length;
      };
      ids.sort((a, b) => barycentre(a) - barycentre(b) || a - b);
      ids.forEach((id, i) => y.set(id, (i - (ids.length - 1) / 2) * ROW_GAP_Y));
    });
  return y;
}
