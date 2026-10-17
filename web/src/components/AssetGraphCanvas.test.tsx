import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import AssetGraphCanvas, { buildLineageFlow, buildOneHopFlow } from "./AssetGraphCanvas";
import { labelSpread } from "../lib/edgeStyle";
import type { Edge } from "../api/types";

describe("AssetGraphCanvas", () => {
  it("shows an empty-state message when there are no edges", () => {
    render(
      <MemoryRouter>
        <AssetGraphCanvas assetId={1} graph={{ parents: [], children: [] }} />
      </MemoryRouter>
    );
    expect(screen.getByText(/no known lineage edges/i)).toBeInTheDocument();
  });

  it("renders the flow canvas when edges are present", () => {
    render(
      <MemoryRouter>
        <AssetGraphCanvas
          assetId={1}
          graph={{
            parents: [
              {
                id: 1,
                sourceNodeId: 2,
                targetNodeId: 1,
                relationshipType: "DERIVED_FROM",
                confidence: 0.95,
                reviewState: "AUTO_ACCEPTED",
                resolver: "xmp_original_document_id",
              },
            ],
            children: [],
          }}
        />
      </MemoryRouter>
    );
    expect(screen.queryByText(/no known lineage edges/i)).not.toBeInTheDocument();
  });

  it("renders multi-hop lineage data when lineage prop is provided", () => {
    render(
      <MemoryRouter>
        <AssetGraphCanvas
          assetId={1}
          lineage={{
            rootId: 1,
            nodes: [
              {
                id: 1,
                nodeUuid: "uuid-1",
                filePath: "/tmp/root.arw",
                fileName: "root.arw",
                fileExt: ".arw",
                sizeBytes: 1000,
                indexingStatus: "INDEXED_FULL",
                graphStatus: "LINKED",
                lifecycleState: "ACTIVE",
                storageLocationId: 1,
                thumbState: "PENDING",
              },
              {
                id: 2,
                nodeUuid: "uuid-2",
                filePath: "/tmp/child.jpg",
                fileName: "child.jpg",
                fileExt: ".jpg",
                sizeBytes: 500,
                indexingStatus: "INDEXED_FULL",
                graphStatus: "LINKED",
                lifecycleState: "ACTIVE",
                storageLocationId: 1,
                thumbState: "PENDING",
              },
            ],
            edges: [
              {
                id: 10,
                sourceNodeId: 1,
                targetNodeId: 2,
                relationshipType: "FINAL_EXPORT",
                confidence: 0.99,
                reviewState: "AUTO_ACCEPTED",
                resolver: "filename_stem",
              },
            ],
          }}
        />
      </MemoryRouter>
    );
    expect(screen.queryByText(/no known lineage edges/i)).not.toBeInTheDocument();
  });
});

describe("buildOneHopFlow", () => {
  const edge = (id: number, relationshipType: Edge["relationshipType"]): Edge => ({
    id,
    sourceNodeId: 2,
    targetNodeId: 1,
    relationshipType,
    confidence: 0.9,
    reviewState: "AUTO_ACCEPTED",
    resolver: "test",
  });

  it("emits unique node ids when two edges join the same pair of nodes", () => {
    const { nodes, edges } = buildOneHopFlow(1, {
      parents: [edge(10, "DERIVED_FROM"), edge(11, "PROXY_OF")],
      children: [],
    });
    const ids = nodes.map((n) => n.id);
    expect(new Set(ids).size).toBe(ids.length);
    // Both edges are still drawn, each with its own id.
    expect(edges.map((e) => e.id)).toEqual(["e-10", "e-11"]);
  });
});

