import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import CompanionPairingsPage from "./CompanionPairingsPage";
import type {
  CompanionPairingDetail,
  CompanionPairingListItem,
  CreateCompanionPairingRequest,
  CreateCompanionPairingResponse,
  ListPairingsResponse,
  PairingCredentialsResponse,
  RenameCompanionPairingRequest,
  RenameCompanionPairingResponse,
  RotateCompanionPairingRequest,
  RotateCompanionPairingResponse,
} from "../api/types";

// Mock the api client at the module level. The pairing service tests
// (server-side Go) cover the data flow; this just verifies the SPA
// renders list rows and reacts to API responses correctly.
const listPairingsMock = vi.fn<() => Promise<ListPairingsResponse>>();
const createPairingMock = vi.fn<
  (input: CreateCompanionPairingRequest) => Promise<CreateCompanionPairingResponse>
>();
const rotatePairingMock = vi.fn<
  (id: number, input: RotateCompanionPairingRequest) => Promise<RotateCompanionPairingResponse>
>();
const renamePairingMock = vi.fn<
  (id: number, input: RenameCompanionPairingRequest) => Promise<RenameCompanionPairingResponse>
>();
const pairingCredentialsMock = vi.fn<(id: number) => Promise<PairingCredentialsResponse>>();
const revokePairingMock = vi.fn<(id: number) => Promise<{ revokedAtUnix: number }>>();
const deletePairingMock = vi.fn<(id: number) => Promise<{ ok: boolean }>>();

