import { render, screen } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";
import App from "./App";
import type { ConnectionState } from "./hooks/useEventStream";

let connectionState: ConnectionState = "open";

vi.mock("./hooks/useEventStream", () => ({
  useEventStream: () => ({ connectionState }),
}));

vi.mock("./hooks/queries", () => ({
  useMe: () => ({ data: null }),
  useUnlinkedCount: () => ({ data: 0 }),
}));

vi.mock("./pages/AssetListPage", () => ({ default: () => <div>Assets page</div> }));
vi.mock("./pages/AssetDetailPage", () => ({ default: () => <div>Asset detail page</div> }));
vi.mock("./pages/AuditQueuePage", () => ({ default: () => <div>Audit queue page</div> }));
vi.mock("./pages/IngestPage", () => ({ default: () => <div>Ingest page</div> }));
vi.mock("./pages/IngestJobsPage", () => ({ default: () => <div>Ingest jobs page</div> }));
vi.mock("./pages/SettingsPage", () => ({ default: () => <div>Settings page</div> }));
vi.mock("./pages/StorageHealthPage", () => ({ default: () => <div>Storage health page</div> }));

describe("App", () => {
  beforeEach(() => {
    connectionState = "open";
  });

  it("shows the reconnecting banner while the event stream is disconnected", () => {
    connectionState = "disconnected";

    render(
      <MemoryRouter>
        <App />
      </MemoryRouter>,
    );

    expect(screen.getByText("Reconnecting")).toBeInTheDocument();
    expect(screen.getByText(/live updates are paused/i)).toBeInTheDocument();
  });

  it("does not show the reconnecting banner when the event stream is open", () => {
    render(
      <MemoryRouter>
        <App />
      </MemoryRouter>,
    );

    expect(screen.queryByText("Reconnecting")).not.toBeInTheDocument();
  });
});
