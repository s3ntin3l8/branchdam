import { INPUT_CLASS } from "./inputStyles";

interface SelectFieldProps {
  value: string;
  options: string[];
  onChange: (value: string) => void;
  disabled?: boolean;
}

export function SelectField({ value, options, onChange, disabled }: SelectFieldProps) {
  return (
    <select value={value} onChange={(e) => onChange(e.target.value)} disabled={disabled} className={INPUT_CLASS}>
      {options.map((opt) => (
        <option key={opt} value={opt}>
          {opt}
        </option>
      ))}
    </select>
  );
}
