import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
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
  PairingAuditResponse,
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
const getPairingMock = vi.fn<(id: number) => Promise<CompanionPairingDetail>>();
const pairingAuditMock = vi.fn<
  (id: number, params: { limit?: number; offset?: number }) => Promise<PairingAuditResponse>
>();

vi.mock("../api/client", () => ({
  api: {
    listPairings: () => listPairingsMock(),
    revokePairing: (id: number) => revokePairingMock(id),
    deletePairing: (id: number) => deletePairingMock(id),
    rotatePairing: (id: number, input: RotateCompanionPairingRequest) =>
      rotatePairingMock(id, input),
    createPairing: (input: CreateCompanionPairingRequest) => createPairingMock(input),
    renamePairing: (id: number, input: RenameCompanionPairingRequest) =>
      renamePairingMock(id, input),
    pairingCredentials: (id: number) => pairingCredentialsMock(id),
    getPairing: (id: number) => getPairingMock(id),
    pairingAudit: (id: number, params: { limit?: number; offset?: number }) => pairingAuditMock(id, params),
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
  getPairingMock.mockReset();
  pairingAuditMock.mockReset();
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
      expect(screen.getByText(/if nothing happens, install or update the agent/i)).toBeInTheDocument();
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
      expect(screen.getByText(/if nothing happens, install or update the agent/i)).toBeInTheDocument();
    });
    // Rotate success reuses the same re-openable copy; no show-once claim.
    expect(screen.queryByText(/will not be shown again/i)).not.toBeInTheDocument();
    expect(screen.getByText(/recorded in the audit log/i)).toBeInTheDocument();
  });

  it("renders the QR as an inert data-URL image and never as a non-branchdam link", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [samplePairing], total: 1 });
    pairingCredentialsMock.mockResolvedValue({
      pairingId: 1,
      agentId: "dev-abc12345",
      friendlyLabel: "Björn's iPhone",
      apiKey: "existingkey123456789012345678901234",
      keyPreview: "1234",
      pairingUrl: "javascript:alert(1)",
      qrSvg: '<svg xmlns="http://www.w3.org/2000/svg"><script>window.__pwned=1</script></svg>',
      sealingEnabled: true,
    });

    renderPage();
    await waitFor(() => screen.getByText("Björn's iPhone"));
    screen.getByRole("button", { name: /show credentials/i }).click();

    const img = await screen.findByAltText("Pairing QR code");
    expect(img.getAttribute("src")).toMatch(/^data:image\/svg\+xml/);
    expect(document.querySelector("script")).toBeNull();
    // No href at all for a non-branchdam: URL (an <a> without href is not a link).
    expect(screen.queryByRole("link", { name: /pair with local agent/i })).toBeNull();
    const anchor = document.querySelector('a[title="Opens the branchDAM agent on this computer"]');
    expect(anchor).not.toBeNull();
    expect(anchor).not.toHaveAttribute("href");
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
      sealingEnabled: true,
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

  it("labels empty apiKey as legacy row when sealingEnabled (Hermes r4)", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [samplePairing], total: 1 });
    pairingCredentialsMock.mockResolvedValue({
      pairingId: 1,
      agentId: "dev-abc12345",
      friendlyLabel: "Björn's iPhone",
      apiKey: "",
      keyPreview: "1234",
      pairingUrl: "",
      qrSvg: "<svg></svg>",
      sealingEnabled: true,
    });

    renderPage();
    await waitFor(() => screen.getByText("Björn's iPhone"));
    fireEvent.click(screen.getByRole("button", { name: /show credentials/i }));

    await waitFor(() => {
      expect(screen.getByText("••••1234")).toBeInTheDocument();
      expect(screen.getByText(/Legacy row — rotate to seal/i)).toBeInTheDocument();
    });
    expect(screen.queryByText(/Unavailable \(keyless server\)/i)).not.toBeInTheDocument();
  });

  it("labels empty apiKey as keyless server when !sealingEnabled (Hermes r4)", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [samplePairing], total: 1 });
    pairingCredentialsMock.mockResolvedValue({
      pairingId: 1,
      agentId: "dev-abc12345",
      friendlyLabel: "Björn's iPhone",
      apiKey: "",
      keyPreview: "abcd",
      pairingUrl: "",
      qrSvg: "<svg></svg>",
      sealingEnabled: false,
    });

    renderPage();
    await waitFor(() => screen.getByText("Björn's iPhone"));
    fireEvent.click(screen.getByRole("button", { name: /show credentials/i }));

    await waitFor(() => {
      expect(screen.getByText("••••abcd")).toBeInTheDocument();
      expect(screen.getByText(/Keyless server — QR only/i)).toBeInTheDocument();
    });
    expect(screen.queryByText(/Unavailable \(keyless server\)/i)).not.toBeInTheDocument();
  });

  it("uses re-openable copy on the create-success credentials body (no show-once claim)", async () => {
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
      expect(screen.getByText(/recorded in the audit log/i)).toBeInTheDocument();
    });
    // Show credentials re-serves this key, so show-once would be false (Hermes round 2).
    expect(screen.queryByText(/will not be shown again/i)).not.toBeInTheDocument();
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
        sealingEnabled: true,
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
      sealingEnabled: true,
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

