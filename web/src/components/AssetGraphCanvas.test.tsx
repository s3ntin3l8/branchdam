import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { ReactFlowProvider } from "@xyflow/react";
import AssetGraphCanvas from "./AssetGraphCanvas";

describe("AssetGraphCanvas", () => {
  it("shows an empty-state message when there are no edges", () => {
    render(
      <MemoryRouter>
        <ReactFlowProvider>
          <AssetGraphCanvas assetId={1} graph={{ parents: [], children: [] }} />
        </ReactFlowProvider>
      </MemoryRouter>
    );
    expect(screen.getByText(/no known lineage edges/i)).toBeInTheDocument();
  });

  it("renders the flow canvas when edges are present", () => {
    render(
      <MemoryRouter>
        <ReactFlowProvider>
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
        </ReactFlowProvider>
      </MemoryRouter>
    );
    expect(screen.queryByText(/no known lineage edges/i)).not.toBeInTheDocument();
  });

  it("renders multi-hop lineage data when lineage prop is provided", () => {
    render(
      <MemoryRouter>
        <ReactFlowProvider>
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
        </ReactFlowProvider>
      </MemoryRouter>
    );
    expect(screen.queryByText(/no known lineage edges/i)).not.toBeInTheDocument();
  });
});
