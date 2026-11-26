import { render, screen, waitFor } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import AssistantTagRevert from "./AssistantTagRevert";
import { api } from "../api/client";
import type { Me } from "../api/types";

vi.mock("../api/client", () => ({
  api: {
    me: vi.fn(),
    revertAssistantTags: vi.fn(),
  },
}));

function renderRevert() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={queryClient}>
      <AssistantTagRevert />
    </QueryClientProvider>,
  );
}

const admin: Me = { kind: "user", authenticated: true, isAdmin: true };

describe("AssistantTagRevert", () => {
  it("hides the action from a non-admin", async () => {
    vi.mocked(api.me).mockResolvedValue({ kind: "user", authenticated: true, isAdmin: false });
    renderRevert();
    await waitFor(() => expect(api.me).toHaveBeenCalled());
    expect(screen.queryByRole("button", { name: "Revert assistant tags" })).not.toBeInTheDocument();
  });

  it("asks an admin to confirm before reverting", async () => {
    vi.mocked(api.me).mockResolvedValue(admin);
    vi.mocked(api.revertAssistantTags).mockResolvedValue({ tagsRemoved: 2, annotationsRemoved: 1 });
    renderRevert();
    await userEvent.click(await screen.findByRole("button", { name: "Revert assistant tags" }));
    await userEvent.click(screen.getByRole("button", { name: "Revert" }));
    expect(api.revertAssistantTags).toHaveBeenCalledWith({ source: "assistant" });
    expect(await screen.findByText("Removed 2 tags and 1 description.")).toBeInTheDocument();
  });
});
