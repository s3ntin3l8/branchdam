import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import AuditQueuePage from "./AuditQueuePage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    me: vi.fn().mockResolvedValue({ kind: "user", name: "admin" }),
    listAuditQueue: vi.fn(),
    confirmEdge: vi.fn(),
    rejectEdge: vi.fn(),
    createEdge: vi.fn(),
    thumbnailUrl: vi.fn((id: number) => `/api/v1/assets/${id}/thumbnail`),
  },
}));

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>);
}

describe("AuditQueuePage", () => {
  it("shows an empty state when there is nothing to review", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [], total: 0 });
    renderWithClient(<AuditQueuePage />);

    expect(await screen.findByText(/nothing needs review/i)).toBeInTheDocument();
  });

  it("renders queue entries with side-by-side nodes and flags a missing parent", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 1,
          sourceNodeId: 10,
          targetNodeId: 20,
          relationshipType: "PROXY_OF",
          confidence: 0.6,
          tier: 3,
          resolver: "filename_stem",
          evidenceJson: "{}",
          parentAlive: false,
          parentMissing: true,
          sourceNode: {
            id: 10,
            nodeUuid: "uuid-10",
            fileName: "RAW_001.ARW",
            filePath: "/storage/RAW_001.ARW",
            thumbState: "READY",
          },
          targetNode: {
            id: 20,
            nodeUuid: "uuid-20",
            fileName: "EXPORT_001.JPG",
            filePath: "/storage/EXPORT_001.JPG",
            thumbState: "READY",
          },
          captureDeltaSeconds: 5,
          phashDistance: 2,
        },
      ],
      total: 1,
    });
    renderWithClient(<AuditQueuePage />);

    expect(await screen.findByText("PROXY_OF")).toBeInTheDocument();
    expect(screen.getByText(/parent missing/i)).toBeInTheDocument();
    expect(screen.getByText("RAW_001.ARW")).toBeInTheDocument();
    expect(screen.getByText("EXPORT_001.JPG")).toBeInTheDocument();
    expect(screen.getByText("5s")).toBeInTheDocument();
    expect(screen.getByText("2 bits")).toBeInTheDocument();
  });

  it("calls confirmEdge when Confirm is clicked", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 42,
          sourceNodeId: 1,
          targetNodeId: 2,
          relationshipType: "DERIVED_FROM",
          confidence: 0.75,
          tier: 2,
          resolver: "filename_stem",
          evidenceJson: "{}",
          parentAlive: true,
          parentMissing: false,
          sourceNode: { id: 1, nodeUuid: "uuid-1", fileName: "src.arw", filePath: "/src.arw", thumbState: "READY" },
          targetNode: { id: 2, nodeUuid: "uuid-2", fileName: "tgt.jpg", filePath: "/tgt.jpg", thumbState: "READY" },
        },
      ],
      total: 1,
    });
    vi.mocked(api.confirmEdge).mockResolvedValue({ ok: true });

    renderWithClient(<AuditQueuePage />);
    const button = await screen.findByRole("button", { name: /confirm/i });
    await userEvent.click(button);

    await waitFor(() => expect(api.confirmEdge).toHaveBeenCalledWith(42));
  });

  it("opens manual link modal and submits createEdge", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({ entries: [], total: 0 });
    vi.mocked(api.createEdge).mockResolvedValue({
      id: 99,
      sourceNodeId: 100,
      targetNodeId: 200,
      relationshipType: "DERIVED_FROM",
      confidence: 1.0,
      reviewState: "CONFIRMED",
      resolver: "manual",
    });

    renderWithClient(<AuditQueuePage />);
    const manualBtn = await screen.findByRole("button", { name: /\+ manual link edge/i });
    await userEvent.click(manualBtn);

    expect(screen.getByText("Manual Link Edge")).toBeInTheDocument();

    const inputs = screen.getAllByRole("spinbutton");
    await userEvent.type(inputs[0], "100");
    await userEvent.type(inputs[1], "200");

    const submitBtn = screen.getByRole("button", { name: /create link/i });
    await userEvent.click(submitBtn);

    await waitFor(() =>
      expect(api.createEdge).toHaveBeenCalledWith({
        sourceNodeId: 100,
        targetNodeId: 200,
        relationshipType: "DERIVED_FROM",
      })
    );
  });

  it("surfaces error message when confirmEdge fails", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 42,
          sourceNodeId: 1,
          targetNodeId: 2,
          relationshipType: "DERIVED_FROM",
          confidence: 0.75,
          tier: 2,
          resolver: "filename_stem",
          evidenceJson: "{}",
          parentAlive: true,
          parentMissing: false,
          sourceNode: { id: 1, nodeUuid: "uuid-1", fileName: "s.arw", filePath: "/s.arw", thumbState: "READY" },
          targetNode: { id: 2, nodeUuid: "uuid-2", fileName: "t.jpg", filePath: "/t.jpg", thumbState: "READY" },
        },
      ],
      total: 1,
    });
    vi.mocked(api.confirmEdge).mockRejectedValue(new Error("Database write failure"));

    renderWithClient(<AuditQueuePage />);
    const button = await screen.findByRole("button", { name: /confirm/i });
    await userEvent.click(button);

    expect(await screen.findByText(/Failed to confirm edge: Error: Database write failure/i)).toBeInTheDocument();
  });

  it("surfaces error message when rejectEdge fails", async () => {
    vi.mocked(api.listAuditQueue).mockResolvedValue({
      entries: [
        {
          id: 43,
          sourceNodeId: 1,
          targetNodeId: 3,
          relationshipType: "DERIVED_FROM",
          confidence: 0.75,
          tier: 2,
          resolver: "filename_stem",
          evidenceJson: "{}",
          parentAlive: true,
          parentMissing: false,
          sourceNode: { id: 1, nodeUuid: "uuid-1", fileName: "s.arw", filePath: "/s.arw", thumbState: "READY" },
          targetNode: { id: 3, nodeUuid: "uuid-3", fileName: "t.jpg", filePath: "/t.jpg", thumbState: "READY" },
        },
      ],
      total: 1,
    });
    vi.mocked(api.rejectEdge).mockRejectedValue(new Error("Network disconnect"));

    renderWithClient(<AuditQueuePage />);
    const button = await screen.findByRole("button", { name: /reject/i });
    await userEvent.click(button);

    expect(await screen.findByText(/Failed to reject edge: Error: Network disconnect/i)).toBeInTheDocument();
  });
});
