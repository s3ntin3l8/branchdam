import { createContext, useRef, useState } from "react";
import { useBlocker, useBeforeUnload } from "react-router";

export const DirtyFormContext = createContext<{
  dirtyCount: number;
  register: () => { markDirty: () => void; markClean: () => void };
}>({
  dirtyCount: 0,
  register: () => ({ markDirty: () => {}, markClean: () => {} }),
});

export function useDirtyFormProvider() {
  const [dirtyCount, setDirtyCount] = useState(0);
  const countRef = useRef(0);

  const register = () => {
    let isDirty = false;
    return {
      markDirty: () => {
        if (!isDirty) {
          isDirty = true;
          countRef.current += 1;
          setDirtyCount(countRef.current);
        }
      },
      markClean: () => {
        if (isDirty) {
          isDirty = false;
          countRef.current = Math.max(0, countRef.current - 1);
          setDirtyCount(countRef.current);
        }
      },
    };
  };

  return { dirtyCount, register };
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
