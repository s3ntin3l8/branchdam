import { render, screen, waitFor, fireEvent } from "@testing-library/react";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { MemoryRouter, Routes, Route } from "react-router";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import UsersPage from "./UsersPage";
import type {
  AdminResetPasswordResponse,
  AttributionUser,
  CreateUserInput,
  CreateUserResponse,
  ListUsersResponse,
  Me,
} from "../api/types";

const listUsersMock = vi.fn<(params?: { limit?: number; offset?: number }) => Promise<ListUsersResponse>>();
const createUserMock = vi.fn<(input: CreateUserInput) => Promise<CreateUserResponse>>();
const adminResetPasswordMock = vi.fn<(id: number) => Promise<AdminResetPasswordResponse>>();
const disableUserMock = vi.fn<(id: number) => Promise<Record<string, never>>>();
const meMock = vi.fn<() => Promise<Me>>();

vi.mock("../api/client", () => ({
  api: {
    me: () => meMock(),
    listUsers: (params?: { limit?: number; offset?: number }) => listUsersMock(params),
    createUser: (input: CreateUserInput) => createUserMock(input),
    adminResetPassword: (id: number) => adminResetPasswordMock(id),
    disableUser: (id: number) => disableUserMock(id),
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

const sampleUsers: AttributionUser[] = [
  {
    id: 1,
    authProvider: "local",
    externalUid: "1",
    username: "alice",
    email: "alice@example.com",
    isAdmin: true,
    source: "local",
    createdAt: 1700000000,
    lastSeenAt: 1700001000,
  },
  {
    id: 2,
    authProvider: "local",
    externalUid: "2",
    username: "bob",
    email: "bob@example.com",
    isAdmin: false,
    source: "local",
    createdAt: 1700002000,
    lastSeenAt: 1700002500,
  },
  {
    id: 3,
    authProvider: "authentik",
    externalUid: "carol-sub",
    username: "carol",
    isAdmin: false,
    source: "authentik",
    createdAt: 1700003000,
    lastSeenAt: 1700003500,
    disabledAt: 1700004000,
  },
];

function renderWithClient(ui: React.ReactElement) {
  const queryClient = new QueryClient({
    defaultOptions: {
      queries: { retry: false },
    },
  });
  return render(
    <QueryClientProvider client={queryClient}>
      <MemoryRouter initialEntries={["/users"]}>
        <Routes>
          <Route path="/users" element={ui} />
        </Routes>
      </MemoryRouter>
    </QueryClientProvider>,
  );
}

describe("UsersPage", () => {
  beforeEach(() => {
    vi.clearAllMocks();
    meMock.mockResolvedValue({
      kind: "user",
      name: "alice",
      authenticated: true,
      isAdmin: true,
      isLocal: true,
      localUserId: 1,
    });
    listUsersMock.mockResolvedValue({
      users: sampleUsers,
      total: sampleUsers.length,
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
  });

  it("renders user table with usernames, roles, and provider badges", async () => {
    renderWithClient(<UsersPage />);

    expect(screen.getByText("Loading users…")).toBeInTheDocument();

    await waitFor(() => {
      expect(screen.getByText("alice")).toBeInTheDocument();
    });

    expect(screen.getByText("bob")).toBeInTheDocument();
    expect(screen.getByText("carol")).toBeInTheDocument();

    // Check badges
    expect(screen.getByText("Admin")).toBeInTheDocument();
    expect(screen.getAllByText("User").length).toBe(3);
    expect(screen.getByText("Disabled")).toBeInTheDocument();
  });

  it("filters users via search input", async () => {
    renderWithClient(<UsersPage />);

    await waitFor(() => {
      expect(screen.getByText("alice")).toBeInTheDocument();
    });

    const searchInput = screen.getByPlaceholderText("Search users by name, username, or email…");
    fireEvent.change(searchInput, { target: { value: "bob" } });

    expect(screen.queryByText("alice")).not.toBeInTheDocument();
    expect(screen.getByText("bob")).toBeInTheDocument();
  });

  it("creates a user and shows generated password modal when password is empty", async () => {
    createUserMock.mockResolvedValue({
      user: {
        id: 4,
        authProvider: "local",
        externalUid: "4",
        username: "dave",
        isAdmin: false,
        source: "local",
        createdAt: 1700005000,
        lastSeenAt: 1700005000,
      },
      temporaryPassword: "generated-secret-pass",
      shownOnceNotice: "Copy this password now.",
    });

    renderWithClient(<UsersPage />);

    await waitFor(() => {
      expect(screen.getByText("alice")).toBeInTheDocument();
    });

    fireEvent.click(screen.getByText("Add User"));
    expect(screen.getByRole("heading", { name: "Create User" })).toBeInTheDocument();

    fireEvent.change(screen.getByPlaceholderText("e.g. jdoe"), { target: { value: "dave" } });
    fireEvent.click(screen.getByRole("button", { name: "Create User" }));

    await waitFor(() => {
      expect(createUserMock).toHaveBeenCalledWith({
        username: "dave",
        email: undefined,
        password: undefined,
        isAdmin: false,
      });
    });

    await waitFor(() => {
      expect(screen.getByText("Temporary Password Generated")).toBeInTheDocument();
      expect(screen.getByText("generated-secret-pass")).toBeInTheDocument();
    });
  });

  it("resets password for a user", async () => {
    adminResetPasswordMock.mockResolvedValue({
      user: {
        id: 2,
        username: "bob",
        isAdmin: false,
        source: "local",
        createdAt: 1700002000,
        createdBy: "system",
      },
      newPassword: "reset-temp-password",
      shownOnceNotice: "Store this password securely.",
    });

    renderWithClient(<UsersPage />);

    await waitFor(() => {
      expect(screen.getByText("bob")).toBeInTheDocument();
    });

    const resetButtons = screen.getAllByText("Reset Password");
    fireEvent.click(resetButtons[1]); // bob is index 1

    expect(screen.getByText("Reset User Password")).toBeInTheDocument();
    fireEvent.click(screen.getByText("Confirm Reset"));

    await waitFor(() => {
      expect(adminResetPasswordMock).toHaveBeenCalledWith(2);
    });

    await waitFor(() => {
      expect(screen.getByText("Temporary Password Generated")).toBeInTheDocument();
      expect(screen.getByText("reset-temp-password")).toBeInTheDocument();
    });
  });

  it("disables an active user and hides disable button for current user", async () => {
    disableUserMock.mockResolvedValue({});
    const confirmSpy = vi.spyOn(window, "confirm").mockReturnValue(true);

    renderWithClient(<UsersPage />);

    await waitFor(() => {
      expect(screen.getByText("bob")).toBeInTheDocument();
    });

    // Alice is localUserId: 1 (current user) so she has no Disable button.
    // Carol is already disabled so she has no Disable button.
    // Only Bob (id: 2) has a Disable button.
    const disableButtons = screen.getAllByText("Disable");
    expect(disableButtons.length).toBe(1);
    fireEvent.click(disableButtons[0]);

    expect(confirmSpy).toHaveBeenCalledWith("Disable logins for bob?");
    await waitFor(() => {
      expect(disableUserMock).toHaveBeenCalledWith(2);
    });
  });
});
