/// <reference types="node" />
import { describe, expect, it } from "vitest";
import { readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, resolve } from "node:path";

/*
 * Static analysis of the theme CSS file. Verifies the dual-theme token
 * contract -- every key token used by Tailwind utilities in JSX must be
 * declared under BOTH `:root[data-theme="dark"]` and
 * `:root[data-theme="light"]`, with distinct values per theme.
 *
 * This is the closest equivalent to a contrast/per-pixel assertion we can
 * make from vitest (jsdom doesn't resolve CSS custom properties through
 * getComputedStyle), but it catches the same class of bug as the earlier
 * "missing light theme token = silent fallback to Tailwind default" issue.
 * Without this test, deleting one branch of theme.css -- or accidentally
 * making the two themes identical -- would not be caught by any unit test.
 *
 * The triple-slash reference at the top pulls in Node's typing for this
 * file only -- the project's tsconfig.app.json keeps the browser-shaped
 * lib set so production code doesn't accidentally pick up Node-only APIs.
 * (Vitest runs in Node, so `node:fs` / `node:url` / `node:path` are
 * available at runtime; we just need the types.)
 */

const here = dirname(fileURLToPath(import.meta.url));
const themeCssPath = resolve(here, "./theme.css");
const themeCss = readFileSync(themeCssPath, "utf8");

/*
 * Pull the body of each top-level `:root[...]` block. We do this with a
 * simple brace-counting scan rather than a regex so the selector can span
 * newlines (`:root,\n:root[data-theme="dark"]`) without tripping the
 * matcher. Comments are skipped so they can't accidentally close a block.
 */
function extractRootBlocks(css: string): Array<{ selector: string; body: string }> {
  const out: Array<{ selector: string; body: string }> = [];
  let i = 0;
  while (i < css.length) {
    // Find the next ":root" that isn't inside a comment or string.
    const idx = css.indexOf(":root", i);
    if (idx === -1) break;
    // Walk back to the start of the rule: skip preceding whitespace, but
    // bail out if we're inside a /* comment */.
    const before = css.slice(0, idx);
    const lastOpen = before.lastIndexOf("/*");
    const lastClose = before.lastIndexOf("*/");
    if (lastOpen > lastClose) {
      // We're inside a comment; skip past it and continue.
      const close = css.indexOf("*/", idx);
      if (close === -1) break;
      i = close + 2;
      continue;
    }
    // Walk forward to the opening brace.
    const openBrace = css.indexOf("{", idx);
    if (openBrace === -1) break;
    const selector = css.slice(idx, openBrace).trim();
    // Count braces to find the matching close.
    let depth = 1;
    let j = openBrace + 1;
    while (j < css.length && depth > 0) {
      const ch = css[j];
      if (ch === "{") depth++;
      else if (ch === "}") depth--;
      j++;
    }
    if (depth !== 0) break;
    const body = css.slice(openBrace + 1, j - 1);
    out.push({ selector, body });
    i = j;
  }
  return out;
}

function tokensIn(body: string): Map<string, string> {
  const out = new Map<string, string>();
  const re = /(--[a-z0-9-]+)\s*:\s*([^;]+);/gi;
  let m: RegExpExecArray | null;
  while ((m = re.exec(body)) !== null) {
    out.set(m[1].toLowerCase(), m[2].trim());
  }
  return out;
}

const blocks = extractRootBlocks(themeCss);

const darkBlocks = blocks.filter((b) => /\[data-theme="dark"\]|:root,/.test(b.selector));
const lightBlocks = blocks.filter((b) => /\[data-theme="light"\]/.test(b.selector));

const darkTokens = new Map<string, string>();
for (const b of darkBlocks) for (const [k, v] of tokensIn(b.body)) darkTokens.set(k, v);
const lightTokens = new Map<string, string>();
for (const b of lightBlocks) for (const [k, v] of tokensIn(b.body)) lightTokens.set(k, v);

