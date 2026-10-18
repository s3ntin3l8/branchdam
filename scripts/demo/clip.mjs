// Clip rects for element screenshots, kept free of side effects so that
// `make test-demo` (browser-free) can cover the math.

/** Page background left around an element shot, in CSS pixels. */
export const ELEMENT_MARGIN = 24;

/**
 * `box` (a viewport-relative bounding box) grown by `margin` on every side,
 * moved into document coordinates with `doc.scrollX/scrollY`, and clamped to
 * the `doc.width` x `doc.height` document.
 */
export function paddedRect(box, doc, margin = ELEMENT_MARGIN) {
  const left = box.x + doc.scrollX - margin;
  const top = box.y + doc.scrollY - margin;
  const right = Math.min(left + box.width + 2 * margin, doc.width);
  const bottom = Math.min(top + box.height + 2 * margin, doc.height);
  const x = Math.max(0, left);
  const y = Math.max(0, top);
  return { x, y, width: right - x, height: bottom - y };
}

/**
 * Clip for `page.screenshot({ fullPage: true, clip })` around `locator`.
 * The rect is in document coordinates, which Playwright only uses with
 * `fullPage: true`; without it the clip is viewport-relative and the scroll
 * offset would be counted twice.
 */
export async function paddedClip(page, locator, margin = ELEMENT_MARGIN) {
  await locator.scrollIntoViewIfNeeded();
  const box = await locator.boundingBox();
  if (!box) throw new Error("element to capture is not visible");
  const doc = await page.evaluate(() => ({
    scrollX: window.scrollX,
    scrollY: window.scrollY,
    width: document.documentElement.scrollWidth,
    height: document.documentElement.scrollHeight,
  }));
  return paddedRect(box, doc, margin);
}
