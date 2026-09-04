import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { createMemoryRouter, RouterProvider } from "react-router";
import { describe, expect, it, vi } from "vitest";
import StorageHealthPage from "./StorageHealthPage";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: {
    getStorageHealth: vi.fn(),
    deleteAgentTelemetry: vi.fn(),
    pruneCache: vi.fn(),
    putStorageLocation: vi.fn(),
    postRestart: vi.fn(),
  },
}));

function baseAgent(overrides: Partial<import("../api/types").AgentScratchHealth> = {}): import("../api/types").AgentScratchHealth {
  return {
    agentId: "workstation-macbook",
    clientVersion: "1.2.0",
    timestampUnix: Math.floor(Date.now() / 1000) - 120,
    mountPath: "/Volumes/ResolveScratch",
    totalBytes: 2000000000000,
    usedBytes: 1200000000000,
    freeBytes: 800000000000,
    mirrorsSizeBytes: 200000000000,
    renderCacheSizeBytes: 600000000000,
    proxiesSizeBytes: 300000000000,
    prunableBytes: 400000000000,
    lastPruneTimestampUnix: Math.floor(Date.now() / 1000) - 3600,
    lastReclaimedBytes: 150000000000,
    lastPruneDurationMs: 2400,
    prunedItemCounts: { mirrors: 12, renderCacheProjects: 4, proxies: 6 },
    isLowSpace: false,
    isCriticalSpace: false,
    isStale: false,
    ...overrides,
  };
}

function baseLocation(overrides: Partial<import("../api/types").StorageLocationHealth> = {}) {
  return {
    id: 1,
    name: "Scratch Mount",
    rootPath: "/mnt/scratch",
    tier: "TIER1_LOCAL_SCRATCH" as const,
    readOnly: false,
    prunable: true,
    isActive: true,
    watch: true,
    sweep: false,
    sweepIntervalSecs: 300,
    cacheTtlHours: 24,
    disabled: false,
    overriddenFields: [],
    nodeCount: 1500,
    totalBytes: 1000000000000,
    usedBytes: 400000000000,
    freeBytes: 600000000000,
    isDegraded: false,
    ...overrides,
  };
}

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: { queries: { retry: false } },
  });
  const router = createMemoryRouter(
    [{ path: "*", element: ui }],
    { initialEntries: ["/storage-health"] },
  );
  const result = render(
    <QueryClientProvider client={queryClient}>
      <RouterProvider router={router} />
    </QueryClientProvider>
  );
  return { ...result, queryClient, router };
}

