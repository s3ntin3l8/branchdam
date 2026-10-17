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
 */
export interface LineageEdgeData extends Record<string, unknown> {
  labelT: number;
  labelDy: number;
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
