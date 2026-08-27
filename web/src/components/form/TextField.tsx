import { INPUT_CLASS } from "./inputStyles";

interface TextFieldProps {
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  placeholder?: string;
}

export function TextField({ value, onChange, disabled, placeholder }: TextFieldProps) {
  return (
    <input
      type="text"
      value={value}
      onChange={(e) => onChange(e.target.value)}
      disabled={disabled}
      placeholder={placeholder}
      className={INPUT_CLASS}
    />
  );
}
