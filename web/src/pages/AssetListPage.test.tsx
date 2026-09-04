import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter } from "react-router";
import { describe, expect, it, vi } from "vitest";
import AssetListPage from "./AssetListPage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    listAssets: vi.fn(),
    getAssetFacets: vi.fn(),
    listStorageLocations: vi.fn(),
    thumbnailUrl: vi.fn((id: number) => `/api/v1/assets/${id}/thumbnail`),
  },
}));

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter>{ui}</MemoryRouter>
    </QueryClientProvider>
  );
}

describe("AssetListPage", () => {
  it("renders asset list, filters panel, and pagination", async () => {
    vi.mocked(api.getAssetFacets).mockResolvedValueOnce({ cameraModels: ["Sony A7IV", "Canon R5"] });
    vi.mocked(api.listStorageLocations).mockResolvedValueOnce({ locations: [] });
    vi.mocked(api.listAssets).mockResolvedValueOnce({
      assets: [
        {
          id: 1,
          nodeUuid: "uuid-1",
          filePath: "/scratch/photo1.jpg",
          fileName: "photo1.jpg",
          fileExt: ".jpg",
          sizeBytes: 2048,
          indexingStatus: "INDEXED_FULL",
          graphStatus: "UNLINKED",
          lifecycleState: "ACTIVE",
          storageLocationId: 1,
          cameraModel: "Sony A7IV",
          thumbState: "READY",
        },
      ],
      total: 1,
    });

    renderWithClient(<AssetListPage />);

    await waitFor(() => {
      expect(screen.getByText("/scratch/photo1.jpg")).toBeInTheDocument();
    });

    expect(screen.getAllByText("Sony A7IV").length).toBeGreaterThan(0);
    expect(screen.getByText("Unlinked Only")).toBeInTheDocument();
    expect(screen.getByText("Showing 1 to 1 of 1 items")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Previous page" })).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Next page" })).toBeInTheDocument();
  });
});
