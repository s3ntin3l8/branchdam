import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import Thumbnail from "./Thumbnail";

vi.mock("../api/client", () => ({
  api: {
    thumbnailUrl: vi.fn((id: number) => `/api/v1/assets/${id}/thumbnail`),
  },
}));

describe("Thumbnail", () => {
  it("renders an img pointed at the thumbnail route when READY", () => {
    render(<Thumbnail assetId={7} thumbState="READY" alt="photo.jpg" />);
    const img = screen.getByAltText("photo.jpg");
    expect(img.tagName).toBe("IMG");
    expect(img).toHaveAttribute("src", "/api/v1/assets/7/thumbnail");
  });

  it("renders a skeleton, not an img, when PENDING", () => {
    render(<Thumbnail assetId={7} thumbState="PENDING" alt="photo.jpg" />);
    expect(screen.queryByRole("img")).not.toBeInTheDocument();
    expect(screen.getByRole("status", { name: "Thumbnail generating" })).toBeInTheDocument();
  });

  it("renders a placeholder, not an img, when UNSUPPORTED", () => {
    render(<Thumbnail assetId={7} thumbState="UNSUPPORTED" alt="photo.jpg" />);
    expect(screen.queryByAltText("photo.jpg")).not.toBeInTheDocument();
    expect(screen.getByLabelText("No thumbnail available")).toBeInTheDocument();
  });

  it("renders the same placeholder when FAILED", () => {
    render(<Thumbnail assetId={7} thumbState="FAILED" alt="photo.jpg" />);
    expect(screen.getByLabelText("No thumbnail available")).toBeInTheDocument();
  });

  it("falls back to placeholder when image fails to load via onError (#357)", () => {
    render(<Thumbnail assetId={7} thumbState="READY" alt="photo.jpg" />);
    const img = screen.getByAltText("photo.jpg");
    expect(img).toBeInTheDocument();

    // Trigger image error
    fireEvent.error(img);

    // Should now show placeholder instead of broken image
    expect(screen.queryByAltText("photo.jpg")).not.toBeInTheDocument();
    expect(screen.getByLabelText("No thumbnail available")).toBeInTheDocument();
  });

  it("resets error state when assetId changes", () => {
    const { rerender } = render(<Thumbnail assetId={7} thumbState="READY" alt="photo.jpg" />);
    const img = screen.getByAltText("photo.jpg");
    fireEvent.error(img);
    expect(screen.getByLabelText("No thumbnail available")).toBeInTheDocument();

    // Rerender with a new assetId
    rerender(<Thumbnail assetId={8} thumbState="READY" alt="photo2.jpg" />);
    const newImg = screen.getByAltText("photo2.jpg");
    expect(newImg).toBeInTheDocument();
    expect(newImg).toHaveAttribute("src", "/api/v1/assets/8/thumbnail");
  });
});