describe("CompanionPairingsPage: pairing details", () => {
  it("shows a pairing's keys and pages through its audit trail", async () => {
    listPairingsMock.mockResolvedValue({ pairings: [samplePairing], total: 1 });
    getPairingMock.mockResolvedValue({
      ...sampleDetail,
      keys: [
        { id: 42, keyPreview: "WXyZ", createdAtUnix: 1700000000 },
        { id: 41, keyPreview: "OLDK", createdAtUnix: 1690000000, expiresAtUnix: 1695000000 },
      ],
    });
    pairingAuditMock.mockImplementation(async (_id, params) => ({
      events:
        (params.offset ?? 0) === 0
          ? [{ id: 9, actor: "user:admin", event: "KEY_ROTATED", detailsJson: '{"keyId":42}', createdAtUnix: 1700000500 }]
          : [{ id: 1, actor: "user:admin", event: "PAIR_CREATED", detailsJson: "{}", createdAtUnix: 1700000000 }],
      total: 21,
    }));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "Details for Björn's iPhone" }));
    const dialog = await screen.findByRole("dialog", { name: "Pairing details: Björn's iPhone" });
    const liveKey = (await within(dialog).findByText("••••WXyZ")).closest("tr")!;
    expect(within(liveKey).getByText("Active")).toBeInTheDocument();
    const oldKey = within(dialog).getByText("••••OLDK").closest("tr")!;
    expect(within(oldKey).getByText("Expired")).toBeInTheDocument();
    expect(getPairingMock).toHaveBeenCalledWith(1);

    expect(await within(dialog).findByText("KEY_ROTATED")).toBeInTheDocument();
    expect(pairingAuditMock).toHaveBeenLastCalledWith(1, { limit: 20, offset: 0 });
    expect(within(dialog).getByText("Page 1 of 2")).toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("button", { name: "Older" }));
    expect(await within(dialog).findByText("PAIR_CREATED")).toBeInTheDocument();
    expect(pairingAuditMock).toHaveBeenLastCalledWith(1, { limit: 20, offset: 20 });

    fireEvent.click(within(dialog).getByRole("button", { name: "Close" }));
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
  });
});

describe("CompanionPairingsPage: rotate grace period", () => {
  async function openRotate() {
    listPairingsMock.mockResolvedValue({ pairings: [samplePairing], total: 1 });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /^Rotate$/ }));
    return screen.findByRole("dialog", { name: /rotate api key/i });
  }

  it.each(["", "0", "10081", "1.5"])("refuses %j and never sends it", async (value) => {
    const dialog = await openRotate();
    const input = within(dialog).getByLabelText(/grace period/i);
    fireEvent.change(input, { target: { value } });
    expect(within(dialog).getByText(/Enter whole minutes from 1 to 10080/)).toBeInTheDocument();
    expect(input).toHaveAttribute("aria-invalid", "true");
    const button = within(dialog).getByRole("button", { name: "Rotate key" });
    expect(button).toBeDisabled();
    fireEvent.click(button);
    expect(rotatePairingMock).not.toHaveBeenCalled();
  });

  it("sends a valid grace period as a number", async () => {
    rotatePairingMock.mockResolvedValue({
      keyId: 43,
      apiKey: "bdam_new",
      keyPreview: "_new",
      pairingUrl: "",
      qrSvg: "",
      previousKeyExpiresAtUnix: 1700003600,
    });
    const dialog = await openRotate();
    fireEvent.change(within(dialog).getByLabelText(/grace period/i), { target: { value: "60" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "Rotate key" }));
    await waitFor(() => expect(rotatePairingMock).toHaveBeenCalledWith(1, { graceMinutes: 60 }));
  });
});
