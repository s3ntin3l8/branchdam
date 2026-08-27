import { INPUT_CLASS } from "./inputStyles";

interface NumberFieldProps {
  value: number;
  onChange: (value: number) => void;
  disabled?: boolean;
}

// onChange fires even when the input is momentarily empty (valueAsNumber ===
// NaN) -- the caller (SettingsFieldEditor) is responsible for disabling Save
// on a NaN draft rather than this component silently clamping or rejecting
// keystrokes, which would make clearing the field to type a new number
// impossible.
export function NumberField({ value, onChange, disabled }: NumberFieldProps) {
  return (
    <input
      type="number"
      value={Number.isNaN(value) ? "" : value}
      onChange={(e) => onChange(e.target.valueAsNumber)}
      disabled={disabled}
      className={INPUT_CLASS}
    />
  );
}
