import { useState } from "react";
import { INPUT_CLASS } from "./inputStyles";
import { useFieldIds } from "./fieldContext";

// StringListField edits a stringList setting (http.trustedProxies,
// agent.skipSignaturePaths, ...): one row per entry with a Remove button,
// and an input that adds an entry on Enter or the Add button. Entries are
// trimmed; blanks and exact duplicates are not added. The server still
// validates each entry (e.g. trustedProxies must be IPs or CIDRs).
interface StringListFieldProps {
  value: string[];
  onChange: (value: string[]) => void;
  disabled?: boolean;
  placeholder?: string;
}

export function StringListField({ value, onChange, disabled, placeholder }: StringListFieldProps) {
  const ids = useFieldIds();
  const [entry, setEntry] = useState("");
  const trimmed = entry.trim();
  const duplicate = trimmed !== "" && value.includes(trimmed);

  const add = () => {
    if (!trimmed || duplicate) return;
    onChange([...value, trimmed]);
    setEntry("");
  };

  return (
    <div className="space-y-2">
      {value.length === 0 ? (
        <p className="text-xs text-neutral-500">No entries.</p>
      ) : (
        <ul className="space-y-1">
          {value.map((item, i) => (
            <li
              key={`${item}-${i}`}
              className="flex items-center justify-between gap-2 rounded border border-neutral-800 bg-neutral-950/60 px-2 py-1"
            >
              <code className="break-all text-xs text-neutral-200">{item}</code>
              <button
                type="button"
                onClick={() => onChange(value.filter((_, j) => j !== i))}
                disabled={disabled}
                aria-label={`Remove ${item}`}
                className="shrink-0 rounded px-1.5 py-0.5 text-xs text-red-400 hover:bg-red-950/40 disabled:opacity-50"
              >
                Remove
              </button>
            </li>
          ))}
        </ul>
      )}
      <div className="flex gap-2">
        <input
          id={ids?.controlId}
          aria-describedby={ids?.docId}
          type="text"
          value={entry}
          onChange={(e) => setEntry(e.target.value)}
          onKeyDown={(e) => {
            if (e.key === "Enter") {
              e.preventDefault();
              add();
            }
          }}
          disabled={disabled}
          placeholder={placeholder ?? "Add an entry"}
          aria-invalid={duplicate}
          className={INPUT_CLASS}
        />
        <button
          type="button"
          onClick={add}
          disabled={disabled || !trimmed || duplicate}
          className="shrink-0 rounded border border-neutral-700 bg-neutral-800 px-3 py-1.5 text-xs font-medium text-neutral-200 hover:bg-neutral-700 disabled:opacity-50"
        >
          Add
        </button>
      </div>
      {duplicate && <p className="text-xs text-amber-400">Already in the list.</p>}
    </div>
  );
}
