import { render, screen, within } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { beforeEach, describe, expect, it, vi } from "vitest";
import type { CompanionPairingDetail, PairingAuditResponse } from "../api/types";
import PairingDetailDialog from "./PairingDetailDialog";

const getPairingMock = vi.fn<(id: number) => Promise<CompanionPairingDetail>>();
const pairingAuditMock = vi.fn<
  (id: number, params: { limit?: number; offset?: number }) => Promise<PairingAuditResponse>
>();

vi.mock("../api/client", () => ({
  api: {
    getPairing: (id: number) => getPairingMock(id),
    pairingAudit: (id: number, params: { limit?: number; offset?: number }) => pairingAuditMock(id, params),
  },
}));

const detail: CompanionPairingDetail = {
  id: 1,
  agentId: "dev-abc12345",
  friendlyLabel: "Field laptop",
  createdAtUnix: 1700000000,
  createdBy: "user:admin",
  keys: [{ id: 42, keyPreview: "WXyZ", createdAtUnix: 1700000000 }],
  auditTail: [],
};

function renderDialog() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <PairingDetailDialog pairingId={1} onClose={vi.fn()} />
    </QueryClientProvider>,
  );
}

beforeEach(() => {
  getPairingMock.mockReset();
  pairingAuditMock.mockReset();
});

describe("PairingDetailDialog", () => {
  it("reports a failed detail load and shows no keys or audit section", async () => {
    getPairingMock.mockRejectedValue(new Error("pairing lookup failed"));
    pairingAuditMock.mockResolvedValue({ events: [], total: 0 });
    renderDialog();

    const dialog = screen.getByRole("dialog", { name: "Pairing details" });
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      "Failed to load pairing: pairing lookup failed",
    );
    expect(within(dialog).queryByRole("heading", { name: "API keys" })).not.toBeInTheDocument();
    expect(within(dialog).queryByRole("heading", { name: "Audit trail" })).not.toBeInTheDocument();
    // Close still works from the failure state.
    expect(within(dialog).getByRole("button", { name: "Close" })).toBeInTheDocument();
  });

  it("keeps the pairing's details when only the audit trail fails to load", async () => {
    getPairingMock.mockResolvedValue(detail);
    pairingAuditMock.mockRejectedValue(new Error("audit unavailable"));
    renderDialog();

    const dialog = await screen.findByRole("dialog", { name: "Pairing details: Field laptop" });
    expect(await within(dialog).findByText("••••WXyZ")).toBeInTheDocument();
    expect(await within(dialog).findByRole("alert")).toHaveTextContent(
      "Failed to load audit trail: audit unavailable",
    );
    expect(within(dialog).queryByText("No audit events.")).not.toBeInTheDocument();
  });

  it("says so when the pairing has no audit events", async () => {
    getPairingMock.mockResolvedValue(detail);
    pairingAuditMock.mockResolvedValue({ events: [], total: 0 });
    renderDialog();

    const dialog = await screen.findByRole("dialog", { name: "Pairing details: Field laptop" });
    expect(await within(dialog).findByText("No audit events.")).toBeInTheDocument();
    expect(within(dialog).queryByRole("alert")).not.toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "Older" })).not.toBeInTheDocument();
    expect(pairingAuditMock).toHaveBeenCalledWith(1, { limit: 20, offset: 0 });
  });
});
