import type { Edge as FlowEdge } from "@xyflow/react";
import type { CSSProperties } from "react";
import type { Edge } from "../api/types";

const relColor: Record<string, string> = {
  DERIVED_FROM: "var(--graph-edge-derived)",
  FINAL_EXPORT: "var(--graph-edge-final)",
  PROXY_OF: "var(--graph-edge-proxy)",
  PROJECT_SIDECAR: "var(--graph-edge-sidecar)",
  DUPLICATE_OF: "var(--graph-edge-duplicate)",
};

// edgeVisual is how the lineage graph draws one edge. The relationship type
// picks the colour; the review state picks the line: an unreviewed
// NEEDS_REVIEW guess is dashed and labelled so it can't be mistaken for a
// CONFIRMED / AUTO_ACCEPTED link, and a REJECTED edge (if one is ever
// returned) is dotted and faded.
export function edgeVisual(
  e: Pick<Edge, "relationshipType" | "confidence" | "reviewState">,
  strokeWidth?: number
): { style: CSSProperties; label: string } {
  const style: CSSProperties = { stroke: relColor[e.relationshipType] ?? "var(--graph-edge-default)" };
  if (strokeWidth !== undefined) style.strokeWidth = strokeWidth;
  let label = `${e.relationshipType} (${e.confidence.toFixed(2)})`;
  if (e.reviewState === "NEEDS_REVIEW") {
    style.strokeDasharray = "6 4";
    label += " - needs review";
  } else if (e.reviewState === "REJECTED") {
    style.strokeDasharray = "2 4";
    style.opacity = 0.4;
    label += " - rejected";
  }
  return { style, label };
}

/**
 * Where an edge's label sits. React Flow's built-in labels all sit at the
 * path midpoint, so edges converging on one handle stack their labels on top
 * of each other. `labelT` slides the label along the source->target line and
 * `labelDy` nudges it vertically, both chosen per edge by `labelSpread`.
 * `waypoints` route an edge that spans several columns around the nodes in
 * between (see `lineageRows`); edges from one source share them, so the label
 * goes on the edge's own last hop (see `labelAnchor`).
 */
export interface LineageEdgeData extends Record<string, unknown> {
  labelT: number;
  labelDy: number;
  /** Points the edge bends through, between its endpoints (flow coordinates). */
  waypoints?: { x: number; y: number }[];
  /**
   * Half the width of the column a routed edge's last waypoint sits in. The
   * label's hop starts at that waypoint (a column centre), so the free gap it
   * may use starts this far to its right (see `clampLabelX`).
   */
  slotHalfWidth?: number;
}

export type LineageFlowEdge = FlowEdge<LineageEdgeData, "lineage">;

const SPREAD_T_FROM = 0.3;
const SPREAD_T_TO = 0.6;
const SPREAD_DY = 24;

/**
 * Label position for edge `index` of `count` that share an endpoint. A lone
 * edge keeps the midpoint. Siblings are fanned out in both axes: the t range
 * leans towards the source end, where converging paths are still apart, and
 * the vertical step (a little over one label height) keeps labels from
 * overlapping even when the siblings' sources share a y. (For a routed edge `t`
 * is re-ranged onto its last hop by `labelAnchor`; the vertical step is what
 * keeps its siblings apart.)
 */
export function labelSpread(index: number, count: number): LineageEdgeData {
  if (count <= 1) return { labelT: 0.5, labelDy: 0 };
  const f = index / (count - 1);
  return {
    labelT: SPREAD_T_FROM + (SPREAD_T_TO - SPREAD_T_FROM) * f,
    labelDy: (index - (count - 1) / 2) * SPREAD_DY,
  };
}

export interface Point {
  x: number;
  y: number;
}

/**
 * Smooth left-to-right path through `points`: each hop is a cubic with
 * horizontal tangents at both ends, so it leaves and enters every point flat
 * and the pieces join without a kink.
 */
export function routePath(points: Point[]): string {
  let d = `M ${points[0].x} ${points[0].y}`;
  for (let i = 1; i < points.length; i++) {
    const a = points[i - 1];
    const b = points[i];
    const dx = (b.x - a.x) / 2;
    d += ` C ${a.x + dx} ${a.y}, ${b.x - dx} ${b.y}, ${b.x} ${b.y}`;
  }
  return d;
}

/**
 * Point on the drawn path a fraction `t` of the way across `points` (by x).
 * Each hop is the cubic `routePath` draws, whose x and y are not linear in
 * each other, so solve it for the parameter at the wanted x (x is monotone, so
 * bisection converges) rather than interpolating the chord: the label then
 * sits on the line, not beside it.
 */