vi.mock("../api/client", () => ({
  api: {
    listPairings: () => listPairingsMock(),
    pairingQRSVGUrl: (id: number) => `/api/v1/companion/pairings/${id}/qr.svg`,
    revokePairing: (id: number) => revokePairingMock(id),
    deletePairing: (id: number) => deletePairingMock(id),
    rotatePairing: (id: number, input: RotateCompanionPairingRequest) =>
      rotatePairingMock(id, input),
    createPairing: (input: CreateCompanionPairingRequest) => createPairingMock(input),
    renamePairing: (id: number, input: RenameCompanionPairingRequest) =>
      renamePairingMock(id, input),
    pairingCredentials: (id: number) => pairingCredentialsMock(id),
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
  createPairingMock.mockReset();
  rotatePairingMock.mockReset();
  renamePairingMock.mockReset();
  pairingCredentialsMock.mockReset();
  revokePairingMock.mockReset();
  deletePairingMock.mockReset();
});

function renderPage() {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
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

  it("displays Pairing URL, Copy URL button, and deep link when a new pairing is created", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [], total: 0 });
    const pairingUrl = "branchdam://?server=https%3A%2F%2Fdam.example.com&key=testkey123456789012345678901234&agent=dev-abc12345";
    createPairingMock.mockResolvedValue({
      pairingId: 10,
      agentId: "dev-abc12345",
      apiKey: "testkey123456789012345678901234",
      keyPreview: "1234",
      pairingUrl,
      qrSvg: "<svg></svg>",
      createdAtUnix: 1700000000,
    });

    renderPage();
    await waitFor(() => screen.getByRole("button", { name: /pair new device/i }));
    screen.getByRole("button", { name: /pair new device/i }).click();

    const input = await screen.findByPlaceholderText(/Björn's iPhone 16 Pro/i);
    fireEvent.change(input, { target: { value: "My Test Device" } });
    screen.getByRole("button", { name: /create pairing/i }).click();

    await waitFor(() => {
      expect(screen.getByText("Pairing URL")).toBeInTheDocument();
      expect(screen.getByText(pairingUrl)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /copy url/i })).toBeInTheDocument();
      const deepLink = screen.getByRole("link", { name: /pair with local agent/i });
      expect(deepLink).toHaveAttribute("href", pairingUrl);
    });
  });

  it("displays Pairing URL, Copy URL button, and deep link when a key is rotated", async () => {
    listPairingsMock.mockResolvedValue({
      pairings: [samplePairing],
      total: 1,
    });
    const pairingUrl = "branchdam://?server=https%3A%2F%2Fdam.example.com&key=newkey12345678901234567890123456&agent=dev-abc12345";
    rotatePairingMock.mockResolvedValue({
      keyId: 99,
      apiKey: "newkey12345678901234567890123456",
      keyPreview: "3456",
      pairingUrl,
      qrSvg: "<svg></svg>",
      previousKeyExpiresAtUnix: 1700086400,
    });

    renderPage();
    await waitFor(() => screen.getByText("Björn's iPhone"));
    screen.getByRole("button", { name: /^Rotate$/ }).click();

    await waitFor(() => screen.getByRole("heading", { name: /rotate api key/i }));
    screen.getByRole("button", { name: /^Rotate key$/ }).click();

    await waitFor(() => {
      expect(screen.getByText("Pairing URL")).toBeInTheDocument();
      expect(screen.getByText(pairingUrl)).toBeInTheDocument();
      expect(screen.getByRole("button", { name: /copy url/i })).toBeInTheDocument();
      const deepLink = screen.getByRole("link", { name: /pair with local agent/i });
      expect(deepLink).toHaveAttribute("href", pairingUrl);
    });
  });

  it("re-fetches and shows credentials for an existing pairing", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [samplePairing], total: 1 });
    const pairingUrl = "branchdam://?server=https%3A%2F%2Fdam.example.com&key=existingkey123456789012345678901234&agent=dev-abc12345";
    pairingCredentialsMock.mockResolvedValue({
      pairingId: 1,
      agentId: "dev-abc12345",
      friendlyLabel: "Björn's iPhone",
      apiKey: "existingkey123456789012345678901234",
      keyPreview: "1234",
      pairingUrl,
      qrSvg: "<svg></svg>",
    });

    renderPage();
    await waitFor(() => screen.getByText("Björn's iPhone"));
    screen.getByRole("button", { name: /show credentials/i }).click();

    await waitFor(() => {
      expect(pairingCredentialsMock).toHaveBeenCalledWith(1);
      expect(screen.getByText("Pairing URL")).toBeInTheDocument();
      expect(screen.getByText(pairingUrl)).toBeInTheDocument();
      expect(screen.getByRole("link", { name: /pair with local agent/i })).toHaveAttribute(
        "href",
        pairingUrl
      );
    });
    // Reveal path must not claim show-once (Hermes: false on re-display).
    expect(screen.queryByText(/will not be shown again/i)).not.toBeInTheDocument();
    expect(screen.getByText(/recorded in the audit log/i)).toBeInTheDocument();
  });

  it("keeps the show-once warning on the create-success credentials body", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [], total: 0 });
    createPairingMock.mockResolvedValue({
      pairingId: 10,
      agentId: "dev-abc12345",
      apiKey: "testkey123456789012345678901234",
      keyPreview: "1234",
      pairingUrl: "branchdam://?server=https%3A%2F%2Fdam.example.com&key=testkey123456789012345678901234&agent=dev-abc12345",
      qrSvg: "<svg></svg>",
      createdAtUnix: 1700000000,
    });

    renderPage();
    await waitFor(() => screen.getByRole("button", { name: /pair new device/i }));
    screen.getByRole("button", { name: /pair new device/i }).click();
    const input = await screen.findByPlaceholderText(/Björn's iPhone 16 Pro/i);
    fireEvent.change(input, { target: { value: "My Test Device" } });
    screen.getByRole("button", { name: /create pairing/i }).click();

    await waitFor(() => {
      expect(screen.getByText(/will not be shown again/i)).toBeInTheDocument();
    });
  });

  it("ignores a stale credentials response when switching pairings mid-flight", async () => {
    const second: CompanionPairingListItem = {
      id: 2,
      agentId: "dev-second99",
      friendlyLabel: "Second phone",
      createdAtUnix: 1700000000,
      createdBy: "user:admin",
      activeKeyCount: 1,
    };
    listPairingsMock.mockResolvedValue({ pairings: [samplePairing, second], total: 2 });

    let resolveFirst!: (v: PairingCredentialsResponse) => void;
    const firstPending = new Promise<PairingCredentialsResponse>((resolve) => {
      resolveFirst = resolve;
    });
    pairingCredentialsMock.mockImplementation((id: number) => {
      if (id === 1) return firstPending;
      return Promise.resolve({
        pairingId: 2,
        agentId: "dev-second99",
        friendlyLabel: "Second phone",
        apiKey: "second-key-aaaaaaaaaaaaaaaaaaaa",
        keyPreview: "aaaa",
        pairingUrl: "branchdam://?key=second-key-aaaaaaaaaaaaaaaaaaaa&agent=dev-second99",
        qrSvg: "<svg></svg>",
      });
    });

    renderPage();
    await waitFor(() => screen.getByText("Björn's iPhone"));

    // Open A (pending), then B before A resolves.
    fireEvent.click(screen.getAllByRole("button", { name: /show credentials/i })[0]);
    await waitFor(() => expect(pairingCredentialsMock).toHaveBeenCalledWith(1));
    fireEvent.click(screen.getAllByRole("button", { name: /show credentials/i })[1]);
    await waitFor(() => expect(pairingCredentialsMock).toHaveBeenCalledWith(2));
    await waitFor(() => {
      expect(screen.getByRole("heading", { name: /Second phone/i })).toBeInTheDocument();
      expect(screen.getByText("second-key-aaaaaaaaaaaaaaaaaaaa")).toBeInTheDocument();
    });

    // Late A response must not overwrite B's modal.
    resolveFirst({
      pairingId: 1,
      agentId: "dev-abc12345",
      friendlyLabel: "Björn's iPhone",
      apiKey: "first-key-zzzzzzzzzzzzzzzzzzzzzz",
      keyPreview: "zzzz",
      pairingUrl: "branchdam://?key=first-key-zzzzzzzzzzzzzzzzzzzzzz&agent=dev-abc12345",
      qrSvg: "<svg></svg>",
    });
    // Give the microtask queue a turn to flush the stale resolve.
    await new Promise((r) => setTimeout(r, 0));
    expect(screen.queryByText("first-key-zzzzzzzzzzzzzzzzzzzzzz")).not.toBeInTheDocument();
    expect(screen.getByText("second-key-aaaaaaaaaaaaaaaaaaaa")).toBeInTheDocument();
    expect(screen.getByRole("heading", { name: /Second phone/i })).toBeInTheDocument();
  });

  it("renames a pairing via the pencil next to the label", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [samplePairing], total: 1 });
    renamePairingMock.mockResolvedValue({
      id: 1,
      agentId: "dev-abc12345",
      friendlyLabel: "Renamed iPhone",
      createdAtUnix: 1700000000,
      createdBy: "user:admin",
    });

    renderPage();
    await waitFor(() => screen.getByText("Björn's iPhone"));
    fireEvent.click(screen.getByRole("button", { name: /rename björn's iphone/i }));

    await waitFor(() => screen.getByRole("heading", { name: /rename pairing/i }));
    const input = screen.getByPlaceholderText(/Björn's iPhone 16 Pro/i);
    fireEvent.change(input, { target: { value: "Renamed iPhone" } });
    fireEvent.click(screen.getByRole("button", { name: /save label/i }));

    await waitFor(() => {
      expect(renamePairingMock).toHaveBeenCalledWith(1, {
        friendlyLabel: "Renamed iPhone",
      });
    });
    await waitFor(() => {
      expect(screen.queryByRole("heading", { name: /rename pairing/i })).not.toBeInTheDocument();
    });
  });

  it("opens ConfirmDialog for revoke instead of window.confirm", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [samplePairing], total: 1 });
    revokePairingMock.mockResolvedValue({ revokedAtUnix: 1700002000 });

    renderPage();
    await waitFor(() => screen.getByText("Björn's iPhone"));
    fireEvent.click(screen.getByRole("button", { name: /^Revoke$/ }));

    await waitFor(() => {
      expect(screen.getByRole("dialog", { name: /revoke pairing/i })).toBeInTheDocument();
    });
    // The old window.confirm prompt must not appear in the dialog body.
    expect(screen.getByText(/All its API keys will stop working immediately/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /confirm revoke/i }));
    await waitFor(() => {
      expect(revokePairingMock).toHaveBeenCalledWith(1);
    });
    await waitFor(() => {
      expect(screen.queryByRole("dialog", { name: /revoke pairing/i })).not.toBeInTheDocument();
    });
  });

  it("opens ConfirmDialog for delete instead of window.confirm", async () => {
    listPairingsMock.mockResolvedValue({
      pairings: [{ ...samplePairing, revokedAtUnix: 1700001000 }],
      total: 1,
    });
    deletePairingMock.mockResolvedValue({ ok: true });

    renderPage();
    await waitFor(() => screen.getByText("Björn's iPhone"));
    fireEvent.click(screen.getByRole("button", { name: /^Delete$/ }));

    await waitFor(() => {
      expect(screen.getByRole("dialog", { name: /delete pairing/i })).toBeInTheDocument();
    });
    expect(screen.getByText(/This cannot be undone/i)).toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: /confirm delete/i }));
    await waitFor(() => {
      expect(deletePairingMock).toHaveBeenCalledWith(1);
    });
    await waitFor(() => {
      expect(screen.queryByRole("dialog", { name: /delete pairing/i })).not.toBeInTheDocument();
    });
  });
});

// Reference the sample detail so TypeScript doesn't drop the
// companion-pairing detail type from the import -- the production
// page reads CompanionPairingDetail via the api mock, and the test
// file doesn't reference it directly. Keeping an unused import
// would have flagged a lint error.
void sampleDetail;
