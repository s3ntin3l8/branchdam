import { useMemo } from "react";
import { useNavigate } from "react-router";
import { Background, Controls, ReactFlow, ReactFlowProvider, type Edge as FlowEdge, type Node as FlowNode } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import type { AssetGraph, LineageResponse } from "../api/types";

interface AssetGraphCanvasProps {
  assetId: number;
  lineage?: LineageResponse;
  graph?: AssetGraph;
}

const RAW_EXTS = new Set([
  ".raw", ".arw", ".cr2", ".cr3", ".nef", ".dng", ".raf", ".orf", ".rw2", ".pef", ".srw", ".3fr", ".fff"
]);

const SIDECAR_EXTS = new Set([
  ".xmp", ".drp", ".fcpxml", ".edl", ".aep", ".prproj"
]);

const EXPORT_EXTS = new Set([
  ".jpg", ".jpeg", ".png", ".tif", ".tiff", ".webp", ".mp4", ".mov", ".mkv", ".avi"
]);

function getNodeCategoryColor(ext: string): { border: string; bg: string } {
  const cleanExt = ext.toLowerCase();
  if (RAW_EXTS.has(cleanExt)) {
    return { border: "var(--graph-node-border-raw)", bg: "var(--graph-node-bg-raw)" };
  }
  if (SIDECAR_EXTS.has(cleanExt)) {
    return { border: "var(--graph-node-border-sidecar)", bg: "var(--graph-node-bg-sidecar)" };
  }
  if (EXPORT_EXTS.has(cleanExt)) {
    return { border: "var(--graph-node-border-export)", bg: "var(--graph-node-bg-export)" };
  }
  return { border: "var(--graph-node-border-neutral)", bg: "var(--graph-node-bg-neutral)" };
}

const relColor: Record<string, string> = {
  DERIVED_FROM: "var(--graph-edge-derived)",
  FINAL_EXPORT: "var(--graph-edge-final)",
  PROXY_OF: "var(--graph-edge-proxy)",
  PROJECT_SIDECAR: "var(--graph-edge-sidecar)",
  DUPLICATE_OF: "var(--graph-edge-duplicate)",
};

export default function AssetGraphCanvas({ assetId, lineage, graph }: AssetGraphCanvasProps) {
  const navigate = useNavigate();

  const { nodes, edges, isEmpty } = useMemo(() => {
    if (lineage) {
      return buildLineageFlow(assetId, lineage);
    }
    if (graph) {
      return buildOneHopFlow(assetId, graph);
    }
    return { nodes: [], edges: [], isEmpty: true };
  }, [assetId, lineage, graph]);

  if (isEmpty) {
    return <p className="text-sm text-neutral-500">No known lineage edges for this asset yet.</p>;
  }

  const handleNodeClick = (_: React.MouseEvent, node: FlowNode) => {
    const targetId = node.id.replace(/^(parent-|child-)/, "");
    if (targetId && !Number.isNaN(Number(targetId))) {
      navigate(`/assets/${targetId}`);
    }
  };

  return (
    <div className="h-96 w-full rounded border border-neutral-800">
      <ReactFlowProvider>
        <ReactFlow
          nodes={nodes}
          edges={edges}
          onNodeClick={handleNodeClick}
          fitView
          proOptions={{ hideAttribution: true }}
        >
          <Background />
          <Controls showInteractive={false} />
        </ReactFlow>
      </ReactFlowProvider>
    </div>
  );
}

