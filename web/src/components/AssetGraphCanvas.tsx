import { useMemo } from "react";
import { useNavigate } from "react-router";
import { Background, Controls, Position, ReactFlow, ReactFlowProvider, type Node as FlowNode } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import type { AssetGraph, LineageResponse } from "../api/types";
import { edgeVisual, labelSpread, type LineageFlowEdge } from "../lib/edgeStyle";
import { lineageRanks, lineageRows } from "../lib/lineageLayout";
import LineageEdge from "./LineageEdge";

// Columns run left to right, so edges leave a node's right side and enter the
// next one's left; the default top/bottom handles made every edge loop around
// its nodes and park its label on top of them.
const HORIZONTAL_HANDLES = { sourcePosition: Position.Right, targetPosition: Position.Left } as const;
// Wide enough that a label sits between two columns instead of on a node.
const LEVEL_GAP_X = 460;

// Module-level so React Flow sees a stable reference across renders.
const edgeTypes = { lineage: LineageEdge };

/** Spread labels of edges that share `key(edge)`, in the given (deterministic) order. */
function spreadLabels<T>(edges: T[], key: (e: T) => string): Map<T, ReturnType<typeof labelSpread>> {
  const groups = new Map<string, T[]>();
  edges.forEach((e) => {
    const k = key(e);
    if (!groups.has(k)) groups.set(k, []);
    groups.get(k)!.push(e);
  });
  const out = new Map<T, ReturnType<typeof labelSpread>>();
  groups.forEach((group) => group.forEach((e, i) => out.set(e, labelSpread(i, group.length))));
  return out;
}

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
          edgeTypes={edgeTypes}
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

export function buildLineageFlow(rootId: number, lineage: LineageResponse): { nodes: FlowNode[]; edges: LineageFlowEdge[]; isEmpty: boolean } {
  if (!lineage.nodes || lineage.nodes.length <= 1 && lineage.edges.length === 0) {
    return { nodes: [], edges: [], isEmpty: true };
  }

  const ranks = lineageRanks(lineage.nodes.map((n) => n.id), lineage.edges);
  const rows = lineageRows(ranks, lineage.edges);

  const nodes: FlowNode[] = [];
  lineage.nodes.forEach((n) => {
    const isRoot = n.id === rootId;
    const x = (ranks.get(n.id) ?? 0) * LEVEL_GAP_X;
    const y = rows.get(n.id) ?? 0;

    const colors = getNodeCategoryColor(n.fileExt || "");

    nodes.push({
      id: String(n.id),
      position: { x, y },
      ...HORIZONTAL_HANDLES,
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

  // Edges into one node converge on the same handle; fan their labels out.
  const spread = spreadLabels(lineage.edges, (e) => String(e.targetNodeId));
  const edges: LineageFlowEdge[] = lineage.edges.map((e) => ({
    id: `e-${e.id}`,
    type: "lineage",
    source: String(e.sourceNodeId),
    target: String(e.targetNodeId),
    ...edgeVisual(e, 2),
    data: spread.get(e)!,
  }));

  return { nodes, edges, isEmpty: false };
}

export function buildOneHopFlow(assetId: number, graph: AssetGraph): { nodes: FlowNode[]; edges: LineageFlowEdge[]; isEmpty: boolean } {
  if (graph.parents.length === 0 && graph.children.length === 0) {
    return { nodes: [], edges: [], isEmpty: true };
  }

  const nodes: FlowNode[] = [
    {
      id: String(assetId),
      position: { x: 0, y: 0 },
      ...HORIZONTAL_HANDLES,
      data: { label: "This asset" },
      style: { background: "var(--graph-node-bg-root)", color: "var(--graph-text)", border: "3px solid var(--color-brand)", borderRadius: "6px", cursor: "pointer" },
    },
  ];
  const edges: LineageFlowEdge[] = [];
  // Parents all converge on this asset and children all leave it, so each
  // side spreads as one group.
  const parentSpread = spreadLabels(graph.parents, () => "parents");
  const childSpread = spreadLabels(graph.children, () => "children");

  // Several edges (different relationship types) can join the same pair of
  // nodes; React Flow needs unique node ids, so emit each neighbour once
  // and let every edge keep its own id.
  const seenNeighbours = new Set<string>();
  graph.parents.forEach((e, i) => {
    const nodeId = `parent-${e.sourceNodeId}`;
    if (!seenNeighbours.has(nodeId)) {
      seenNeighbours.add(nodeId);
      nodes.push({
        id: `parent-${e.sourceNodeId}`,
        position: { x: -LEVEL_GAP_X, y: (i - (graph.parents.length - 1) / 2) * 90 },
        ...HORIZONTAL_HANDLES,
        data: { label: `Node ${e.sourceNodeId}` },
        style: { background: "var(--graph-node-bg-neutral)", color: "var(--graph-text)", border: "1px solid var(--graph-node-border-neutral)", borderRadius: "6px", cursor: "pointer" },
      });
    }
    edges.push({
      id: `e-${e.id}`,
      source: `parent-${e.sourceNodeId}`,
      target: String(assetId),
      type: "lineage",
      ...edgeVisual(e),
      data: parentSpread.get(e)!,
    });
  });

  graph.children.forEach((e, i) => {
    const nodeId = `child-${e.targetNodeId}`;
    if (!seenNeighbours.has(nodeId)) {
      seenNeighbours.add(nodeId);
      nodes.push({
        id: `child-${e.targetNodeId}`,
        position: { x: LEVEL_GAP_X, y: (i - (graph.children.length - 1) / 2) * 90 },
        ...HORIZONTAL_HANDLES,
        data: { label: `Node ${e.targetNodeId}` },
        style: { background: "var(--graph-node-bg-neutral)", color: "var(--graph-text)", border: "1px solid var(--graph-node-border-neutral)", borderRadius: "6px", cursor: "pointer" },
      });
    }
    edges.push({
      id: `e-${e.id}`,
      source: String(assetId),
      target: `child-${e.targetNodeId}`,
      type: "lineage",
      ...edgeVisual(e),
      data: childSpread.get(e)!,
    });
  });

  return { nodes, edges, isEmpty: false };
}
