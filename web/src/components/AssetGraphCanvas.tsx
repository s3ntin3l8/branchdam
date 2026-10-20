import { useMemo } from "react";
import { useNavigate } from "react-router";
import { Background, Controls, Position, ReactFlow, ReactFlowProvider, type Node as FlowNode } from "@xyflow/react";
import "@xyflow/react/dist/style.css";
import type { AssetGraph, LineageResponse } from "../api/types";
import { edgeVisual, labelSpread, type LineageFlowEdge } from "../lib/edgeStyle";
import { compareStrings, lineageRanks, lineageRows } from "../lib/lineageLayout";
import LineageEdge from "./LineageEdge";

// Columns run left to right, so edges leave a node's right side and enter the
// next one's left; the default top/bottom handles made every edge loop around
// its nodes and park its label on top of them.
const HORIZONTAL_HANDLES = { sourcePosition: Position.Right, targetPosition: Position.Left } as const;
// Wide enough that a label sits between two columns instead of on a node.
const LEVEL_GAP_X = 460;
// Lineage nodes have a fixed width, so a routed edge's waypoints and the
// clearance between a column's nodes and its route slots are computed for the
// real boxes (a content-sized node would make that a guess). Long names wrap.
const NODE_WIDTH = 160;
// Centre of a node from its top-left position. The height is content-driven
// (names wrap), so y is an estimate; a waypoint only has to land level with its
// row, and slots sit a full row from any node.
const NODE_CENTRE = { x: NODE_WIDTH / 2, y: 22 };
// Vertical offset between routed edges that join the same pair of nodes.
const PARALLEL_NUDGE_Y = 8;

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

  // Lay out by file name, not by the order (or ids) the API returns: ids
  // follow ingest order, so two imports of the same files would otherwise draw
  // differently. Edge order decides label spread and parallel nudges too.
  const names = new Map(lineage.nodes.map((n) => [n.id, n.fileName]));
  const nameOf = (id: number) => names.get(id) ?? "";
  const lineageEdges = [...lineage.edges].sort(
    (a, b) =>
      compareStrings(nameOf(a.sourceNodeId), nameOf(b.sourceNodeId)) ||
      compareStrings(nameOf(a.targetNodeId), nameOf(b.targetNodeId)) ||
      compareStrings(a.relationshipType, b.relationshipType) ||
      a.id - b.id,
  );
  const ranks = lineageRanks(lineage.nodes.map((n) => n.id), lineageEdges);
  const { rows, routes } = lineageRows(ranks, lineageEdges, names);

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
        width: NODE_WIDTH,
        overflowWrap: "anywhere",
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
  const spread = spreadLabels(lineageEdges, (e) => String(e.targetNodeId));
  // Edges on one source/target pair (different relationship types) share a
  // route; nudge each routed copy apart so none is drawn exactly over another.
  const pairSize = new Map<string, number>();
  const pairSeen = new Map<string, number>();
  const pairKey = (e: { sourceNodeId: number; targetNodeId: number }) => `${e.sourceNodeId}>${e.targetNodeId}`;
  lineageEdges.forEach((e) => pairSize.set(pairKey(e), (pairSize.get(pairKey(e)) ?? 0) + 1));
  const edges: LineageFlowEdge[] = lineageEdges.map((e, i) => {
    const seen = pairSeen.get(pairKey(e)) ?? 0;
    pairSeen.set(pairKey(e), seen + 1);
    const nudge = (seen - (pairSize.get(pairKey(e))! - 1) / 2) * PARALLEL_NUDGE_Y;
    return {
      id: `e-${e.id}`,
      type: "lineage",
      source: String(e.sourceNodeId),
      target: String(e.targetNodeId),
      ...edgeVisual(e, 2),
      data: {
        ...spread.get(e)!,
        // An edge spanning several columns bends through free slots in the ones
        // between, rather than running behind the nodes there.
        waypoints: routes.get(i)?.map((s) => ({ x: s.rank * LEVEL_GAP_X + NODE_CENTRE.x, y: s.y + NODE_CENTRE.y + nudge })),
        slotHalfWidth: routes.has(i) ? NODE_CENTRE.x : undefined,
      },
    };
  });

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