function buildLineageFlow(rootId: number, lineage: LineageResponse): { nodes: FlowNode[]; edges: FlowEdge[]; isEmpty: boolean } {
  if (!lineage.nodes || lineage.nodes.length <= 1 && lineage.edges.length === 0) {
    return { nodes: [], edges: [], isEmpty: true };
  }

  // Calculate BFS levels relative to root
  const levels = new Map<number, number>();
  levels.set(rootId, 0);

  const adjParents = new Map<number, number[]>();
  const adjChildren = new Map<number, number[]>();

  lineage.edges.forEach((e) => {
    if (!adjChildren.has(e.sourceNodeId)) adjChildren.set(e.sourceNodeId, []);
    adjChildren.get(e.sourceNodeId)!.push(e.targetNodeId);

    if (!adjParents.has(e.targetNodeId)) adjParents.set(e.targetNodeId, []);
    adjParents.get(e.targetNodeId)!.push(e.sourceNodeId);
  });

  // BFS Upstream (Parents)
  const qUp: { id: number; level: number }[] = [{ id: rootId, level: 0 }];
  const visitedUp = new Set<number>([rootId]);
  while (qUp.length > 0) {
    const curr = qUp.shift()!;
    const parents = adjParents.get(curr.id) || [];
    parents.forEach((p) => {
      if (!visitedUp.has(p)) {
        visitedUp.add(p);
        const l = curr.level - 1;
        levels.set(p, l);
        qUp.push({ id: p, level: l });
      }
    });
  }

  // BFS Downstream (Children)
  const qDown: { id: number; level: number }[] = [{ id: rootId, level: 0 }];
  const visitedDown = new Set<number>([rootId]);
  while (qDown.length > 0) {
    const curr = qDown.shift()!;
    const children = adjChildren.get(curr.id) || [];
    children.forEach((c) => {
      if (!visitedDown.has(c)) {
        visitedDown.add(c);
        const l = curr.level + 1;
        levels.set(c, l);
        qDown.push({ id: c, level: l });
      }
    });
  }

  // Group nodes by level to compute deterministic positions
  const levelGroups = new Map<number, number[]>();
  lineage.nodes.forEach((n) => {
    const l = levels.get(n.id) ?? 0;
    if (!levelGroups.has(l)) levelGroups.set(l, []);
    levelGroups.get(l)!.push(n.id);
  });

  // Sort node IDs inside level groups for deterministic ordering across refetches
  levelGroups.forEach((ids) => ids.sort((a, b) => a - b));

  const nodes: FlowNode[] = [];
  lineage.nodes.forEach((n) => {
    const isRoot = n.id === rootId;
    const l = levels.get(n.id) ?? 0;
    const group = levelGroups.get(l) || [n.id];
    const index = group.indexOf(n.id);

    const x = l * 280;
    const y = (index - (group.length - 1) / 2) * 110;

    const colors = getNodeCategoryColor(n.fileExt || "");

    nodes.push({
      id: String(n.id),
      position: { x, y },
      data: {
        label: (
          <div className="text-center">
            <div className="font-semibold">{n.fileName}</div>
            {isRoot && <div className="mt-0.5 text-[10px] uppercase font-bold text-amber-300">(Root Asset)</div>}
          </div>
        ),
      },
      style: {
        background: isRoot ? "var(--graph-node-bg-root)" : colors.bg,
        color: "var(--graph-text)",
        border: isRoot ? "3px solid var(--color-brand)" : `2px solid ${colors.border}`,
        borderRadius: "6px",
        padding: "8px 12px",
        cursor: "pointer",
      },
    });
  });

  const edges: FlowEdge[] = lineage.edges.map((e) => ({
    id: `e-${e.id}`,
    source: String(e.sourceNodeId),
    target: String(e.targetNodeId),
    label: `${e.relationshipType} (${e.confidence.toFixed(2)})`,
    style: { stroke: relColor[e.relationshipType] ?? "var(--graph-edge-default)", strokeWidth: 2 },
    labelStyle: { fill: "var(--graph-text)", fontSize: 11 },
  }));

  return { nodes, edges, isEmpty: false };
}

function buildOneHopFlow(assetId: number, graph: AssetGraph): { nodes: FlowNode[]; edges: FlowEdge[]; isEmpty: boolean } {
  if (graph.parents.length === 0 && graph.children.length === 0) {
    return { nodes: [], edges: [], isEmpty: true };
  }

  const nodes: FlowNode[] = [
    {
      id: String(assetId),
      position: { x: 0, y: 0 },
      data: { label: "This asset" },
      style: { background: "var(--graph-node-bg-root)", color: "var(--graph-text)", border: "3px solid var(--color-brand)", borderRadius: "6px", cursor: "pointer" },
    },
  ];
  const edges: FlowEdge[] = [];

  graph.parents.forEach((e, i) => {
    nodes.push({
      id: `parent-${e.sourceNodeId}`,
      position: { x: -260, y: (i - (graph.parents.length - 1) / 2) * 90 },
      data: { label: `Node ${e.sourceNodeId}` },
      style: { background: "var(--graph-node-bg-neutral)", color: "var(--graph-text)", border: "1px solid var(--graph-node-border-neutral)", borderRadius: "6px", cursor: "pointer" },
    });
    edges.push({
      id: `e-${e.id}`,
      source: `parent-${e.sourceNodeId}`,
      target: String(assetId),
      label: `${e.relationshipType} (${e.confidence.toFixed(2)})`,
      style: { stroke: relColor[e.relationshipType] ?? "var(--graph-edge-default)" },
      labelStyle: { fill: "var(--graph-text)", fontSize: 11 },
    });
  });

  graph.children.forEach((e, i) => {
    nodes.push({
      id: `child-${e.targetNodeId}`,
      position: { x: 260, y: (i - (graph.children.length - 1) / 2) * 90 },
      data: { label: `Node ${e.targetNodeId}` },
      style: { background: "var(--graph-node-bg-neutral)", color: "var(--graph-text)", border: "1px solid var(--graph-node-border-neutral)", borderRadius: "6px", cursor: "pointer" },
    });
    edges.push({
      id: `e-${e.id}`,
      source: String(assetId),
      target: `child-${e.targetNodeId}`,
      label: `${e.relationshipType} (${e.confidence.toFixed(2)})`,
      style: { stroke: relColor[e.relationshipType] ?? "var(--graph-edge-default)" },
      labelStyle: { fill: "var(--graph-text)", fontSize: 11 },
    });
  });

  return { nodes, edges, isEmpty: false };
}