export function pointAlong(points: Point[], t: number): Point {
  const first = points[0];
  const last = points[points.length - 1];
  const x = first.x + (last.x - first.x) * t;
  for (let i = 1; i < points.length; i++) {
    const a = points[i - 1];
    const b = points[i];
    if (x > b.x && i < points.length - 1) continue;
    // Horizontal tangents with controls at the hop's x midpoint:
    //   x(u) = a.x + (b.x - a.x) * (1.5u - 1.5u^2 + u^3)
    //   y(u) = a.y + (b.y - a.y) * (3u^2 - 2u^3)
    const fx = b.x === a.x ? 0 : Math.min(1, Math.max(0, (x - a.x) / (b.x - a.x)));
    let lo = 0;
    let hi = 1;
    for (let k = 0; k < 40; k++) {
      const u = (lo + hi) / 2;
      if (1.5 * u - 1.5 * u * u + u * u * u < fx) lo = u;
      else hi = u;
    }
    const u = (lo + hi) / 2;
    return { x, y: a.y + (b.y - a.y) * (3 * u * u - 2 * u * u * u) };
  }
  return first;
}

// A routed edge's label sits in the early part of its last hop, close to the
// free slot it leaves, which keeps it off the nodes in the target's column. On
// 300 random DAGs that put fewer labels over nodes than the midpoint (254 vs
// 306) and far fewer than the late end (401), with no more label/label overlap.
const ROUTED_T_FROM = 0.15;
const ROUTED_T_SPAN = 0.2;

/**
 * Where an edge's label goes. A plain edge labels its whole source->target
 * span at `t`. A routed edge's earlier hops are shared with every other edge
 * from the same source, so its label goes on the last hop (last waypoint ->
 * target), the only stretch that is its own, with `t` (from `labelSpread`,
 * 0.3-0.6) re-ranged to the early part of that hop.
 */
// Labels render at 11px (LineageEdge). This per-character width is a
// deliberate over-estimate for that size (caps, digits and underscores
// included), so a clamped label keeps its clearance even if the font is a
// little wider than expected.
const LABEL_CHAR_WIDTH = 6.2;
/** Horizontal padding LineageEdge puts on each side of a label. */
export const LABEL_PAD_X = 4;
/** Minimum space between a label and the node column next to it. */
const LABEL_NODE_CLEARANCE = 6;

/** Estimated rendered width of an edge label (flow units), padding included. */
export function estimateLabelWidth(text: string): number {
  return text.length * LABEL_CHAR_WIDTH + 2 * LABEL_PAD_X;
}

/**
 * Keeps a label of `width` centred at `x` inside the gap `[left, right]`
 * between the two node columns its hop joins. Nodes only ever sit inside
 * columns, so a label in the gap can't be drawn under one. Sliding a label
 * along its edge (`labelSpread`) can otherwise push a long label back over the
 * source node: centred at t = 0.3 of a 300-unit hop, "DERIVED_FROM (0.85) -
 * needs review" started under the node and read "ERIVED_FROM...". A label
 * wider than the gap is centred in it.
 */
export function clampLabelX(x: number, width: number, left: number, right: number): number {
  const lo = left + LABEL_NODE_CLEARANCE + width / 2;
  const hi = right - LABEL_NODE_CLEARANCE - width / 2;
  if (lo > hi) return (left + right) / 2;
  return Math.min(hi, Math.max(lo, x));
}

export function labelAnchor(source: Point, waypoints: Point[], target: Point, t: number): Point {
  if (waypoints.length === 0) return pointAlong([source, target], t);
  const spread = (Math.min(SPREAD_T_TO, Math.max(SPREAD_T_FROM, t)) - SPREAD_T_FROM) / (SPREAD_T_TO - SPREAD_T_FROM);
  return pointAlong([waypoints[waypoints.length - 1], target], ROUTED_T_FROM + ROUTED_T_SPAN * spread);
}

/**
 * Where LineageEdge draws an edge's label (its centre, flow coordinates). The
 * anchor comes from `labelAnchor`; it is then kept inside the free gap of the
 * label's hop (the whole edge, or a routed edge's last hop, which starts at a
 * column centre `slotHalfWidth` left of that gap) so it is never under a node,
 * put back on the line at that x, and nudged by `labelDy`.
 */
export function placeLabel(
  source: Point,
  waypoints: Point[],
  target: Point,
  label: string,
  { labelT = 0.5, labelDy = 0, slotHalfWidth = 0 }: Partial<Pick<LineageEdgeData, "labelT" | "labelDy" | "slotHalfWidth">> = {},
): Point {
  const at = labelAnchor(source, waypoints, target, labelT);
  const hop = waypoints.length > 0 ? [waypoints[waypoints.length - 1], target] : [source, target];
  const gapLeft = hop[0].x + (waypoints.length > 0 ? slotHalfWidth : 0);
  const x = clampLabelX(at.x, estimateLabelWidth(label), gapLeft, hop[1].x);
  const onLine = x === at.x || hop[1].x === hop[0].x ? at : pointAlong(hop, (x - hop[0].x) / (hop[1].x - hop[0].x));
  return { x, y: onLine.y + labelDy };
}
