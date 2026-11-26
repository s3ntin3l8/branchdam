import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import AssetTagsPanel from "./AssetTagsPanel";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    listAssetTags: vi.fn(),
    listAssetAnnotations: vi.fn(),
    addAssetTag: vi.fn(),
    deleteAssetTag: vi.fn(),
  },
}));

function renderPanel() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <AssetTagsPanel assetId={7} />
    </QueryClientProvider>,
  );
}

describe("AssetTagsPanel", () => {
  it("shows an AI badge and lets a user add and remove a tag", async () => {
    vi.mocked(api.listAssetTags).mockResolvedValue({
      tags: [
        { id: 1, nodeId: 7, tag: "night", source: "assistant", createdByUserId: 2, createdAt: 1 },
        { id: 2, nodeId: 7, tag: "kept", source: "user", createdByUserId: 3, createdAt: 2 },
      ],
    });
    vi.mocked(api.listAssetAnnotations).mockResolvedValue({
      annotations: [
        {
          id: 4,
          nodeId: 7,
          kind: "description",
          text: "a dark pier",
          source: "assistant",
          createdByUserId: 2,
          createdAt: 1,
          updatedAt: 1,
        },
        {
          id: 5,
          nodeId: 7,
          kind: "description",
          text: "my note",
          source: "user",
          createdByUserId: 3,
          createdAt: 2,
          updatedAt: 2,
        },
      ],
    });
    vi.mocked(api.addAssetTag).mockResolvedValue({
      id: 3, nodeId: 7, tag: "dawn", source: "user", createdByUserId: 3, createdAt: 3,
    });
    vi.mocked(api.deleteAssetTag).mockResolvedValue({ ok: true });

    renderPanel();
    expect(await screen.findByText("night")).toBeInTheDocument();
    expect(screen.getByText("a dark pier")).toBeInTheDocument();
    expect(screen.getByText("my note")).toBeInTheDocument();
    expect(screen.getAllByText("AI")).toHaveLength(2);

    await userEvent.type(screen.getByLabelText("Add tag"), "dawn");
    await userEvent.click(screen.getByRole("button", { name: "Add tag" }));
    expect(api.addAssetTag).toHaveBeenCalledWith(7, "dawn");

    await userEvent.click(screen.getByRole("button", { name: "Remove tag kept" }));
    expect(api.deleteAssetTag).toHaveBeenCalledWith(7, "kept");
  });
});
