// Clip math for the demo capture's element shots. Run: make test-demo
import assert from "node:assert/strict";
import { describe, it } from "node:test";
import { ELEMENT_MARGIN, paddedRect } from "./clip.mjs";

const doc = { scrollX: 0, scrollY: 0, width: 1440, height: 3000 };

describe("paddedRect", () => {
  it("grows the box by the margin on every side", () => {
    assert.deepEqual(paddedRect({ x: 300, y: 200, width: 1000, height: 400 }, doc), {
      x: 300 - ELEMENT_MARGIN,
      y: 200 - ELEMENT_MARGIN,
      width: 1000 + 2 * ELEMENT_MARGIN,
      height: 400 + 2 * ELEMENT_MARGIN,
    });
  });

  it("moves a viewport-relative box into document coordinates", () => {
    const r = paddedRect({ x: 300, y: 0, width: 1000, height: 400 }, { ...doc, scrollY: 1800 }, 10);
    assert.deepEqual(r, { x: 290, y: 1790, width: 1020, height: 420 });
  });

  it("clamps a negative origin and keeps the far edge", () => {
    const r = paddedRect({ x: 5, y: 0, width: 100, height: 50 }, doc, 10);
    assert.deepEqual(r, { x: 0, y: 0, width: 115, height: 60 });
  });

  it("clamps to the document's right and bottom edges", () => {
    const r = paddedRect({ x: 1400, y: 2980, width: 40, height: 20 }, doc, 10);
    assert.deepEqual(r, { x: 1390, y: 2970, width: 50, height: 30 });
  });
});
