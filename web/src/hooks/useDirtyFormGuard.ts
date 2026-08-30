import { createContext, useCallback, useState } from "react";
import { useBlocker, useBeforeUnload } from "react-router";

export const DirtyFormContext = createContext<{
  dirtyCount: number;
  register: (key: string) => { markDirty: () => void; markClean: () => void };
}>({
  dirtyCount: 0,
  register: () => ({ markDirty: () => {}, markClean: () => {} }),
});

export function useDirtyFormProvider() {
  const [dirtyFields, setDirtyFields] = useState(new Set<string>());

  const markDirty = useCallback((key: string) => {
    setDirtyFields((prev) => {
      if (prev.has(key)) return prev;
      const next = new Set(prev);
      next.add(key);
      return next;
    });
  }, []);

  const markClean = useCallback((key: string) => {
    setDirtyFields((prev) => {
      if (!prev.has(key)) return prev;
      const next = new Set(prev);
      next.delete(key);
      return next;
    });
  }, []);

  const register = useCallback(
    (key: string) => ({
      markDirty: () => markDirty(key),
      markClean: () => markClean(key),
    }),
    [markDirty, markClean],
  );

  return { dirtyCount: dirtyFields.size, register };
}

export function useDirtyGuard(dirtyCount: number) {
  useBeforeUnload((e) => {
    if (dirtyCount > 0) {
      e.preventDefault();
    }
  });

  const blocker = useBlocker(
    ({ currentLocation, nextLocation }) =>
      dirtyCount > 0 && currentLocation.pathname !== nextLocation.pathname
  );

  return blocker;
}
