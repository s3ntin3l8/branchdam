#!/usr/bin/env node
// Reproducible screenshots from an isolated demo instance (scripts/demo/instance.mjs).
//
//   make screenshots                       # docs/screenshots/*.png (dark + light), used by the README
//   make screenshots-landing OUT=<dir>     # landing-page stills + a short loop (needs ffmpeg)
//
// The Playwright dependency lives in web/ (devDependency); browsers come from
// the normal Playwright cache (`npx playwright install chromium` once).
import { spawnSync } from "node:child_process";
import { createRequire } from "node:module";
import { mkdirSync, mkdtempSync, readdirSync, renameSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import path from "node:path";
import { fileURLToPath } from "node:url";
import { parseArgs } from "node:util";
import { DEMO_HEADERS, startDemo } from "./instance.mjs";

const ROOT = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const { chromium } = createRequire(path.join(ROOT, "web/package.json"))("@playwright/test");

const { values } = parseArgs({
  options: {
    profile: { type: "string", default: "readme" },
    out: { type: "string" },
    keep: { type: "boolean", default: false },
  },
});
if (!["readme", "landing"].includes(values.profile))
  throw new Error(`unknown --profile "${values.profile}" (readme | landing)`);
if (values.profile === "landing" && !values.out) throw new Error("--profile landing needs --out <dir>");

const README_DIR = path.join(ROOT, "docs/screenshots");
/** The richest version graph in the dataset: RAW, camera JPEG, graded export, EDL, proxy, review links. */
const HERO_FILE = "DSC01101.ARW";

/**
 * The only writer of the `branchdam.theme` key in a capture. `colorScheme` and
 * the stored theme must agree, so only an explicit dark or light is accepted
 * (no "system", which would let the OS emulation decide).
 */
async function newContext(browser, theme, options) {
  if (theme !== "dark" && theme !== "light") throw new Error(`capture theme must be dark or light, got ${theme}`);
  const context = await browser.newContext({ colorScheme: theme, extraHTTPHeaders: DEMO_HEADERS, ...options });
  // useTheme.ts reads a JSON string from this key before first paint.
  await context.addInitScript(
    (t) => globalThis.localStorage.setItem("branchdam.theme", JSON.stringify(t)),
    theme,
  );
  return context;
}

async function open(page, url, scrollTo) {
  await page.goto(url, { waitUntil: "networkidle" });
  await page.evaluate(() => document.fonts.ready);
  if (scrollTo) {
    await page.getByText(scrollTo, { exact: true }).evaluate((el) => el.scrollIntoView({ block: "start" }));
  }
  await page.waitForTimeout(1200); // thumbnails fade in, the graph lays out
}

/** The graph sits at the bottom of the detail page, so it's captured as its own element. */
function shot(page, dir, name, element) {
  mkdirSync(dir, { recursive: true });
  const tmp = path.join(dir, `.${name}`);
  const target = element
    ? page.locator("section").filter({ has: page.getByText(element, { exact: true }) }).last()
    : page;
  return target.screenshot({ path: tmp }).then(() => {
    renameSync(tmp, path.join(dir, name));
    console.log(`wrote ${name}`);
  });
}

async function heroAssetId(demo) {
  const body = await demo.api("/api/v1/assets?limit=500");
  const assets = body.assets ?? body.items ?? body;
  const hero = assets.find((a) => a.fileName === HERO_FILE);
  if (!hero) throw new Error(`demo asset ${HERO_FILE} not found`);
  return hero.id;
}

function pages(demo, heroId) {
  return [
    { name: "assets", url: `${demo.url}/assets` },
    { name: "asset-detail", url: `${demo.url}/assets/${heroId}` },
    { name: "asset-graph", url: `${demo.url}/assets/${heroId}`, scrollTo: "Multi-hop Lineage Graph" },
    { name: "audit-queue", url: `${demo.url}/audit` },
    { name: "storage-health", url: `${demo.url}/storage-health` },
    { name: "jobs", url: `${demo.url}/jobs` },
  ];
}

async function captureReadme(browser, demo, heroId) {
  for (const theme of ["dark", "light"]) {
    const context = await newContext(browser, theme, { viewport: { width: 1440, height: 900 } });
    const page = await context.newPage();
    for (const p of pages(demo, heroId)) {
      await open(page, p.url, p.scrollTo);
      await shot(page, README_DIR, `${p.name}${theme === "light" ? "-light" : ""}.png`, p.scrollTo);
    }
    await context.close();
  }
}

function ffmpeg(args) {
  const res = spawnSync("ffmpeg", ["-y", "-loglevel", "error", ...args], { stdio: "inherit" });
  if (res.error || res.status !== 0) throw new Error(`ffmpeg ${args.join(" ")} failed`);
}

/** From the asset grid into the hero asset's version graph. */
async function recordLoop(browser, demo, heroId, outDir) {
  if (spawnSync("ffmpeg", ["-version"]).status !== 0) {
    console.warn("ffmpeg not found; skipping the landing loop");
    return;
  }
  // Recorded in viewport (CSS) pixels: the loop is 1x while the stills are 2x.
  const size = { width: 1600, height: 1000 };
  const videoDir = mkdtempSync(path.join(tmpdir(), "branchdam-loop-"));
  const context = await newContext(browser, "dark", { viewport: size, recordVideo: { dir: videoDir, size } });
  const started = Date.now();
  const page = await context.newPage();
  await open(page, `${demo.url}/assets`);
  const offset = (Date.now() - started) / 1000;
  await page.waitForTimeout(1200);
  await page.goto(`${demo.url}/assets/${heroId}`, { waitUntil: "networkidle" });
  await page.waitForTimeout(1500);
  await page
    .getByText("Multi-hop Lineage Graph", { exact: true })
    .evaluate((el) => el.scrollIntoView({ block: "start", behavior: "smooth" }));
  await page.waitForTimeout(4000);
  await context.close();

  const [raw] = readdirSync(videoDir);
  const src = path.join(videoDir, raw);
  const base = path.join(outDir, "asset-graph");
  const input = ["-ss", offset.toFixed(2), "-i", src, "-an"];
  ffmpeg([...input, "-c:v", "libvpx-vp9", "-crf", "36", "-b:v", "0", "-row-mt", "1", `${base}.webm`]);
  ffmpeg([...input, "-c:v", "libx264", "-crf", "26", "-pix_fmt", "yuv420p", "-movflags", "+faststart", `${base}.mp4`]);
  ffmpeg(["-sseof", "-0.5", "-i", src, "-frames:v", "1", "-q:v", "3", `${base}.jpg`]);
  rmSync(videoDir, { recursive: true, force: true });
  console.log("wrote asset-graph.{webm,mp4,jpg}");
}

async function captureLanding(browser, demo, heroId) {
  const outDir = path.resolve(values.out);
  const factors = [
    { name: "desktop", viewport: { width: 1600, height: 1000 }, deviceScaleFactor: 2 },
    { name: "mobile", viewport: { width: 390, height: 844 }, deviceScaleFactor: 3, isMobile: true, hasTouch: true },
  ];
  for (const { name, ...options } of factors) {
    const context = await newContext(browser, "dark", options);
    const page = await context.newPage();
    for (const p of pages(demo, heroId)) {
      await open(page, p.url, p.scrollTo);
      await shot(page, outDir, `${p.name}-${name}.png`, p.scrollTo);
    }
    await context.close();
  }
  await recordLoop(browser, demo, heroId, outDir);
}

const demo = await startDemo({ quiet: true, keep: values.keep });
const browser = await chromium.launch();
try {
  const heroId = await heroAssetId(demo);
  if (values.profile === "landing") await captureLanding(browser, demo, heroId);
  else await captureReadme(browser, demo, heroId);
} finally {
  await browser.close();
  console.log("[screenshots] tearing down the demo...");
  await demo.stop();
}
