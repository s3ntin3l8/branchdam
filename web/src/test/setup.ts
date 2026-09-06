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

// jsdom doesn't implement IntersectionObserver, which the SettingsLayout
// uses for scroll-spy active-state tracking. A minimal stub that fires
// the callback immediately is sufficient for tests.
class IntersectionObserverStub {
  callback: IntersectionObserverCallback;
  constructor(callback: IntersectionObserverCallback, options?: IntersectionObserverInit) {
    void options;
    this.callback = callback;
  }
  observe() {}
  unobserve() {}
  disconnect() {}
}
// eslint-disable-next-line @typescript-eslint/no-explicit-any
(globalThis as any).IntersectionObserver = IntersectionObserverStub;

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

/*
 * In-memory localStorage polyfill. The configured jsdom environment in this
 * project doesn't expose window.localStorage, but useTheme() (and any future
 * browser-storage-backed feature) reads/writes it on mount. Tests that
 * exercise storage behavior install this stub -- production code hits a
 * real localStorage in the browser. The shape mirrors the spec:
 * string keys, string values, no expiry, throws on QuotaExceededError.
 */
class MemoryStorage implements Storage {
  private store = new Map<string, string>();

  get length(): number {
    return this.store.size;
  }

  key(index: number): string | null {
    return Array.from(this.store.keys())[index] ?? null;
  }

  getItem(key: string): string | null {
    return this.store.has(key) ? this.store.get(key)! : null;
  }

  setItem(key: string, value: string): void {
    this.store.set(key, String(value));
  }

  removeItem(key: string): void {
    this.store.delete(key);
  }

  clear(): void {
    this.store.clear();
  }
}

const memoryStorage = new MemoryStorage();
try {
  if (typeof window !== "undefined" && typeof window.localStorage === "undefined") {
    Object.defineProperty(window, "localStorage", {
      configurable: true,
      writable: true,
      value: memoryStorage,
    });
  }
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  if (typeof (globalThis as any).localStorage === "undefined") {
    // eslint-disable-next-line @typescript-eslint/no-explicit-any
    (globalThis as any).localStorage = memoryStorage;
  }
} catch {
  /* setup is best-effort -- individual tests may need to install their own */
}
