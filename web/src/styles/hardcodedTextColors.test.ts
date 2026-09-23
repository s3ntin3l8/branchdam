/// <reference types="node" />
import { describe, expect, it } from "vitest";
import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join, relative, resolve } from "node:path";

/*
 * Static analysis of JSX class strings: hard bans a small set of theme-unsafe
 * text-color tokens (`text-white` and the "always-pale" amber/red/emerald
 * stops) unless the same string also carries a theme-invariant solid
 * background from the allowlist.
 *
 * Why this exists: light theme inverts the neutral ladder, so
 * `bg-neutral-900` becomes near-white while Tailwind's `text-white` stays
 * `#fff` -- white-on-light dialog body text was invisible. The same shape
 * of bug also bites the *pale* stops that don't flip in either theme:
 * `text-amber-100`, `text-amber-200`, `text-red-200`, and the
 * `text-emerald-200` stop (declared in neither theme, so it falls back to
 * Tailwind's pale default). Sitting on `bg-*-950` chips -- which DO flip
 * pale in light mode -- they become pale-on-pale and just as unreadable.
 * The allowlist matches the button pattern documented in theme.css
 * (`bg-amber-600 text-white`, etc.) where the background token is
 * deliberately pinned per theme.
 *
 * Why scan every string literal, not just `className="…"` strings: the
 * earlier version missed three patterns the codebase already uses:
 *   1. module-level class constants (`RestartServerButton.tsx:35-36`)
 *   2. `className={"…"}` with a JS expression
 *   3. ternaries like `className={c ? "…text-white" : "…"}` in JSX
 * (`NodePickerModal.tsx:235-239`). Scanning every `"…"`, `'…'`, and `` `…` ``
 * literal closes all three -- a string containing one of the banned tokens
 * is checked against the allowlist no matter how it ends up in a className.
 *
 * Runs under vitest like theme.test.ts (jsdom can't resolve CSS custom
 * properties through getComputedStyle, so a source scan is the closest
 * equivalent to a contrast assertion). Node types via triple-slash ref --
 * production tsconfig keeps a browser-shaped lib set.
 */

const here = dirname(fileURLToPath(import.meta.url));
const srcRoot = resolve(here, "..");

/**
 * Theme-invariant solid backgrounds that make white text safe in both
 * themes (see theme.css comments on amber 500-800 / sky 500-700 / brand).
 * The `-900` / `-950` red/emerald stops also stay dark in both themes and
 * are documented as the "dark chip bg" tokens, so they're allowed here
 * even though we otherwise ban pale text colors -- they pair safely with
 * the dark text stops (300/400), but *not* with the always-pale stops we
 * ban below. Both rules apply independently: a banned token still needs
 * an allowed bg.
 *
 * Opacity modifiers (`bg-brand/90`) are deliberately not allowed as the
 * *sole* background -- white text needs a solid fill. The trailing
 * `(?![/\w-])` rejects `bg-indigo-600/50`, `bg-red-900/60`, etc.
 */
