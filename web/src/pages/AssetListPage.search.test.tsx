import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";
import AssetListPage from "./AssetListPage";
import { api } from "../api/client";
import type { Asset, AttributionUser, ListUsersParams } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    me: vi.fn(),
    listUsers: vi.fn(),
    listAssets: vi.fn(),
    getAssetFacets: vi.fn(),
    listStorageLocations: vi.fn(),
    thumbnailUrl: vi.fn((id: number) => `/api/v1/assets/${id}/thumbnail`),
    deleteAsset: vi.fn(),
    restoreAsset: vi.fn(),
  },
  ApiError: class ApiError extends Error {
    status: number;
    constructor(status: number, message: string) {
      super(message);
      this.status = status;
    }
  },
}));

function renderAt(path: string) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={[path]}>
        <AssetListPage />
      </MemoryRouter>
    </QueryClientProvider>
  );
}

function asset(overrides: Partial<Asset>): Asset {
  return {
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
    thumbState: "READY",
    ...overrides,
  };
}

function user(id: number, username: string): AttributionUser {
  return { id, username, authProvider: "local", externalUid: "", createdAt: 0, lastSeenAt: 0 };
}

beforeEach(() => {
  vi.clearAllMocks();
  vi.mocked(api.me).mockResolvedValue({ kind: "user", authenticated: true, isAdmin: true });
  vi.mocked(api.getAssetFacets).mockResolvedValue({ cameraModels: [] });
  vi.mocked(api.listStorageLocations).mockResolvedValue({ locations: [] });
  vi.mocked(api.listAssets).mockResolvedValue({ assets: [asset({})], total: 1 });
  vi.mocked(api.listUsers).mockResolvedValue({ users: [], total: 0 });
});

describe("AssetListPage search and sort", () => {
  it("sends the search box to the server as q (debounced) and resets to page 1", async () => {
    renderAt("/assets?page=3");
    await screen.findByText("/scratch/photo1.jpg");

    await userEvent.type(screen.getByRole("searchbox", { name: "Search" }), "dsc");

    await waitFor(() =>
      expect(api.listAssets).toHaveBeenLastCalledWith(expect.objectContaining({ q: "dsc", offset: 0 }))
    );
  });

  it("commits the search immediately on Enter", async () => {
    renderAt("/assets");
    await screen.findByText("/scratch/photo1.jpg");

    await userEvent.type(screen.getByRole("searchbox", { name: "Search" }), "IMG{Enter}");
    await waitFor(() => expect(api.listAssets).toHaveBeenLastCalledWith(expect.objectContaining({ q: "IMG" })));
  });

  it("sends the chosen sort to the server", async () => {
    renderAt("/assets");
    await screen.findByText("/scratch/photo1.jpg");

    await userEvent.selectOptions(screen.getByLabelText("Sort"), "name_asc");
    await waitFor(() =>
      expect(api.listAssets).toHaveBeenLastCalledWith(expect.objectContaining({ sort: "name_asc", offset: 0 }))
    );
  });

  it("restores q and sort from the URL", async () => {
    renderAt("/assets?q=clip&sort=captured_desc");
    await screen.findByText("/scratch/photo1.jpg");
    expect(api.listAssets).toHaveBeenCalledWith(expect.objectContaining({ q: "clip", sort: "captured_desc" }));
    expect(screen.getByRole("searchbox", { name: "Search" })).toHaveValue("clip");
  });
});

describe("AssetListPage uploader names", () => {
  it("resolves uploader names for the assets on the current page, not just the first page of users", async () => {
    vi.mocked(api.listAssets).mockResolvedValue({
      assets: [
        asset({ id: 51, filePath: "/scratch/a.jpg", uploadedByUserId: 77 }),
        asset({ id: 52, filePath: "/scratch/b.jpg", uploadedByUserId: 3 }),
      ],
      total: 60,
    });
    // Stand-in for /api/v1/users: without ids it would return only the
    // first page of users, which holds neither uploader.
    vi.mocked(api.listUsers).mockImplementation(async (params: ListUsersParams = {}) => {
      const all = [user(3, "carol"), user(77, "zed")];
      const users = params.ids ? all.filter((u) => params.ids!.includes(u.id)) : [];
      return { users, total: users.length };
    });

    renderAt("/assets?page=2");

    expect(await screen.findByText("zed")).toBeInTheDocument();
    expect(screen.getByText("carol")).toBeInTheDocument();
    expect(api.listUsers).toHaveBeenCalledWith({ ids: [3, 77], limit: 2 });
  });
});
