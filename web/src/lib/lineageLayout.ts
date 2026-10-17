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

/** A column slot an edge passes through between its endpoints' columns. */
export interface RouteSlot {
  rank: number;
  y: number;
}

/**
 * Vertical position of every node, plus a route for each edge that spans more
 * than one column. Such an edge gets a dummy slot in every column it crosses;
 * the slot is laid out like a node, so real nodes make room for it and the
 * edge bends through the gap instead of running behind a node (#598).
 *
 * Within a column, items are ordered by the mean height of their parents (then
 * id, for a stable order across refetches) so edges between neighbouring
 * columns run roughly flat instead of crossing; columns are stacked and
 * centred. `routes` is keyed by index into `edges`.
 */
export function lineageRows(
  ranks: Map<number, number>,
  edges: LayoutEdge[],
): { rows: Map<number, number>; routes: Map<number, RouteSlot[]> } {
  interface Item {
    key: string;
    rank: number;
    sortId: number;
    parents: string[];
  }
  const items = new Map<string, Item>();
  const nodeKey = (id: number) => `n${id}`;
  ranks.forEach((rank, id) => items.set(nodeKey(id), { key: nodeKey(id), rank, sortId: id, parents: [] }));

  const slotKeys = new Map<number, string[]>();
  edges.forEach((e, i) => {
    const from = ranks.get(e.sourceNodeId);
    const to = ranks.get(e.targetNodeId);
    if (from === undefined || to === undefined || to <= from) return;
    let prev = nodeKey(e.sourceNodeId);
    const keys: string[] = [];
    for (let rank = from + 1; rank < to; rank++) {
      const key = `d${i}:${rank}`;
      items.set(key, { key, rank, sortId: e.sourceNodeId, parents: [prev] });
      keys.push(key);
      prev = key;
    }
    items.get(nodeKey(e.targetNodeId))!.parents.push(prev);
    slotKeys.set(i, keys);
  });

  const columns = new Map<number, Item[]>();
  items.forEach((item) => {
    if (!columns.has(item.rank)) columns.set(item.rank, []);
    columns.get(item.rank)!.push(item);
  });

  const y = new Map<string, number>();
  [...columns.keys()]
    .sort((a, b) => a - b)
    .forEach((rank) => {
      const column = columns.get(rank)!;
      const barycentre = (item: Item): number => {
        const placed = item.parents.filter((p) => y.has(p));
        return placed.length === 0 ? 0 : placed.reduce((sum, p) => sum + y.get(p)!, 0) / placed.length;
      };
      column.sort((a, b) => barycentre(a) - barycentre(b) || a.sortId - b.sortId || (a.key < b.key ? -1 : 1));
      column.forEach((item, i) => y.set(item.key, (i - (column.length - 1) / 2) * ROW_GAP_Y));
    });

  const rows = new Map<number, number>();
  ranks.forEach((_, id) => rows.set(id, y.get(nodeKey(id))!));
  const routes = new Map<number, RouteSlot[]>();
  slotKeys.forEach((keys, i) => {
    if (keys.length > 0) routes.set(i, keys.map((key) => ({ rank: items.get(key)!.rank, y: y.get(key)! })));
  });
  return { rows, routes };
}
