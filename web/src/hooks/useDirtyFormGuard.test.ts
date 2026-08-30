import { act, renderHook } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { useDirtyFormProvider } from "./useDirtyFormGuard";

describe("useDirtyFormProvider", () => {
  it("starts with dirtyCount=0", () => {
    const { result } = renderHook(() => useDirtyFormProvider());
    expect(result.current.dirtyCount).toBe(0);
  });

  it("increments dirtyCount when a field is marked dirty", () => {
    const { result } = renderHook(() => useDirtyFormProvider());
    const handle = result.current.register("field-a");
    act(() => handle.markDirty());
    expect(result.current.dirtyCount).toBe(1);
  });

  it("markDirty is idempotent per key: re-marking the same field does not double-count", () => {
    const { result } = renderHook(() => useDirtyFormProvider());
    const handle = result.current.register("field-a");
    act(() => {
      handle.markDirty();
      handle.markDirty();
      handle.markDirty();
    });
    expect(result.current.dirtyCount).toBe(1);
  });

  it("tracks distinct keys independently", () => {
    const { result } = renderHook(() => useDirtyFormProvider());
    const a = result.current.register("field-a");
    const b = result.current.register("field-b");
    act(() => {
      a.markDirty();
      b.markDirty();
    });
    expect(result.current.dirtyCount).toBe(2);
  });

  it("markClean decrements dirtyCount", () => {
    const { result } = renderHook(() => useDirtyFormProvider());
    const a = result.current.register("field-a");
    const b = result.current.register("field-b");
    act(() => {
      a.markDirty();
      b.markDirty();
    });
    expect(result.current.dirtyCount).toBe(2);
    act(() => a.markClean());
    expect(result.current.dirtyCount).toBe(1);
  });

  it("markClean is idempotent: cleaning a clean field does not go negative", () => {
    const { result } = renderHook(() => useDirtyFormProvider());
    const a = result.current.register("field-a");
    act(() => {
      a.markClean();
      a.markClean();
    });
    expect(result.current.dirtyCount).toBe(0);
  });

  it("dirtyCount converges to 0 after all fields are cleaned", () => {
    const { result } = renderHook(() => useDirtyFormProvider());
    const a = result.current.register("field-a");
    const b = result.current.register("field-b");
    act(() => {
      a.markDirty();
      b.markDirty();
    });
    act(() => {
      a.markClean();
      b.markClean();
    });
    expect(result.current.dirtyCount).toBe(0);
  });

  it("two separate components with the same key are not double-counted", () => {
    // Same key, two register() calls: the Set deduplicates, so
    // dirtyCount reflects unique dirty fields, not mark-dirty calls.
    const { result } = renderHook(() => useDirtyFormProvider());
    const a1 = result.current.register("shared-key");
    const a2 = result.current.register("shared-key");
    act(() => {
      a1.markDirty();
      a2.markDirty();
    });
    expect(result.current.dirtyCount).toBe(1);
  });
});
