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

// Mock HTMLElement offset dimensions for ReactFlow canvas sizing in jsdom
Object.defineProperty(HTMLElement.prototype, "offsetHeight", {
  configurable: true,
  value: 500,
});
Object.defineProperty(HTMLElement.prototype, "offsetWidth", {
  configurable: true,
  value: 800,
});

// Intercept and suppress non-fatal @xyflow/react dev container warnings during test runs
const originalConsoleError = console.error;
console.error = (...args: unknown[]) => {
  const msg = typeof args[0] === "string" ? args[0] : "";
  if (msg.includes("[React Flow]: The parent container needs a width and a height")) {
    return;
  }
  originalConsoleError(...args);
};
