import { describe, expect, it } from "vitest";
import { returnPathFrom } from "./returnPath";

describe("returnPathFrom", () => {
  it("returns the stashed in-app path", () => {
    expect(returnPathFrom({ from: "/assets/12?tab=graph#lineage" })).toBe("/assets/12?tab=graph#lineage");
  });

  it("falls back to / without usable state", () => {
    expect(returnPathFrom(null)).toBe("/");
    expect(returnPathFrom(undefined)).toBe("/");
    expect(returnPathFrom({})).toBe("/");
    expect(returnPathFrom({ from: 42 })).toBe("/");
  });

  it("refuses anything that could leave the origin", () => {
    expect(returnPathFrom({ from: "https://evil.example/" })).toBe("/");
    expect(returnPathFrom({ from: "//evil.example/" })).toBe("/");
    expect(returnPathFrom({ from: "/\\evil.example/" })).toBe("/");
  });

  it("never returns to /login itself", () => {
    expect(returnPathFrom({ from: "/login" })).toBe("/");
    expect(returnPathFrom({ from: "/login?next=1" })).toBe("/");
  });
});
