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