describe("StorageHealthPage", () => {
  it("renders storage location gauges and queue depth metrics", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValueOnce({
      locations: [
        {
          id: 1,
          name: "Scratch Mount",
          rootPath: "/mnt/scratch",
          tier: "TIER1_LOCAL_SCRATCH",
          readOnly: false,
          prunable: true,
          isActive: true,
          watch: true,
          sweep: false,
          sweepIntervalSecs: 300,
          cacheTtlHours: 24,
          disabled: false,
          overriddenFields: [],
          nodeCount: 1500,
          totalBytes: 1000000000000,
          usedBytes: 400000000000,
          freeBytes: 600000000000,
          isDegraded: false,
        },
        {
          id: 2,
          name: "Master Archive",
          rootPath: "/mnt/archive",
          tier: "TIER3_MASTER_ARCHIVE",
          readOnly: true,
          prunable: false,
          isActive: true,
          watch: false,
          sweep: false,
          sweepIntervalSecs: 0,
          cacheTtlHours: 0,
          disabled: false,
          overriddenFields: [],
          nodeCount: 0,
          totalBytes: 0,
          usedBytes: 0,
          freeBytes: 0,
          isDegraded: true,
          degradedMessage: "permission denied",
        },
      ],
      queues: {
        workerPoolInFlight: 3,
        workerPoolQueued: 12,
        workerPoolCapacity: 64,
        workerCount: 4,
        runningScanJobs: 1,
      },
    });

    renderWithClient(<StorageHealthPage />);

    await waitFor(() => {
      expect(screen.getByText("Storage Health")).toBeInTheDocument();
    });

    expect(screen.getByText("Scratch Mount")).toBeInTheDocument();
    expect(screen.getByText("TIER1_LOCAL_SCRATCH")).toBeInTheDocument();
    expect(screen.getByText("HEALTHY")).toBeInTheDocument();
    expect(screen.getByText("1,500")).toBeInTheDocument();

    expect(screen.getByText("Master Archive")).toBeInTheDocument();
    expect(screen.getByText("DEGRADED")).toBeInTheDocument();
    expect(screen.getByText("permission denied")).toBeInTheDocument();

    expect(screen.getByText("3")).toBeInTheDocument();
    expect(screen.getByText("12 / 64")).toBeInTheDocument();
    expect(screen.getByText("4")).toBeInTheDocument();
    expect(screen.getByText("1")).toBeInTheDocument();
  });

  it("only shows the purge control on prunable locations", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValueOnce({
      locations: [
        {
          id: 1, name: "Scratch Mount", rootPath: "/mnt/scratch", tier: "TIER1_LOCAL_SCRATCH",
          readOnly: false, prunable: true, isActive: true, watch: true, sweep: false, sweepIntervalSecs: 300,
          cacheTtlHours: 24, disabled: false, overriddenFields: [], nodeCount: 1, totalBytes: 100, usedBytes: 50,
          freeBytes: 50, isDegraded: false,
        },
        {
          id: 2, name: "Master Archive", rootPath: "/mnt/archive", tier: "TIER3_MASTER_ARCHIVE",
          readOnly: true, prunable: false, isActive: true, watch: false, sweep: false, sweepIntervalSecs: 0,
          cacheTtlHours: 0, disabled: false, overriddenFields: [], nodeCount: 0, totalBytes: 0, usedBytes: 0,
          freeBytes: 0, isDegraded: false,
        },
      ],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());

    expect(screen.getAllByRole("button", { name: /purge cache/i })).toHaveLength(1);
  });

  it("runs a dry-run plan then executes on confirm", async () => {
    // mockResolvedValue (not -Once): the purge mutation's onSuccess
    // invalidates ["storage-health"], triggering a refetch this test must
    // also satisfy, or TanStack Query surfaces "Query data cannot be
    // undefined" and the page renders its error state instead.
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [
        {
          id: 1, name: "Scratch Mount", rootPath: "/mnt/scratch", tier: "TIER1_LOCAL_SCRATCH",
          readOnly: false, prunable: true, isActive: true, watch: true, sweep: false, sweepIntervalSecs: 300,
          cacheTtlHours: 24, disabled: false, overriddenFields: [], nodeCount: 1, totalBytes: 100, usedBytes: 50,
          freeBytes: 50, isDegraded: false,
        },
      ],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });
    vi.mocked(api.pruneCache).mockResolvedValueOnce({
      executed: false,
      candidates: [{ nodeId: 42, filePath: "/mnt/scratch/proxy.jpg", sizeBytes: 12345, purged: false }],
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: /purge cache/i }));
    await waitFor(() => expect(api.pruneCache).toHaveBeenCalledWith({ storageLocationId: 1 }));
    await waitFor(() => expect(screen.getByText(/1 file eligible/)).toBeInTheDocument());

    vi.mocked(api.pruneCache).mockResolvedValueOnce({
      executed: true,
      candidates: [{ nodeId: 42, filePath: "/mnt/scratch/proxy.jpg", sizeBytes: 12345, purged: true }],
    });
    await userEvent.click(screen.getByRole("button", { name: /confirm purge/i }));
    await waitFor(() =>
      expect(api.pruneCache).toHaveBeenCalledWith({ storageLocationId: 1, execute: true }),
    );
    await waitFor(() => expect(screen.getByText("Purged 1 file.")).toBeInTheDocument());
  });

  it("shows a DISABLED badge independently of INACTIVE", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation({ disabled: true, isActive: true })],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());

    expect(screen.getByText("DISABLED")).toBeInTheDocument();
    expect(screen.queryByText("INACTIVE")).not.toBeInTheDocument();
  });

  it("opens an inline edit form and saves only the fields that changed", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation()],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });
    vi.mocked(api.putStorageLocation).mockResolvedValueOnce({ ok: true });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: "Edit" }));
    const nameInput = screen.getByDisplayValue("Scratch Mount");
    await userEvent.clear(nameInput);
    await userEvent.type(nameInput, "Renamed");

    await userEvent.click(screen.getByRole("button", { name: "Save" }));

    await waitFor(() =>
      expect(api.putStorageLocation).toHaveBeenCalledWith(1, { set: { name: "Renamed" } }),
    );
  });

  it("only offers Reset to config when the location has a live override", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation({ overriddenFields: ["sweep"] })],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });
    vi.mocked(api.putStorageLocation).mockResolvedValueOnce({ ok: true });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());
    await userEvent.click(screen.getByRole("button", { name: "Edit" }));

    await userEvent.click(screen.getByRole("button", { name: "Reset to config" }));
    await waitFor(() =>
      expect(api.putStorageLocation).toHaveBeenCalledWith(1, { unset: ["sweep"] }),
    );
  });

  it("does not show Reset to config on a location with no override", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation()],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());
    await userEvent.click(screen.getByRole("button", { name: "Edit" }));

    expect(screen.queryByRole("button", { name: "Reset to config" })).not.toBeInTheDocument();
  });

  it("disabling a location is a separate, immediate action from Save", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation({ disabled: false })],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });
    vi.mocked(api.putStorageLocation).mockResolvedValueOnce({ ok: true });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());
    await userEvent.click(screen.getByRole("button", { name: "Edit" }));

    await userEvent.click(screen.getByRole("button", { name: "Disable location" }));
    await waitFor(() =>
      expect(api.putStorageLocation).toHaveBeenCalledWith(1, { set: { enabled: false } }),
    );
  });

  // Hermes review finding on PR #282: Enable/Disable fires its own PUT with
  // only {enabled}, which would silently drop an unsaved edit to one of the
  // other five fields sitting in the same form.
  it("disables the Enable/Disable action while the batched form has unsaved edits", async () => {
    // No clearMocks/resetMocks config for this suite; putStorageLocation's
    // call count otherwise accumulates across every test in this file.
    vi.mocked(api.putStorageLocation).mockClear();
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation({ disabled: false })],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());
    await userEvent.click(screen.getByRole("button", { name: "Edit" }));

    const nameInput = screen.getByDisplayValue("Scratch Mount");
    await userEvent.type(nameInput, " Renamed");

    expect(screen.getByRole("button", { name: "Disable location" })).toBeDisabled();

    await userEvent.click(screen.getByRole("button", { name: "Disable location" }));
    expect(api.putStorageLocation).not.toHaveBeenCalled();
  });

  // useStorageHealth polls every 10s and useEventStream invalidates it on
  // every SSE nudge, so a background refetch landing while the edit form is
  // open must not clobber an in-progress edit -- the same
  // baseline-resync-during-render hazard Hermes filed CHANGES_REQUESTED on
  // for SettingsFieldEditor in PR3, pinned here for StorageLocationEditForm.
  it("keeps an in-progress draft when a background refetch returns unchanged data", async () => {
    // No clearMocks/resetMocks config for this suite, so call counts
    // otherwise accumulate across every test in this file.
    vi.mocked(api.getStorageHealth).mockClear();
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation()],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });

    const { queryClient } = renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());

    await userEvent.click(screen.getByRole("button", { name: "Edit" }));
    const nameInput = screen.getByDisplayValue("Scratch Mount");
    await userEvent.clear(nameInput);
    await userEvent.type(nameInput, "Draft In Progress");

    // Simulate the 10s poll / SSE-nudge refetch landing mid-edit with
    // identical server data -- invalidateQueries awaits the refetch of any
    // active query by default, so no extra waitFor is needed here.
    await queryClient.invalidateQueries({ queryKey: ["storage-health"] });
    expect(api.getStorageHealth).toHaveBeenCalledTimes(2);

    expect(screen.getByDisplayValue("Draft In Progress")).toBeInTheDocument();
  });

  // Store.PendingRestart() (SettingsPage's banner) never sees
  // storage-location overrides -- see AGENTS.md's storageLocation.<rootPath>.*
  // invariant -- so this page's own restart button is the only affordance
  // an operator gets after a location-only edit. It must therefore be
  // gated on overriddenFields, not always shown.
  it("shows a restart-to-apply button only when a location has a pending override", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation({ overriddenFields: [] })],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Scratch Mount")).toBeInTheDocument());

    expect(screen.queryByRole("button", { name: "Restart to apply" })).not.toBeInTheDocument();
  });

  it("fires the restart request after confirming the restart-to-apply button", async () => {
    const user = userEvent.setup();
    vi.mocked(api.postRestart).mockClear();
    vi.mocked(api.postRestart).mockResolvedValue({ ok: true });
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation({ overriddenFields: ["watch"] })],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
    });

    renderWithClient(<StorageHealthPage />);

    const restartButton = await screen.findByRole("button", { name: "Restart to apply" });
    await user.click(restartButton);
    await user.click(await screen.findByRole("button", { name: "Restart" }));

    await waitFor(() => {
      expect(api.postRestart).toHaveBeenCalledTimes(1);
    });
  });

  it("renders empty state when no workstation agents are reporting", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation()],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
      agents: [],
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("Workstation Scratch Storage (Tier 1)")).toBeInTheDocument());

    expect(screen.getByText("No workstation agents reporting scratch telemetry yet.")).toBeInTheDocument();
  });

  it("renders connected workstation scratch storage card with capacity, breakdown, and prune statistics", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation()],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
      agents: [baseAgent()],
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("workstation-macbook")).toBeInTheDocument());

    expect(screen.getByText("v1.2.0")).toBeInTheDocument();
    expect(screen.getByText("TIER1_SCRATCH")).toBeInTheDocument();
    expect(screen.getByText("/Volumes/ResolveScratch")).toBeInTheDocument();
    expect(screen.getAllByText("HEALTHY").length).toBe(2);

    // Storage breakdown
    expect(screen.getByText("Render Cache")).toBeInTheDocument();
    expect(screen.getByText("Ingest Mirrors")).toBeInTheDocument();
    expect(screen.getByText("Proxies")).toBeInTheDocument();
    expect(screen.getByText("Prunable")).toBeInTheDocument();

    // Prune statistics
    expect(screen.getByText("Pruning Run Statistics")).toBeInTheDocument();
    expect(screen.getByText("12 mirrors")).toBeInTheDocument();
    expect(screen.getByText("4 renderCacheProjects")).toBeInTheDocument();
    expect(screen.getByText("6 proxies")).toBeInTheDocument();
  });

  it("renders low space and critical space warnings on workstation cards", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation()],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
      agents: [
        baseAgent({ agentId: "agent-low", isLowSpace: true, isCriticalSpace: false }),
        baseAgent({ agentId: "agent-critical", isLowSpace: true, isCriticalSpace: true }),
      ],
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("agent-low")).toBeInTheDocument());

    expect(screen.getByText("LOW SPACE")).toBeInTheDocument();
    expect(screen.getByText("CRITICAL SPACE")).toBeInTheDocument();
    expect(screen.getByText(/Critical low scratch storage space/)).toBeInTheDocument();
  });

  it("renders stale badge on offline workstation agents", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation()],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
      agents: [baseAgent({ agentId: "agent-stale", isStale: true })],
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("agent-stale")).toBeInTheDocument());

    expect(screen.getByText("STALE")).toBeInTheDocument();
  });

  it("allows dismissing a workstation agent from the dashboard", async () => {
    const user = userEvent.setup();
    vi.mocked(api.deleteAgentTelemetry).mockClear();
    vi.mocked(api.deleteAgentTelemetry).mockResolvedValue({ ok: true });
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation()],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
      agents: [baseAgent({ agentId: "agent-to-dismiss" })],
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("agent-to-dismiss")).toBeInTheDocument());

    const dismissBtn = screen.getByRole("button", { name: "Dismiss" });
    await user.click(dismissBtn);

    const confirmBtn = await screen.findByRole("button", { name: "Confirm" });
    await user.click(confirmBtn);

    await waitFor(() => {
      expect(api.deleteAgentTelemetry).toHaveBeenCalledWith("agent-to-dismiss");
    });
  });

  it("renders mobile companion device with MOBILE_COMPANION badge", async () => {
    vi.mocked(api.getStorageHealth).mockResolvedValue({
      locations: [baseLocation()],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
      agents: [baseAgent({ agentId: "pixel-9-pro", clientVersion: "2.0.0" })],
    });

    renderWithClient(<StorageHealthPage />);
    await waitFor(() => expect(screen.getByText("pixel-9-pro")).toBeInTheDocument());

    expect(screen.getByText("MOBILE_COMPANION")).toBeInTheDocument();
    expect(screen.getByText("v2.0.0")).toBeInTheDocument();
  });

  it("guards against accidental navigation when a storage location has unsaved edits", async () => {
    const user = userEvent.setup();
    vi.mocked(api.getStorageHealth).mockResolvedValueOnce({
      locations: [baseLocation({ id: 1, name: "Scratch Mount" })],
      queues: { workerPoolInFlight: 0, workerPoolQueued: 0, workerPoolCapacity: 0, workerCount: 0, runningScanJobs: 0 },
      agents: [],
    });

    const { router } = renderWithClient(<StorageHealthPage />);
    expect(await screen.findByText("Scratch Mount")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Edit" }));
    const nameInput = screen.getByDisplayValue("Scratch Mount");
    await user.type(nameInput, " Modified");

    await router.navigate("/other-page");

    expect(await screen.findByText("Unsaved Changes")).toBeInTheDocument();
    expect(screen.getByText("You have unsaved changes. Leave without saving?")).toBeInTheDocument();

    await user.click(screen.getByRole("button", { name: "Stay" }));
    await waitFor(() => {
      expect(screen.queryByText("Unsaved Changes")).not.toBeInTheDocument();
    });
  });
});
