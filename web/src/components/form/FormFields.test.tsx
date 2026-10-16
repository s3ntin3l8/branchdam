import { useState } from "react";
import { render, screen } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { describe, expect, it, vi } from "vitest";
import { FieldRow } from "./FieldRow";
import { NumberField } from "./NumberField";
import { SecretField } from "./SecretField";
import { SelectField } from "./SelectField";
import { TextField } from "./TextField";
import { ToggleField } from "./ToggleField";

describe("FieldRow", () => {
  it("labels its control and describes it with the doc text", () => {
    render(
      <FieldRow label="Display name" doc="Shown in the header." source="config">
        <TextField value="branchDAM" onChange={vi.fn()} />
      </FieldRow>,
    );
    const input = screen.getByLabelText("Display name");
    expect(input).toHaveValue("branchDAM");
    expect(input).toHaveAccessibleDescription("Shown in the header.");
  });

  it("shows provenance, pending-restart and read-only notes", () => {
    const { rerender } = render(
      <FieldRow label="Port" source="override" pendingRestart readOnlyReason="Set by the environment.">
        <NumberField value={8080} onChange={vi.fn()} />
      </FieldRow>,
    );
    expect(screen.getByText("UI override")).toBeInTheDocument();
    expect(screen.getByText("Applies on restart")).toBeInTheDocument();
    expect(screen.getByText("Set by the environment.")).toBeInTheDocument();

    rerender(
      <FieldRow label="Port" source="config">
        <NumberField value={8080} onChange={vi.fn()} />
      </FieldRow>,
    );
    expect(screen.getByText("from config.yaml / .env")).toBeInTheDocument();
    expect(screen.queryByText("Applies on restart")).not.toBeInTheDocument();
  });

  it("renders a plain label, not a dangling <label for>, when the row has no control", () => {
    const { container } = render(
      <FieldRow label="Listen address" doc="Where the server listens." source="config" hasControl={false}>
        <p>:8080</p>
      </FieldRow>,
    );
    expect(screen.getByText("Listen address").tagName).toBe("SPAN");
    expect(container.querySelector("label")).toBeNull();
    expect(container.querySelector("[for]")).toBeNull();
    expect(screen.getByText(":8080")).toBeInTheDocument();
  });

  it("labels each kind of control", () => {
    render(
      <>
        <FieldRow label="Mode" source="config">
          <SelectField value="b" options={["a", "b"]} onChange={vi.fn()} />
        </FieldRow>
        <FieldRow label="API key" source="config">
          <SecretField hasValue={false} value="" onChange={vi.fn()} />
        </FieldRow>
        <FieldRow label="Enabled" source="config">
          <ToggleField checked onChange={vi.fn()} />
        </FieldRow>
      </>,
    );
    expect(screen.getByLabelText("Mode")).toHaveValue("b");
    expect(screen.getByLabelText("API key")).toHaveAttribute("type", "password");
    expect(screen.getByRole("switch", { name: "Enabled" })).toHaveAttribute("aria-checked", "true");
  });
});

describe("form fields", () => {
  it("TextField and SelectField report changes", async () => {
    const onText = vi.fn();
    const onSelect = vi.fn();
    render(
      <>
        <TextField value="" onChange={onText} placeholder="type here" />
        <SelectField value="a" options={["a", "b"]} onChange={onSelect} />
      </>,
    );
    await userEvent.type(screen.getByPlaceholderText("type here"), "x");
    expect(onText).toHaveBeenCalledWith("x");
    await userEvent.selectOptions(screen.getByRole("combobox"), "b");
    expect(onSelect).toHaveBeenCalledWith("b");
  });

  it("NumberField renders NaN as empty and reports numbers", async () => {
    function Harness() {
      const [v, setV] = useState(Number.NaN);
      return <NumberField value={v} onChange={setV} />;
    }
    render(<Harness />);
    const input = screen.getByRole("spinbutton");
    expect(input).toHaveValue(null);
    await userEvent.type(input, "42");
    expect(input).toHaveValue(42);
  });

  it("ToggleField flips its value and respects disabled", async () => {
    const onChange = vi.fn();
    const { rerender } = render(<ToggleField checked={false} onChange={onChange} />);
    await userEvent.click(screen.getByRole("switch"));
    expect(onChange).toHaveBeenCalledWith(true);

    onChange.mockClear();
    rerender(<ToggleField checked={false} onChange={onChange} disabled />);
    await userEvent.click(screen.getByRole("switch"));
    expect(onChange).not.toHaveBeenCalled();
  });

  it("SecretField hides by default, can be revealed, and generates a random value", async () => {
    function Harness() {
      const [v, setV] = useState("");
      return <SecretField hasValue value={v} onChange={setV} generatable />;
    }
    render(<Harness />);
    const input = screen.getByPlaceholderText(/enter a new value to replace/i);
    expect(input).toHaveAttribute("type", "password");

    await userEvent.click(screen.getByRole("button", { name: "Show" }));
    expect(input).toHaveAttribute("type", "text");
    await userEvent.click(screen.getByRole("button", { name: "Hide" }));
    expect(input).toHaveAttribute("type", "password");

    await userEvent.click(screen.getByRole("button", { name: "Generate" }));
    // 32 random bytes, base64url without padding, revealed so it can be copied.
    expect((input as HTMLInputElement).value).toMatch(/^[A-Za-z0-9_-]{43}$/);
    expect(input).toHaveAttribute("type", "text");
  });
});
