import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import SettingsPage from "./SettingsPage";
import { api, ApiError } from "../api/client";
import type { SettingsField, SettingsResponse } from "../api/types";

vi.mock("../api/client", async (importOriginal) => {
  const actual = await importOriginal<typeof import("../api/client")>();
  return {
    ApiError: actual.ApiError,
    api: {
      config: vi.fn(),
      listPathRewrites: vi.fn(),
      getSettings: vi.fn(),
      putSettings: vi.fn(),
    },
  };
});

function field(overrides: Partial<SettingsField>): SettingsField {
  return {
    key: "immich.apiUrl",
    type: "string",
    label: "Immich API URL",
    group: "Immich",
    value: "http://immich:2283",
    source: "override",
    applyMode: "live",
    secret: false,
    editable: true,
    ...overrides,
  };
}

function settingsResponse(overrides: Partial<SettingsResponse> = {}): SettingsResponse {
  return {
    fields: [
      field({}),
      field({
        key: "immich.apiKey",
        label: "Immich API Key",
        value: undefined,
        source: "config",
        secret: true,
        hasValue: true,
      }),
      field({
        key: "workers.hashWorkers",
        type: "int",
        label: "Hash Workers",
        group: "Workers",
        value: 4,
        source: "config",
        applyMode: "restart",
        pendingRestart: true,
      }),
      field({
        key: "listenAddr",
        type: "string",
        label: "Listen Address",
        group: "Server",
        value: ":8080",
        source: "config",
        applyMode: "never",
        editable: false,
        readOnlyReason: "A running process cannot change its own listen address.",
      }),
      field({
        key: "authz.groups",
        type: "stringList",
        label: "Admin Groups",
        group: "Authorization",
        value: ["dam-admins"],
        source: "config",
        applyMode: "never",
        editable: false,
        readOnlyReason: "editable only via config.yaml/.env",
      }),
    ],
    pendingRestart: [],
    secretsAvailable: true,
    ...overrides,
  };
}

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>);
}

