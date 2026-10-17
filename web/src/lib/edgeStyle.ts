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
 * overlapping even when the siblings' sources share a y.
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

/**
 * Where an edge's label goes. A plain edge labels its whole source->target
 * span. A routed edge's earlier hops are shared with every other edge from the
 * same source, so its label goes on the last hop (last waypoint -> target),
 * the only stretch that is its own.
 */
export function labelAnchor(source: Point, waypoints: Point[], target: Point, t: number): Point {
  const from = waypoints.length > 0 ? waypoints[waypoints.length - 1] : source;
  return pointAlong([from, target], t);
}
