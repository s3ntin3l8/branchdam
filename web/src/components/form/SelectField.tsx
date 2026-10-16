import { INPUT_CLASS } from "./inputStyles";
import { useFieldIds } from "./fieldContext";

interface SelectFieldProps {
  value: string;
  options: string[];
  onChange: (value: string) => void;
  disabled?: boolean;
}

export function SelectField({ value, options, onChange, disabled }: SelectFieldProps) {
  const ids = useFieldIds();
  return (
    <select
      id={ids?.controlId}
      aria-describedby={ids?.docId}
      value={value}
      onChange={(e) => onChange(e.target.value)}
      disabled={disabled}
      className={INPUT_CLASS}
    >
      {options.map((opt) => (
        <option key={opt} value={opt}>
          {opt}
        </option>
      ))}
    </select>
  );
}
