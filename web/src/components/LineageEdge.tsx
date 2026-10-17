import { BaseEdge, EdgeLabelRenderer, getBezierPath, type EdgeProps } from "@xyflow/react";
import type { LineageFlowEdge } from "../lib/edgeStyle";

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
  const [path] = getBezierPath({ sourceX, sourceY, targetX, targetY, sourcePosition, targetPosition });
  const t = data?.labelT ?? 0.5;
  const labelX = sourceX + (targetX - sourceX) * t;
  const labelY = sourceY + (targetY - sourceY) * t + (data?.labelDy ?? 0);

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
