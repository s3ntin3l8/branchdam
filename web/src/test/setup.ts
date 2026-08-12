import "@testing-library/jest-dom/vitest";

// jsdom doesn't implement ResizeObserver, which @xyflow/react relies on
// internally to size its canvas. A no-op stub is sufficient for tests that
// only assert on rendered content, not on actual layout/sizing behavior.
class ResizeObserverStub {
  observe() {}
  unobserve() {}
  disconnect() {}
}
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).ResizeObserver = ResizeObserverStub;
