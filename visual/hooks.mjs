// App hooks for visual-kit (CONTRACT.md "visual/scenes.json"). They run inside the visual-kit image and reach the
// demo over HTTP only.
//
// The demo's server runs in forward-auth mode, so there is no login: the browser sends the headers an auth proxy
// would. The theme needs no hook either: the app follows `prefers-color-scheme` until a stored choice says
// otherwise, and visual-kit emulates the scheme of each shot.
import { DEMO_HEADERS } from "../scripts/demo/headers.mjs";

export async function beforeScene({ context }) {
  await context.setExtraHTTPHeaders(DEMO_HEADERS);
}

export async function settle({ page, scene }) {
  // Thumbnails are lazy (`loading="lazy"`) and fade in. A full-page shot would catch the ones below the fold unloaded,
  // so scroll the whole page once, then wait until every image is in.
  await page.evaluate(async () => {
    const step = Math.max(window.innerHeight / 2, 200);
    for (let y = 0; y < document.documentElement.scrollHeight; y += step) {
      window.scrollTo(0, y);
      await new Promise((resolve) => setTimeout(resolve, 150));
    }
    window.scrollTo(0, 0);
  });
  await page.evaluate(() => document.fonts.ready);
  // A lazy image outside the viewport (in an inner scroll area, or a variant hidden at this width) never starts
  // loading, so it would never be complete: load them all.
  await page.evaluate(() => {
    for (const img of document.querySelectorAll('img[loading="lazy"]')) img.loading = "eager";
  });
  // An image that finished with an error is settled too: assets with no thumbnail answer 404, and the app shows
  // its fallback. Only one that is still loading has to be waited for.
  await page
    .waitForFunction(() => [...document.images].every((img) => img.complete), null, {
      timeout: 30_000,
      polling: 250,
    })
    .catch(async () => {
      const loading = await page.evaluate(() =>
        [...document.images].filter((img) => !img.complete).map((img) => img.currentSrc || img.src),
      );
      throw new Error(
        `${scene.name}: ${loading.length} image(s) still loading after 30s (${loading.slice(0, 2).join(", ")})`,
      );
    });

  // The thumbnails fade in, the version graph lays itself out, and the lists refetch on an interval: wait until every
  // element has held its box for 1.5 s.
  await page
    .evaluate(async () => {
      const boxes = () =>
        [...document.querySelectorAll("body *")]
          .map((e) => {
            const r = e.getBoundingClientRect();
            return [r.x, r.y, r.width, r.height].map((n) => n.toFixed(2)).join(",");
          })
          .join("|");
      let last = boxes();
      let steady = 0;
      for (let i = 0; i < 80; i++) {
        await new Promise((resolve) => setTimeout(resolve, 250));
        const now = boxes();
        steady = now === last ? steady + 1 : 0;
        if (steady >= 6) return;
        last = now;
      }
      throw new Error("the page keeps moving");
    })
    .catch((err) => {
      throw new Error(`${scene.name}: ${err.message.split("\n")[0]}`);
    });
}