const ALLOWED_SOLID_BG =
  /(?:^|[\s"'`$])bg-(?:brand|indigo-600|amber-[6-9]00|red-[5-9]00|emerald-[6-8]00|sky-[567]00)(?![/\w-])/;

/** group-hover pairing: solid bg and white text must flip together. */
const ALLOWED_GROUP_HOVER_BG =
  /group-hover:bg-(?:brand|indigo-600|amber-[6-9]00|red-[5-9]00|emerald-[6-8]00|sky-[567]00)(?![/\w-])/;

/**
 * Tokens the test bans outright on a theme-aware background. `text-white`
 * never has a theme-aware variant. The *100/*200 amber and red stops and
 * the undeclared emerald-200 stop stay pale in both themes (see theme.css
 * lines 50-71 dark + 175-210 light) -- they're only legible on a dark,
 * theme-invariant chip bg, which the allowlist above covers.
 *
 * The 300/400 stops (e.g. `text-amber-300`, `text-emerald-300`) flip to
 * dark in light mode by design, so they're safe on the matching `bg-*-950`
 * chips and we don't ban them.
 */
const BANNED_TOKENS = [
  "text-white",
  "text-amber-100",
  "text-amber-200",
  "text-red-200",
  "text-emerald-200",
];

const BANNED_RE = new RegExp(
  String.raw`\b(?:(?:group-hover:|hover:)?(?:${BANNED_TOKENS.join("|")}))\b`,
);

function listSourceFiles(dir: string): string[] {
  const out: string[] = [];
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const full = join(dir, entry.name);
    if (entry.isDirectory()) {
      out.push(...listSourceFiles(full));
    } else if (
      (entry.name.endsWith(".ts") || entry.name.endsWith(".tsx")) &&
      !entry.name.endsWith(".test.ts") &&
      !entry.name.endsWith(".test.tsx")
    ) {
      out.push(full);
    }
  }
  return out;
}

/**
 * Pull every `"…"`, `'…'`, and `` `…` `` literal from source. Template
 * literals may span lines and embed `${…}` conditionals; the non-greedy
 * match stops at the first closing backtick, which is correct for the
 * single-level templates this codebase uses. We deliberately match every
 * string literal rather than only `className=` attributes, so module-level
 * class constants, ternaries, and dynamic class strings are all covered.
 */
function extractStringLiterals(src: string): string[] {
  const out: string[] = [];
  // Skip block comments (/* … */) and line comments (// …) so a comment
  // mentioning "text-black" or "text-white" doesn't get scanned as data.
  const stripped = src
    .replace(/\/\*[\s\S]*?\*\//g, " ")
    .replace(/(^|[^:\\])\/\/[^\n]*/g, "$1 ");
  const reDouble = /"([^"\\]*(?:\\.[^"\\]*)*)"/g;
  const reSingle = /'([^'\\]*(?:\\.[^'\\]*)*)'/g;
  const reTpl = /`([^`\\]*(?:\\.[^`\\]*)*)`/g;
  let m: RegExpExecArray | null;
  while ((m = reDouble.exec(stripped)) !== null) out.push(m[1]);
  while ((m = reSingle.exec(stripped)) !== null) out.push(m[1]);
  while ((m = reTpl.exec(stripped)) !== null) out.push(m[1]);
  return out;
}

const violations: string[] = [];
const allowed: string[] = [];
let textBlackCount = 0;

for (const file of listSourceFiles(srcRoot)) {
  const rel = relative(srcRoot, file).split("\\").join("/");
  const src = readFileSync(file, "utf8");

  for (const s of extractStringLiterals(src)) {
    if (/\btext-black\b/.test(s)) textBlackCount++;
    if (!BANNED_RE.test(s)) continue;
    const hasSolidBg = ALLOWED_SOLID_BG.test(s);
    const hasGroupHoverPair =
      /group-hover:(?:text-white|text-amber-100|text-amber-200|text-red-200|text-emerald-200)\b/.test(s) &&
      ALLOWED_GROUP_HOVER_BG.test(s);
    if (hasSolidBg || hasGroupHoverPair) {
      allowed.push(`${rel}: ${s.trim().slice(0, 80)}`);
    } else {
      violations.push(`${rel}: ${s.trim()}`);
    }
  }
}

describe("hardcoded theme-unsafe text colors", () => {
  it("bans text-white / pale stops without an allowlisted solid background", () => {
    expect(
      violations,
      `${BANNED_TOKENS.join(", ")} without an allowlisted solid bg (use text-neutral-100 / text-{amber,red,emerald}-300 for theme-aware highlights):\n${violations.join("\n")}`,
    ).toEqual([]);
  });

  it("keeps the documented bg-X text-white button pattern on the allowlist", () => {
    // Sanity: the scan must not ban every occurrence (empty allowlist
    // would pass the first assertion vacuously if extractStringLiterals
    // broke). Spot-check a known button pattern stays allowed -- if the
    // allowlist regex regresses this fails first.
    expect(allowed.length).toBeGreaterThan(0);
    expect(
      allowed.some(
        (a) =>
          a.startsWith("pages/LoginPage.tsx:") &&
          a.includes("bg-brand") &&
          a.includes("text-white"),
      ),
    ).toBe(true);
  });

  it("never hardcodes text-black", () => {
    expect(textBlackCount).toBe(0);
  });
});
