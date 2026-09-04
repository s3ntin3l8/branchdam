import { describe, expect, it } from "vitest";
import { formatBytes } from "./formatBytes";

describe("formatBytes", () => {
  it("formats zero and negative values as '0 B'", () => {
    expect(formatBytes(0)).toBe("0 B");
    expect(formatBytes(-1)).toBe("0 B");
    expect(formatBytes(-1024)).toBe("0 B");
    expect(formatBytes(NaN)).toBe("0 B");
    expect(formatBytes(Infinity)).toBe("0 B");
  });

  it("formats bytes without decimal point", () => {
    expect(formatBytes(1)).toBe("1 B");
    expect(formatBytes(500)).toBe("500 B");
    expect(formatBytes(1023)).toBe("1023 B");
  });

  it("formats kilobytes correctly", () => {
    expect(formatBytes(1024)).toBe("1 KB");
    expect(formatBytes(1536)).toBe("1.5 KB");
    expect(formatBytes(2048)).toBe("2 KB");
  });

  it("formats megabytes correctly", () => {
    expect(formatBytes(1024 * 1024)).toBe("1 MB");
    expect(formatBytes(1024 * 1024 * 2.5)).toBe("2.5 MB");
  });

  it("formats gigabytes, terabytes, and petabytes correctly", () => {
    expect(formatBytes(1024 ** 3)).toBe("1 GB");
    expect(formatBytes(1024 ** 4)).toBe("1 TB");
    expect(formatBytes(1024 ** 5)).toBe("1 PB");
  });

  it("handles numbers larger than petabytes gracefully", () => {
    expect(formatBytes(1024 ** 6)).toBe("1024 PB");
  });
});
