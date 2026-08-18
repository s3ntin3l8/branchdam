import { render, screen } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { describe, expect, it, vi } from "vitest";
import SettingsPage from "./SettingsPage";

vi.mock("../hooks/queries", () => ({
  useConfig: () => ({
    data: { version: "v1.2.3" },
    isLoading: false,
  }),
  usePathRewrites: () => ({
    data: [
      { from: "D:\\Footage", to: "/storage/footage" },
      { from: "/Volumes/NAS", to: "/storage/nas" },
    ],
    isLoading: false,
  }),
}));

describe("SettingsPage", () => {
  it("renders server version and path rewrites table", () => {
    const queryClient = new QueryClient({
      defaultOptions: { queries: { retry: false } },
    });

    render(
      <QueryClientProvider client={queryClient}>
        <SettingsPage />
      </QueryClientProvider>
    );

    expect(screen.getByText("System Settings")).toBeDefined();
    expect(screen.getByText("v1.2.3")).toBeDefined();
    expect(screen.getByText("D:\\Footage")).toBeDefined();
    expect(screen.getByText("/storage/footage")).toBeDefined();
    expect(screen.getByText("/Volumes/NAS")).toBeDefined();
    expect(screen.getByText("/storage/nas")).toBeDefined();
  });
});
