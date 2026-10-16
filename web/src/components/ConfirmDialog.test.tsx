import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import ConfirmDialog from "./ConfirmDialog";

function renderDialog(overrides: Partial<React.ComponentProps<typeof ConfirmDialog>> = {}) {
  const props = {
    title: "Delete thing",
    body: <>This removes the thing.</>,
    confirmLabel: "Confirm Delete",
    pendingLabel: "Deleting…",
    isPending: false,
    onConfirm: vi.fn(),
    onCancel: vi.fn(),
    ...overrides,
  };
  render(<ConfirmDialog {...props} />);
  return props;
}

describe("ConfirmDialog", () => {
  it("renders a labelled dialog with the body and focuses Cancel, not Confirm", () => {
    renderDialog();
    expect(screen.getByRole("dialog", { name: "Delete thing" })).toBeInTheDocument();
    expect(screen.getByText("This removes the thing.")).toBeInTheDocument();
    expect(screen.getByRole("button", { name: "Cancel" })).toHaveFocus();
  });

  it("calls onConfirm and onCancel from their buttons", async () => {
    const props = renderDialog();
    await userEvent.click(screen.getByRole("button", { name: "Confirm Delete" }));
    expect(props.onConfirm).toHaveBeenCalledTimes(1);
    await userEvent.click(screen.getByRole("button", { name: "Cancel" }));
    expect(props.onCancel).toHaveBeenCalledTimes(1);
  });

  it("cancels on Escape", async () => {
    const props = renderDialog();
    await userEvent.keyboard("{Escape}");
    expect(props.onCancel).toHaveBeenCalledTimes(1);
  });

  it("can't be dismissed or re-confirmed while pending", async () => {
    const props = renderDialog({ isPending: true });
    expect(screen.getByRole("button", { name: "Deleting…" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "Cancel" })).toBeDisabled();
    await userEvent.keyboard("{Escape}");
    expect(props.onCancel).not.toHaveBeenCalled();
  });

  it("shows the error with its label", () => {
    renderDialog({ error: new Error("disk full"), errorLabel: "Failed to delete" });
    expect(screen.getByText("Failed to delete: Error: disk full")).toBeInTheDocument();
  });
});
