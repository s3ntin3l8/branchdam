import { BaseEdge, EdgeLabelRenderer, getBezierPath, type EdgeProps } from "@xyflow/react";
import { LABEL_PAD_X, labelAnchor, placeLabel, routePath, type LineageFlowEdge } from "../lib/edgeStyle";

export default function LineageEdge({
  id,
  sourceX,
  sourceY,
  targetX,
  targetY,
  sourcePosition,
  targetPosition,
  style,
  markerEnd,
  label,
  data,
}: EdgeProps<LineageFlowEdge>) {
  const waypoints = data?.waypoints ?? [];
  // A plain edge is one bezier; one spanning several columns bends through
  // free slots so it does not run behind the nodes in between.
  const points = [{ x: sourceX, y: sourceY }, ...waypoints, { x: targetX, y: targetY }];
  const path =
    waypoints.length > 0
      ? routePath(points)
      : getBezierPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition })[0];
  const source = points[0];
  const target = points[points.length - 1];
  // A string label is kept clear of the nodes (see placeLabel); anything else
  // has no width estimate, so it stays at its anchor.
  const anchor = labelAnchor(source, waypoints, target, data?.labelT ?? 0.5);
  const { x: labelX, y: labelY } =
    typeof label === "string"
      ? placeLabel(source, waypoints, target, label, data ?? {})
      : { x: anchor.x, y: anchor.y + (data?.labelDy ?? 0) };

  return (
    <>
      <BaseEdge id={id} path={path} style={style} markerEnd={markerEnd} />
      {label ? (
        <EdgeLabelRenderer>
          <div
            // Not interactive: clicks and drags pass through to the canvas.
            className="nodrag nopan"
            style={{
              position: "absolute",
              transform: `translate(-50%, -50%) translate(${labelX}px, ${labelY}px)`,
              pointerEvents: "none",
              padding: `2px ${LABEL_PAD_X}px`,
              borderRadius: 2,
              fontSize: 11,
              whiteSpace: "nowrap",
              background: "var(--xy-edge-label-background-color)",
              color: "var(--xy-edge-label-color)",
            }}
          >
            {label}
          </div>
        </EdgeLabelRenderer>
      ) : null}
    </>
  );
}
