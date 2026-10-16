import { INPUT_CLASS } from "./inputStyles";
import { useFieldIds } from "./fieldContext";

interface TextFieldProps {
  value: string;
  onChange: (value: string) => void;
  disabled?: boolean;
  placeholder?: string;
}

export function TextField({ value, onChange, disabled, placeholder }: TextFieldProps) {
  const ids = useFieldIds();
  return (
    <input
      id={ids?.controlId}
      aria-describedby={ids?.docId}
      type="text"
      value={value}
      onChange={(e) => onChange(e.target.value)}
      disabled={disabled}
      placeholder={placeholder}
      className={INPUT_CLASS}
    />
  );
}
