import { render, screen, within } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import AssetMediaPreview from "./AssetMediaPreview";
import type { Asset, LineageResponse } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    thumbnailUrl: vi.fn((id: number) => `/api/v1/assets/${id}/thumbnail`),
    streamUrl: vi.fn((id: number) => `/api/v1/assets/${id}/stream`),
  },
}));

function asset(overrides: Partial<Asset> = {}): Asset {
  return {
    id: 7,
    nodeUuid: "uuid-7",
    filePath: "/archive/photo.jpg",
    fileName: "photo.jpg",
    fileExt: "jpg",
    sizeBytes: 40 * 1024 * 1024,
    indexingStatus: "INDEXED_FULL",
    graphStatus: "LINKED",
    lifecycleState: "ACTIVE",
    storageLocationId: 1,
    thumbState: "READY",
    ...overrides,
  };
}

describe("AssetMediaPreview", () => {
  it("previews an image from its thumbnail and streams the master only in the lightbox", async () => {
    render(<AssetMediaPreview asset={asset()} />);

    const preview = screen.getByTestId("asset-image-preview");
    expect(within(preview).getByRole("img", { name: "photo.jpg" })).toHaveAttribute(
      "src",
      "/api/v1/assets/7/thumbnail",
    );
    expect(document.querySelector('img[src="/api/v1/assets/7/stream"]')).toBeNull();

    await userEvent.click(screen.getByRole("button", { name: /view full resolution/i }));
    const lightbox = screen.getByRole("dialog", { name: "photo.jpg" });
    expect(within(lightbox).getByRole("img", { name: "photo.jpg" })).toHaveAttribute(
      "src",
      "/api/v1/assets/7/stream",
    );
    expect(screen.getByTestId("lightbox-close-button")).toHaveFocus();

    await userEvent.keyboard("{Escape}");
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("shows the thumbnail placeholder, not the master, while an image thumbnail is pending", () => {
    render(<AssetMediaPreview asset={asset({ thumbState: "PENDING" })} />);
    expect(screen.getByRole("status", { name: "Thumbnail generating" })).toBeInTheDocument();
    expect(document.querySelector('img[src="/api/v1/assets/7/stream"]')).toBeNull();
  });

  it("plays video from the stream with the thumbnail as poster", () => {
    render(<AssetMediaPreview asset={asset({ fileName: "clip.mp4", fileExt: "mp4" })} />);
    const video = screen.getByTestId("asset-video-player");
    expect(video).toHaveAttribute("poster", "/api/v1/assets/7/thumbnail");
    expect(video.querySelector("source")).toHaveAttribute("src", "/api/v1/assets/7/stream");
    expect(video.querySelector("source")).toHaveAttribute("type", "video/mp4");
  });

  it("plays audio from the stream", () => {
    render(<AssetMediaPreview asset={asset({ fileName: "take.wav", fileExt: "wav" })} />);
    expect(screen.getByTestId("asset-audio-player")).toHaveAttribute("src", "/api/v1/assets/7/stream");
  });

  it("falls back to the generated thumbnail for RAW and other formats", () => {
    render(<AssetMediaPreview asset={asset({ fileName: "DSC0001.ARW", fileExt: "ARW" })} />);
    expect(screen.getByRole("img", { name: "DSC0001.ARW" })).toHaveAttribute("src", "/api/v1/assets/7/thumbnail");
    expect(screen.getByText("Preview (Generated Thumbnail)")).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: /view full resolution/i })).not.toBeInTheDocument();
  });

  it("switches between an original and its proxy", async () => {
    const original = asset({ id: 1, fileName: "clip.mov", fileExt: "mov" });
    const proxy = asset({ id: 2, fileName: "clip_proxy.mp4", fileExt: "mp4" });
    const lineage: LineageResponse = {
      rootId: 1,
      nodes: [original, proxy],
      edges: [
        {
          id: 9,
          sourceNodeId: 1,
          targetNodeId: 2,
          relationshipType: "PROXY_OF",
          confidence: 1,
          reviewState: "CONFIRMED",
          resolver: "test",
        },
      ],
    };
    render(<AssetMediaPreview asset={original} lineage={lineage} />);

    const source = () => screen.getByTestId("asset-video-player").querySelector("source");
    expect(source()).toHaveAttribute("src", "/api/v1/assets/1/stream");

    await userEvent.click(screen.getByRole("button", { name: /proxy \(mp4\)/i }));
    expect(source()).toHaveAttribute("src", "/api/v1/assets/2/stream");
    expect(screen.getByText(/viewing linked proxy: clip_proxy\.mp4/i)).toBeInTheDocument();
  });
});
