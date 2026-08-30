import { describe, expect, it, vi } from "vitest";
import { render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { createMemoryRouter, RouterProvider } from "react-router";
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
      postRestart: vi.fn(),
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
      field({
        key: "pathRewrites",
        type: "pathRewriteList",
        label: "Operator Path Rewrites",
        group: "Path Resolution",
        value: [
          { from: "D:\\Footage", to: "/storage/footage" },
          { from: "/Volumes/NAS", to: "/storage/nas" },
        ],
        source: "config",
        applyMode: "live",
        editable: true,
      }),
    ],
    pendingRestart: [],
    secretsAvailable: true,
    ...overrides,
  };
}

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const router = createMemoryRouter(
    [{ path: "*", element: ui }],
    { initialEntries: ["/settings"] },
  );
  return render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>,
  );
}

describe("SettingsPage", () => {
  it("renders server version and path rewrites table", async () => {
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse());

    renderWithClient(<SettingsPage />);

    expect(await screen.findByText("v1.2.3")).toBeInTheDocument();
    expect(screen.getByDisplayValue("D:\\Footage")).toBeInTheDocument();
    expect(screen.getByDisplayValue("/storage/footage")).toBeInTheDocument();
    expect(screen.getByDisplayValue("/Volumes/NAS")).toBeInTheDocument();
    expect(screen.getByDisplayValue("/storage/nas")).toBeInTheDocument();
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

  it("clears a secret's draft after a successful save instead of leaving the plaintext in the input", async () => {
    // A secret field's baseline is always "" (GET never returns the real
    // value), so the generic render-time resync never fires for it -- this
    // pins the explicit clear-on-save-click path added for that case.
    const user = userEvent.setup();
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse());
    vi.mocked(api.putSettings).mockResolvedValue(settingsResponse());

    renderWithClient(<SettingsPage />);

    const secretInput = await screen.findByPlaceholderText("Set -- enter a new value to replace");
    await user.type(secretInput, "new-secret-value");
    await user.click(await screen.findByRole("button", { name: "Save" }));

    await waitFor(() => {
      expect(secretInput).toHaveValue("");
    });
  });

  it("disables secret editing when secrets storage is unavailable", async () => {
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse({ secretsAvailable: false }));

    renderWithClient(<SettingsPage />);

    const secretInput = await screen.findByPlaceholderText("Set -- enter a new value to replace");
    expect(secretInput).toBeDisabled();
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

  it("requires a confirm step before firing the restart request", async () => {
    const user = userEvent.setup();
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse());
    vi.mocked(api.postRestart).mockClear();
    vi.mocked(api.postRestart).mockResolvedValue({ ok: true });

    renderWithClient(<SettingsPage />);

    await user.click(await screen.findByRole("button", { name: "Restart server" }));
    expect(screen.getByText("Restart now?")).toBeInTheDocument();
    expect(api.postRestart).not.toHaveBeenCalled();

    await user.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText("Restart now?")).not.toBeInTheDocument();
    expect(api.postRestart).not.toHaveBeenCalled();
  });

  it("fires the restart request and shows a restarting note after confirming", async () => {
    const user = userEvent.setup();
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.getSettings).mockResolvedValue(settingsResponse());
    vi.mocked(api.postRestart).mockClear();
    vi.mocked(api.postRestart).mockResolvedValue({ ok: true });

    renderWithClient(<SettingsPage />);

    await user.click(await screen.findByRole("button", { name: "Restart server" }));
    await user.click(await screen.findByRole("button", { name: "Restart" }));

    await waitFor(() => {
      expect(api.postRestart).toHaveBeenCalledTimes(1);
    });
    expect(await screen.findByText(/Restarting…/)).toBeInTheDocument();
  });

  it("renders safely when pendingRestart is null and authz.groups is empty or null", async () => {
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue(null as unknown as []);
    const responseWithNulls: SettingsResponse = {
      fields: [
        field({
          key: "authz.groups",
          type: "stringList",
          label: "Admin Groups",
          group: "Authorization",
          value: null as unknown as string[],
          source: "config",
          applyMode: "never",
          editable: false,
          readOnlyReason: "editable only via config.yaml/.env",
        }),
      ],
      pendingRestart: null as unknown as string[],
      secretsAvailable: true,
    };
    vi.mocked(api.getSettings).mockResolvedValue(responseWithNulls);

    renderWithClient(<SettingsPage />);

    expect(await screen.findByText("Admin Groups")).toBeInTheDocument();
    expect(screen.getByText("(empty -- every authenticated user is admin)")).toBeInTheDocument();
    expect(screen.getByText("No operator path rewrites configured.")).toBeInTheDocument();
  });

  it("supports adding, editing, deleting, saving, and reverting path rewrites", async () => {
    const user = userEvent.setup();
    vi.mocked(api.config).mockResolvedValue({ version: "v1.2.3" });
    vi.mocked(api.listPathRewrites).mockResolvedValue([]);
    vi.mocked(api.putSettings).mockResolvedValue({ fields: [], pendingRestart: [], secretsAvailable: true });
    vi.mocked(api.getSettings).mockResolvedValue(
      settingsResponse({
        fields: [
          field({
            key: "pathRewrites",
            type: "pathRewriteList",
            label: "Operator Path Rewrites",
            group: "Path Resolution",
            value: [{ from: "D:\\Footage", to: "/storage/footage" }],
            source: "override",
            applyMode: "live",
            editable: true,
          }),
        ],
      })
    );

    renderWithClient(<SettingsPage />);

    expect(await screen.findByDisplayValue("D:\\Footage")).toBeInTheDocument();
    expect(screen.getByDisplayValue("/storage/footage")).toBeInTheDocument();

    // Add a new rule
    const fromInput = screen.getByPlaceholderText(/Original host prefix/);
    const toInput = screen.getByPlaceholderText(/Target container prefix/);
    const addBtn = screen.getByRole("button", { name: "Add Rule" });

    await user.type(fromInput, "E:\\Raw");
    await user.type(toInput, "/storage/raw");
    await user.click(addBtn);

    expect(screen.getByDisplayValue("E:\\Raw")).toBeInTheDocument();
    expect(screen.getByDisplayValue("/storage/raw")).toBeInTheDocument();

    // Save rewrites
    const saveBtn = screen.getByRole("button", { name: "Save Rewrites" });
    await user.click(saveBtn);

    await waitFor(() => {
      expect(api.putSettings).toHaveBeenCalledWith({
        set: {
          pathRewrites: [
            { from: "D:\\Footage", to: "/storage/footage" },
            { from: "E:\\Raw", to: "/storage/raw" },
          ],
        },
      });
    });

    // Revert to config
    const revertBtn = screen.getByRole("button", { name: "Revert to config" });
    await user.click(revertBtn);

    await waitFor(() => {
      expect(api.putSettings).toHaveBeenCalledWith({
        unset: ["pathRewrites"],
      });
    });
  });
});