describe("SettingsPage", () => {
  it("renders server version and path rewrites table", async () => {
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([
      { from: "D:\\Footage", to: "/storage/footage" },
      { from: "/Volumes/NAS", to: "/storage/nas" },
    ]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse());

    renderWithClient(<SettingsPage />);

    expect(await screen.findByText("v1.2.3")).toBeInTheDocument();
    expect(screen.getByText("D:\\Footage")).toBeInTheDocument();
    expect(screen.getByText("/storage/footage")).toBeInTheDocument();
    expect(screen.getByText("/Volumes/NAS")).toBeInTheDocument();
    expect(screen.getByText("/storage/nas")).toBeInTheDocument();
  });

  it("groups fields and shows provenance chips", async () => {
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse());

    renderWithClient(<SettingsPage />);

    expect(await screen.findByText("Immich")).toBeInTheDocument();
    expect(screen.getByText("Workers")).toBeInTheDocument();
    expect(screen.getByText("UI override")).toBeInTheDocument();
    expect(screen.getAllByText("from config.yaml / .env").length).toBeGreaterThan(0);
  });

  it("shows the pending-restart banner listing the affected keys", async () => {
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse({ pendingRestart: ["workers.hashWorkers"] }));

    renderWithClient(<SettingsPage />);

    expect(await screen.findByText(/Restart required/)).toBeInTheDocument();
    expect(screen.getByText(/workers.hashWorkers/)).toBeInTheDocument();
  });

  it("shows a secrets-unavailable banner when the server has no encryption key", async () => {
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse({ secretsAvailable: false }));

    renderWithClient(<SettingsPage />);

    expect(await screen.findByText(/Secret storage is unavailable/)).toBeInTheDocument();
    expect(screen.getByText("BRANCHDAM_SECRET_KEY")).toBeInTheDocument();
  });

  it("never renders a secret's value and disables Save until a new one is typed", async () => {
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse());

    renderWithClient(<SettingsPage />);

    const secretInput = await screen.findByPlaceholderText("Set -- enter a new value to replace");
    expect(secretInput).toHaveValue("");
    expect(screen.queryByText(/immich_api_key_secret_value/i)).not.toBeInTheDocument();
  });

  it("shows a read-only reason and no Save button for a non-editable field", async () => {
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse());

    renderWithClient(<SettingsPage />);

    expect(await screen.findByText("A running process cannot change its own listen address.")).toBeInTheDocument();
    expect(screen.getByText(":8080")).toBeInTheDocument();
  });

  it("saves an edited field via PUT {set} and refetches on success", async () => {
    const user = userEvent.setup();
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValueOnce(settingsResponse());
    vi.mocked(api.putSettings).mockResolvedValue(settingsResponse());

    renderWithClient(<SettingsPage />);

    const input = await screen.findByDisplayValue("http://immich:2283");
    await user.clear(input);
    await user.type(input, "http://immich.example:2283");

    const saveButton = await screen.findByRole("button", { name: "Save" });
    await user.click(saveButton);

    await waitFor(() => {
      expect(api.putSettings).toHaveBeenCalledWith({ set: { "immich.apiUrl": "http://immich.example:2283" } });
    });
  });

  it("clears the Save button after a successful save instead of leaving it stuck on", async () => {
    // Regression test: dirty must not be write-only. Before the fix, dirty
    // was only ever set to true on change and never reset on a successful
    // Save, so the button stayed enabled forever and the draft never
    // resynced to the newly-saved server value.
    const user = userEvent.setup();
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    const updated = settingsResponse({
      fields: settingsResponse().fields.map((f) =>
        f.key === "immich.apiUrl" ? { ...f, value: "http://immich.example:2283" } : f
      ),
    });
    vi.mocked(api.getSettings).mockResolvedValueOnce(settingsResponse()).mockResolvedValue(updated);
    vi.mocked(api.putSettings).mockResolvedValue(updated);

    renderWithClient(<SettingsPage />);

    const input = await screen.findByDisplayValue("http://immich:2283");
    await user.clear(input);
    await user.type(input, "http://immich.example:2283");
    await user.click(await screen.findByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(screen.queryByRole("button", { name: "Save" })).not.toBeInTheDocument();
    });
    expect(await screen.findByDisplayValue("http://immich.example:2283")).toBeInTheDocument();
  });

  it("restores the Save button on a failed save so the edit can be retried without retyping", async () => {
    const user = userEvent.setup();
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse());
    vi.mocked(api.putSettings).mockRejectedValue(new ApiError(422, "field \"immich.apiUrl\": must be a string"));

    renderWithClient(<SettingsPage />);

    const input = await screen.findByDisplayValue("http://immich:2283");
    await user.clear(input);
    await user.type(input, "http://immich.example:2283");
    await user.click(await screen.findByRole("button", { name: "Save" }));

    expect(await screen.findByText(/must be a string/)).toBeInTheDocument();
    expect(await screen.findByRole("button", { name: "Save" })).toBeInTheDocument();
    expect(screen.getByDisplayValue("http://immich.example:2283")).toBeInTheDocument();
  });

  it("reverts an overridden field via PUT {unset}", async () => {
    const user = userEvent.setup();
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValueOnce(settingsResponse());
    vi.mocked(api.putSettings).mockResolvedValue(settingsResponse());

    renderWithClient(<SettingsPage />);

    const revertButton = await screen.findByRole("button", { name: "Revert to config" });
    await user.click(revertButton);

    await waitFor(() => {
      expect(api.putSettings).toHaveBeenCalledWith({ unset: ["immich.apiUrl"] });
    });
  });

  it("shows an admin-required message on a 403 response", async () => {
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockRejectedValue(new ApiError(403, "admin authorization required"));

    renderWithClient(<SettingsPage />);

    expect(await screen.findByText("Admin access is required to view settings.")).toBeInTheDocument();
  });
});
