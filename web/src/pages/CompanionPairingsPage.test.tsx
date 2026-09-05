import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import CompanionPairingsPage from "./CompanionPairingsPage";
import type {
  CompanionPairingDetail,
  CompanionPairingListItem,
  ListPairingsResponse,
} from "../api/types";

// Mock the api client at the module level. The pairing service tests
// (server-side Go) cover the data flow; this just verifies the SPA
// renders list rows and reacts to API responses correctly.
const listPairingsMock = vi.fn<() => Promise<ListPairingsResponse>>();

vi.mock("../api/client", () => ({
  api: {
    listPairings: () => listPairingsMock(),
    pairingQRSVGUrl: (id: number) => `/api/v1/companion/pairings/${id}/qr.svg`,
    revokePairing: vi.fn(),
    rotatePairing: vi.fn(),
    createPairing: vi.fn(),
  },
  ApiError: class ApiError extends Error {
    status: number;
    constructor(status: number, message: string) {
      super(message);
      this.status = status;
      this.name = "ApiError";
    }
  },
}));

const samplePairing: CompanionPairingListItem = {
  id: 1,
  agentId: "dev-abc12345",
  friendlyLabel: "Björn's iPhone",
  createdAtUnix: 1700000000,
  createdBy: "user:admin",
  activeKeyCount: 1,
};

const sampleDetail: CompanionPairingDetail = {
  id: 1,
  agentId: "dev-abc12345",
  friendlyLabel: "Björn's iPhone",
  createdAtUnix: 1700000000,
  createdBy: "user:admin",
  keys: [
    {
      id: 42,
      keyPreview: "WXyZ",
      createdAtUnix: 1700000000,
    },
  ],
  auditTail: [
    { id: 1, actor: "user:admin", event: "PAIR_CREATED", detailsJson: "{}", createdAtUnix: 1700000000 },
    { id: 2, actor: "user:admin", event: "KEY_MINTED", detailsJson: "{}", createdAtUnix: 1700000000 },
  ],
};

beforeEach(() => {
  listPairingsMock.mockReset();
});

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/companion"]}>
        <Routes>
          <Route path="/companion" element={<CompanionPairingsPage />} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>
  );
}

describe("CompanionPairingsPage", () => {
  afterEach(() => {
    vi.clearAllMocks();
  });

  it("renders an empty state when no pairings exist", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [], total: 0 });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/No paired devices yet/i)).toBeInTheDocument();
    });
  });

  it("renders a pairing row from the server response", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [samplePairing], total: 1 });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText("Björn's iPhone")).toBeInTheDocument();
      expect(screen.getByText("dev-abc12345")).toBeInTheDocument();
    });
  });

  it("opens the create modal when the CTA is clicked", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [], total: 0 });
    renderPage();
    await waitFor(() => screen.getByRole("button", { name: /pair new device/i }));
    screen.getByRole("button", { name: /pair new device/i }).click();
    await waitFor(() => {
      expect(screen.getByPlaceholderText(/Björn's iPhone 16 Pro/i)).toBeInTheDocument();
    });
  });

  it("shows a Revoked status for revoked pairings", async () => {
    listPairingsMock.mockResolvedValue({
      pairings: [{ ...samplePairing, revokedAtUnix: 1700001000 }],
      total: 1,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/Revoked/i)).toBeInTheDocument();
    });
  });

  it("disables Rotate and Revoke for revoked pairings", async () => {
    listPairingsMock.mockResolvedValue({
      pairings: [{ ...samplePairing, revokedAtUnix: 1700001000 }],
      total: 1,
    });
    renderPage();
    await waitFor(() => screen.getByText("Björn's iPhone"));
    const rotateButton = screen.getByRole("button", { name: /^Rotate$/ });
    const revokeButton = screen.getByRole("button", { name: /^Revoke$/ });
    expect(rotateButton).toBeDisabled();
    expect(revokeButton).toBeDisabled();
  });
});

// Reference the sample detail so TypeScript doesn't drop the
// companion-pairing detail type from the import -- the production
// page reads CompanionPairingDetail via the api mock, and the test
// file doesn't reference it directly. Keeping an unused import
// would have flagged a lint error.
void sampleDetail;
