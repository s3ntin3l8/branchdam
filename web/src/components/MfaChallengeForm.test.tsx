import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import MfaChallengeForm from "./MfaChallengeForm";

describe("MfaChallengeForm", () => {
  it("submits the entered code, and only once something is entered", async () => {
    const onSubmit = vi.fn();
    render(<MfaChallengeForm onSubmit={onSubmit} pending={false} error={undefined} />);

    const verify = screen.getByRole("button", { name: "Verify" });
    expect(verify).toBeDisabled();

    await userEvent.type(screen.getByLabelText("Verification code"), "123456");
    expect(verify).toBeEnabled();
    await userEvent.click(verify);
    expect(onSubmit).toHaveBeenCalledWith("123456");
  });

  it("submits on Enter", async () => {
    const onSubmit = vi.fn();
    render(<MfaChallengeForm onSubmit={onSubmit} pending={false} error={undefined} />);
    await userEvent.type(screen.getByLabelText("Verification code"), "abcd-efgh{Enter}");
    expect(onSubmit).toHaveBeenCalledWith("abcd-efgh");
  });

  it("shows the pending state and the error", () => {
    render(<MfaChallengeForm onSubmit={vi.fn()} pending error="Invalid code" />);
    expect(screen.getByRole("button", { name: "Verifying…" })).toBeDisabled();
    expect(screen.getByText("Invalid code")).toBeInTheDocument();
  });
});
