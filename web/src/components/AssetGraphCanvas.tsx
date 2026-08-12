import { useMemo } from "react";
import { Background, Controls, ReactFlow, ReactFlowProvider, type Edge as FlowEdge, type Node as FlowNode } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import type { AssetGraph } from "../api/types";

/**
 * Renders the one-hop graph the backend returns (direct parents + direct
 * children of the focused asset) -- matches internal/httpapi's
 * /api/v1/assets/{id}/graph, which is deliberately one hop in increment 1
 * (see routes.go). Deeper traversal would mean re-querying per node the
 * user expands, not something this component does on its own yet.
 */
export default function AssetGraphCanvas({ assetId, graph }: { assetId: number; graph: AssetGraph }) {
  const { nodes, edges } = useMemo(() => buildFlow(assetId, graph), [assetId, graph]);

  if (graph.parents.length === 0 && graph.children.length === 0) {
    return <p className="text-sm text-neutral-500">No known lineage edges for this asset yet.</p>;
  }

  return (
    <div className="h-80 w-full rounded border border-neutral-800">
      <ReactFlowProvider>
        <ReactFlow nodes={nodes} edges={edges} fitView proOptions={{ hideAttribution: true }}>
          <Background />
          <Controls showInteractive={false} />
        </ReactFlow>
      </ReactFlowProvider>
    </div>
  );
}

const relColor: Record<string, string> = {
  DERIVED_FROM: "#38bdf8",
  FINAL_EXPORT: "#34d399",
  PROXY_OF: "#a78bfa",
  PROJECT_SIDECAR: "#fb923c",
  DUPLICATE_OF: "#f87171",
};

function buildFlow(assetId: number, graph: AssetGraph): { nodes: FlowNode[]; edges: FlowEdge[] } {
  const nodes: FlowNode[] = [
    {
      id: String(assetId),
      position: { x: 0, y: 0 },
      data: { label: "This asset" },
      style: { background: "#1e293b", color: "#fff", border: "1px solid #475569" },
    },
  ];
  const edges: FlowEdge[] = [];

  graph.parents.forEach((e, i) => {
    nodes.push({
      id: `parent-${e.sourceNodeId}`,
      position: { x: -260, y: i * 90 },
      data: { label: `Node ${e.sourceNodeId}` },
      style: { background: "#0f172a", color: "#cbd5e1", border: "1px solid #334155" },
    });
    edges.push({
      id: `e-${e.id}`,
      source: `parent-${e.sourceNodeId}`,
      target: String(assetId),
      label: `${e.relationshipType} (${e.confidence.toFixed(2)})`,
      style: { stroke: relColor[e.relationshipType] ?? "#64748b" },
      labelStyle: { fill: "#cbd5e1", fontSize: 11 },
    });
  });

  graph.children.forEach((e, i) => {
    nodes.push({
      id: `child-${e.targetNodeId}`,
      position: { x: 260, y: i * 90 },
      data: { label: `Node ${e.targetNodeId}` },
      style: { background: "#0f172a", color: "#cbd5e1", border: "1px solid #334155" },
    });
    edges.push({
      id: `e-${e.id}`,
      source: String(assetId),
      target: `child-${e.targetNodeId}`,
      label: `${e.relationshipType} (${e.confidence.toFixed(2)})`,
      style: { stroke: relColor[e.relationshipType] ?? "#64748b" },
      labelStyle: { fill: "#cbd5e1", fontSize: 11 },
    });
  });

  return { nodes, edges };
}
