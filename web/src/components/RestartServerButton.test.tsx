import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { RestartServerCard, RestartToApplyButton } from "./RestartServerButton";
import { api } from "../api/client";

vi.mock("../api/client", () => ({
  api: { postRestart: vi.fn() },
}));

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={queryClient}>{ui}</QueryClientProvider>);
}

beforeEach(() => {
  vi.mocked(api.postRestart).mockReset();
});

describe("RestartServerCard", () => {
  it("asks before restarting and does nothing on Cancel", async () => {
    renderWithClient(<RestartServerCard />);

    await userEvent.click(screen.getByRole("button", { name: "Restart server" }));
    expect(screen.getByText("Restart now?")).toBeInTheDocument();
    expect(api.postRestart).not.toHaveBeenCalled();

    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(screen.queryByText("Restart now?")).not.toBeInTheDocument();
    expect(api.postRestart).not.toHaveBeenCalled();
  });

  it("restarts on confirm and says it is reconnecting", async () => {
    vi.mocked(api.postRestart).mockResolvedValueOnce({ ok: true });
    renderWithClient(<RestartServerCard />);

    await userEvent.click(screen.getByRole("button", { name: "Restart server" }));
    await userEvent.click(screen.getByRole("button", { name: "Restart" }));

    await waitFor(() => expect(api.postRestart).toHaveBeenCalledTimes(1));
    expect(await screen.findByText(/restarting… this page will reconnect/i)).toBeInTheDocument();
  });

  it("surfaces a failed restart", async () => {
    vi.mocked(api.postRestart).mockRejectedValueOnce(new Error("forbidden"));
    renderWithClient(<RestartServerCard />);

    await userEvent.click(screen.getByRole("button", { name: "Restart server" }));
    await userEvent.click(screen.getByRole("button", { name: "Restart" }));

    expect(await screen.findByText(/failed to restart: error: forbidden/i)).toBeInTheDocument();
  });
});

describe("RestartToApplyButton", () => {
  it("confirms inline, then shows Restarting… after success", async () => {
    vi.mocked(api.postRestart).mockResolvedValueOnce({ ok: true });
    renderWithClient(<RestartToApplyButton />);

    await userEvent.click(screen.getByRole("button", { name: "Restart to apply" }));
    expect(api.postRestart).not.toHaveBeenCalled();
    await userEvent.click(screen.getByRole("button", { name: "Restart" }));

    expect(await screen.findByText("Restarting…")).toBeInTheDocument();
    expect(api.postRestart).toHaveBeenCalledTimes(1);
  });
});