describe("review-state edge styling", () => {
  const edge = (id: number, reviewState: Edge["reviewState"]): Edge => ({
    id,
    sourceNodeId: 2,
    targetNodeId: 1,
    relationshipType: "DERIVED_FROM",
    confidence: 0.6,
    reviewState,
    resolver: "test",
  });

  it("draws a NEEDS_REVIEW edge dashed and labelled, unlike a CONFIRMED one", () => {
    const { edges } = buildOneHopFlow(1, {
      parents: [edge(10, "NEEDS_REVIEW"), edge(11, "CONFIRMED")],
      children: [edge(12, "AUTO_ACCEPTED")],
    });
    const byId = Object.fromEntries(edges.map((e) => [e.id, e]));
    expect(byId["e-10"].style?.strokeDasharray).toBeTruthy();
    expect(String(byId["e-10"].label)).toMatch(/needs review/i);
    expect(byId["e-11"].style?.strokeDasharray).toBeUndefined();
    expect(String(byId["e-11"].label)).not.toMatch(/review/i);
    expect(byId["e-12"].style?.strokeDasharray).toBeUndefined();
    // Same relationship colour either way: only the line pattern differs.
    expect(byId["e-10"].style?.stroke).toBe(byId["e-11"].style?.stroke);
  });

  describe("label placement (#576)", () => {
    const lineageEdge = (id: number, source: number, target: number): Edge => ({
      id,
      sourceNodeId: source,
      targetNodeId: target,
      relationshipType: "PROXY_OF",
      confidence: 0.6,
      reviewState: "NEEDS_REVIEW",
      resolver: "test",
    });
    const node = (id: number) => ({
      id,
      nodeUuid: `uuid-${id}`,
      filePath: `/s/${id}.arw`,
      fileName: `${id}.arw`,
      fileExt: ".arw",
      sizeBytes: 1,
      indexingStatus: "INDEXED_FULL",
      graphStatus: "LINKED",
      lifecycleState: "ACTIVE",
      storageLocationId: 1,
    });

    it("keeps a lone edge's label at the midpoint", () => {
      expect(labelSpread(0, 1)).toEqual({ labelT: 0.5, labelDy: 0 });
    });

    it("gives every edge into one node a distinct label position", () => {
      const { edges } = buildLineageFlow(1, {
        rootId: 1,
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        nodes: [1, 2, 3, 4, 5].map(node) as any,
        edges: [lineageEdge(10, 1, 5), lineageEdge(11, 2, 5), lineageEdge(12, 3, 5), lineageEdge(13, 4, 2)],
      });
      const into5 = edges.filter((e) => e.target === "5").map((e) => e.data!);
      expect(into5).toHaveLength(3);
      // Pairwise different on both axes: no two labels can share a spot.
      for (let i = 0; i < into5.length; i++) {
        for (let j = i + 1; j < into5.length; j++) {
          expect(into5[i].labelT).not.toBe(into5[j].labelT);
          expect(into5[i].labelDy).not.toBe(into5[j].labelDy);
        }
      }
      // The edge into a different node is alone in its group.
      expect(edges.find((e) => e.target === "2")!.data).toEqual({ labelT: 0.5, labelDy: 0 });
    });

    it("routes an edge that skips columns through flow-coordinate waypoints in the columns it crosses", () => {
      // 1 -> 2 -> 3 -> 4 and 1 -> 4: the last edge crosses columns 1 and 2.
      const { edges, nodes } = buildLineageFlow(1, {
        rootId: 1,
        // eslint-disable-next-line @typescript-eslint/no-explicit-any
        nodes: [1, 2, 3, 4].map(node) as any,
        edges: [lineageEdge(10, 1, 2), lineageEdge(11, 2, 3), lineageEdge(12, 3, 4), lineageEdge(13, 1, 4)],
      });
      const waypoints = edges.find((e) => e.id === "e-13")!.data!.waypoints!;
      const nodeX = (id: string) => nodes.find((n) => n.id === id)!.position.x;
      expect(waypoints).toHaveLength(2);
      // Each waypoint sits inside the x-span of the column it crosses.
      expect(waypoints[0].x).toBeGreaterThan(nodeX("2"));
      expect(waypoints[0].x).toBeLessThan(nodeX("3"));
      expect(waypoints[1].x).toBeGreaterThan(nodeX("3"));
      expect(waypoints[1].x).toBeLessThan(nodeX("4"));
      // Edges between adjacent columns are not routed.
      expect(edges.find((e) => e.id === "e-10")!.data!.waypoints).toBeUndefined();
    });

    it("is deterministic for the same input and uses the custom edge type", () => {
      const graph = { parents: [lineageEdge(20, 2, 1), lineageEdge(21, 3, 1)], children: [lineageEdge(22, 1, 4)] };
      const a = buildOneHopFlow(1, graph);
      const b = buildOneHopFlow(1, graph);
      expect(a.edges).toEqual(b.edges);
      expect(a.edges.every((e) => e.type === "lineage")).toBe(true);
      const byId = Object.fromEntries(a.edges.map((e) => [e.id, e.data]));
      expect(byId["e-20"]).not.toEqual(byId["e-21"]);
      expect(byId["e-22"]).toEqual({ labelT: 0.5, labelDy: 0 });
    });
  });
});
