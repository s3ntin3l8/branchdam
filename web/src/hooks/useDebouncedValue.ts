import { useEffect, useState } from "react";

// useDebouncedValue returns value once it has stopped changing for delayMs.
// The server-side search boxes (node picker, Users page) key their queries
// on the debounced value so a burst of keystrokes is one request, not one
// per character.
export function useDebouncedValue<T>(value: T, delayMs = 300): T {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = setTimeout(() => setDebounced(value), delayMs);
    return () => clearTimeout(timer);
  }, [value, delayMs]);
  return debounced;
}
