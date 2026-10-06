import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import AssetGraphCanvas, { buildOneHopFlow } from "./AssetGraphCanvas";
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
