import { describe, expect, it } from "vitest";
import { render, screen } from "@testing-library/react";
import AssetGraphCanvas from "./AssetGraphCanvas";

describe("AssetGraphCanvas", () => {
  it("shows an empty-state message when there are no edges", () => {
    render(<AssetGraphCanvas assetId={1} graph={{ parents: [], children: [] }} />);
    expect(screen.getByText(/no known lineage edges/i)).toBeInTheDocument();
  });

  it("renders the flow canvas when edges are present", () => {
    render(
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
      />,
    );
    expect(screen.queryByText(/no known lineage edges/i)).not.toBeInTheDocument();
  });
});