describe("theme.css dual-theme contract", () => {
  it("declares at least one dark theme block", () => {
    expect(darkBlocks.length).toBeGreaterThan(0);
  });

  it("declares at least one light theme block", () => {
    expect(lightBlocks.length).toBeGreaterThan(0);
  });

  // The Tailwind utility classes used in JSX all resolve to these tokens.
  // If any token is missing from one theme, the corresponding utility
  // silently falls back to Tailwind's built-in default -- which would give
  // us the dark value in light mode (or vice versa).
  const requiredTokens = [
    "--color-neutral-50",
    "--color-neutral-100",
    "--color-neutral-200",
    "--color-neutral-300",
    "--color-neutral-400",
    "--color-neutral-500",
    "--color-neutral-600",
    "--color-neutral-700",
    "--color-neutral-800",
    "--color-neutral-900",
    "--color-neutral-950",
    "--color-indigo-500",
    "--color-indigo-600",
    "--color-amber-600",
    "--color-amber-700",
    "--color-amber-800",
    "--color-amber-900",
    "--color-amber-950",
    "--color-red-300",
    "--color-red-400",
    "--color-red-700",
    "--color-red-800",
    "--color-red-900",
    "--color-red-950",
    "--color-emerald-700",
    "--color-emerald-950",
    "--color-sky-500",
    "--color-sky-600",
    "--color-sky-700",
    "--color-sky-950",
    "--color-blue-400",
    "--color-blue-900",
    "--color-blue-950",
    "--color-brand",
    "--surface-canvas",
    "--surface-raised",
    "--border-subtle",
    "--text-primary",
    "--text-secondary",
    "--text-muted",
    "--graph-node-bg-root",
    "--graph-node-bg-raw",
    "--graph-node-bg-sidecar",
    "--graph-node-bg-export",
    "--graph-node-bg-neutral",
    "--graph-node-border-root",
    "--graph-node-border-raw",
    "--graph-node-border-sidecar",
    "--graph-node-border-export",
    "--graph-node-border-neutral",
    "--graph-edge-derived",
    "--graph-edge-final",
    "--graph-edge-proxy",
    "--graph-edge-sidecar",
    "--graph-edge-duplicate",
    "--graph-edge-default",
    "--graph-text",
  ];

  for (const token of requiredTokens) {
    it(`declares ${token} in dark theme`, () => {
      expect(darkTokens.has(token)).toBe(true);
    });
    it(`declares ${token} in light theme`, () => {
      expect(lightTokens.has(token)).toBe(true);
    });
  }

  // Per-theme invertibility contract: in light mode the neutral ladder
  // should resolve to a *light* value, while in dark mode it should resolve
  // to a *dark* value. This is the cheapest invariant that catches "I
  // accidentally pasted the same hex into both themes" regressions. The
  // semantic surface tokens (--surface-*, --text-*) are aliases of the
  // neutral ladder, so we only need to assert the source-of-truth stops.
  describe("light/dark token divergence", () => {
    const tokensThatMustDiffer = [
      "--color-neutral-950",
      "--color-neutral-900",
      "--color-neutral-100",
    ];

    for (const token of tokensThatMustDiffer) {
      it(`${token} resolves to different values per theme`, () => {
        const dark = darkTokens.get(token);
        const light = lightTokens.get(token);
        expect(dark, `missing ${token} in dark theme`).toBeDefined();
        expect(light, `missing ${token} in light theme`).toBeDefined();
        expect(dark).not.toBe(light);
      });
    }

    it("--color-neutral-950 in light mode is a near-white hex (page background)", () => {
      const light = lightTokens.get("--color-neutral-950");
      expect(light).toBeDefined();
      expect(light).toMatch(/^#[0-9a-fA-F]{3,6}$/);
      const r = parseInt(light!.slice(1, 3), 16);
      const g = parseInt(light!.slice(3, 5), 16);
      const b = parseInt(light!.slice(5, 7), 16);
      expect(r + g + b).toBeGreaterThan(600);
    });

    it("--color-neutral-100 in light mode is dark (primary text contrast on light page)", () => {
      const light = lightTokens.get("--color-neutral-100");
      expect(light).toMatch(/^#[0-9a-fA-F]{3,6}$/);
      const r = parseInt(light!.slice(1, 3), 16);
      const g = parseInt(light!.slice(3, 5), 16);
      const b = parseInt(light!.slice(5, 7), 16);
      expect(r + g + b).toBeLessThan(200);
    });
  });

  // Theme-invariant accent borders/edges (category identity colors) must
  // NOT differ between themes, otherwise the lineage graph's category
  // semantics shift between modes.
  const themeInvariantTokens = [
    "--graph-node-border-raw",
    "--graph-node-border-sidecar",
    "--graph-node-border-export",
    "--graph-edge-derived",
    "--graph-edge-final",
    "--graph-edge-proxy",
    "--graph-edge-sidecar",
    "--graph-edge-duplicate",
  ];

  for (const token of themeInvariantTokens) {
    it(`${token} stays constant across themes (category identity)`, () => {
      expect(darkTokens.get(token)).toBe(lightTokens.get(token));
    });
  }
});
