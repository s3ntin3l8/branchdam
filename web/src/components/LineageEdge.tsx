import { BaseEdge, EdgeLabelRenderer, getBezierPath, type EdgeProps } from "@xyflow/react";
import { labelAnchor, routePath, type LineageFlowEdge } from "../lib/edgeStyle";

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
  const at = labelAnchor(points[0], waypoints, points[points.length - 1], data?.labelT ?? 0.5);
  const labelX = at.x;
  const labelY = at.y + (data?.labelDy ?? 0);

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
              padding: "2px 4px",
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
