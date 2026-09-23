/// <reference types="node" />
import { describe, expect, it } from "vitest";
import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join, relative, resolve } from "node:path";
import { KNOWN_SAFE } from "./hardcodedTextColors.fixture";

/*
 * Static analysis of JSX class strings: hard bans a family of theme-unsafe
 * text-color tokens -- `text-white` plus the always-pale accent stops
 * (X-50, X-100, X-200 for the accent color families) -- unless the same
 * string also carries a theme-invariant solid background from the allowlist.
 *
 * Why this exists: light theme inverts the neutral ladder, so
 * `bg-neutral-900` becomes near-white while Tailwind's `text-white` stays
 * `#fff` -- white-on-light dialog body text was invisible. The same shape
 * of bug also bites the *pale* stops that don't flip in either theme:
 * text-amber-100, text-amber-200, text-red-200, and the undeclared
 * emerald-200 stop (falls back to Tailwind's pale default). Sitting on
 * `bg-*-950` chips -- which DO flip pale in light mode -- they become
 * pale-on-pale and just as unreadable.
 *
 * Why ban by accent family (50/100/200 stops) instead of enumerating: the
 * underlying invariant is "Tailwind defaults for these stops stay pale in
 * both themes" -- true for every accent family, not just the four we'd
 * enumerated. theme.css comments at lines 175-202 document that the 300+
 * stops flip dark in light mode by design, so 50/100/200 are the
 * always-pale band.
 *
 * Why scan every string literal, not just `className=` attributes: the
 * earlier version missed three patterns the codebase already uses:
 *   1. module-level class constants (`RestartServerButton.tsx:35-36`)
 *   2. `className={"…"}` with a JS expression
 *   3. ternaries like `className={c ? "…text-white" : "…"}` in JSX
 * (`NodePickerModal.tsx:235-239`). Scanning every `"…"`, `'…'`, and `` `…` ``
 * literal closes all three -- a string containing one of the banned tokens
 * is checked against the allowlist no matter how it ends up in a className.
 *
 * Why exclude `\n` from the single-quoted string match: a regex that pairs
 * `'…'` greedily across newlines swallows the next real `'`-literal after
 * any prose apostrophe -- e.g. `<p>don't</p>` followed by
 * `'bg-neutral-900 text-white'` consumes both into one bogus "literal".
 * Real `'`-quoted strings in JS don't span newlines unless explicitly
 * escaped (`'\n'`), so excluding newlines from the negated class closes
 * the gap without dropping real coverage.
 *
 * Runs under vitest like theme.test.ts (jsdom can't resolve CSS custom
 * properties through getComputedStyle, so a source scan is the closest
 * equivalent to a contrast assertion). Node types via triple-slash ref --
 * production tsconfig keeps a browser-shaped lib set.
 */

const here = dirname(fileURLToPath(import.meta.url));
const srcRoot = resolve(here, "..");

/**
 * Banned tokens. `text-white` is unconditionally banned. The accent stop
 * pattern covers the always-pale stops for every accent family Tailwind
 * ships -- theme.css only overrides the 300/400 (and sometimes 200)
 * stops; 50/100/200 fall through to Tailwind's pale defaults in both
 * themes. `{amber,red,emerald}` get explicit overrides at *100/*200 in
 * theme.css but stay pale; the others aren't declared at all.
 */
const BANNED_TOKEN_PATTERNS: string[] = [
  "text-white",
  "text-amber-(?:50|100|200)",
  "text-red-(?:50|100|200)",
  "text-emerald-(?:50|100|200)",
  "text-sky-(?:50|100|200)",
  "text-indigo-(?:50|100|200)",
  "text-blue-(?:50|100|200)",
  "text-purple-(?:50|100|200)",
  "text-rose-(?:50|100|200)",
  "text-fuchsia-(?:50|100|200)",
  "text-teal-(?:50|100|200)",
  "text-orange-(?:50|100|200)",
  "text-yellow-(?:50|100|200)",
  "text-pink-(?:50|100|200)",
  "text-cyan-(?:50|100|200)",
  "text-lime-(?:50|100|200)",
  "text-violet-(?:50|100|200)",
];

const BANNED_RE = new RegExp(
  String.raw`\b(?:(?:group-hover:|hover:|focus:|active:|disabled:|visited:)?(?:${BANNED_TOKEN_PATTERNS.join("|")}))\b`,
);

/**
 * Theme-invariant or dark-in-both-themes solid backgrounds. White text on
 * any of these is legible in both modes:
 *   - `bg-brand` (constant, theme.css line 24 / 144)
 *   - `bg-black`, `bg-white` (Tailwind built-ins)
 *   - `bg-{amber,red,emerald,sky}-[5-8]00` (theme.css comments at
 *     175-202 explicitly pin 500-800 dark in both themes)
 *   - `bg-indigo-[5-8]00` (theme.css line 168-171, dark both themes;
 *     indigo-900 flips PALE in light and is intentionally excluded)
 *   - `bg-{blue,purple,rose,fuchsia,teal}-[6-8]00` (undeclared, fall
 *     back to Tailwind dark defaults in both themes)
 *   - `bg-{amber,red,emerald,sky,indigo,blue,purple,rose,fuchsia,teal}-900`
 *     is NOT included: indigo-900 / blue-900 / purple-900 / etc. flip
 *     pale in light, while amber/red/emerald/sky-900 stay dark. The
 *     asymmetric family lets those four stay in the allowlist while
 *     excluding the rest. A genuine dark solid bg for a pale-text chip
 *     is the safer choice.
 *
 * The `(?:^|[\s"'`$:]|hover:|group-hover:|focus:|active:|disabled:|visited:)`
 * prefix accepts any Tailwind state-modifier position; the trailing
 * `(?![/\w-])` rejects opacity-modified bgs (`bg-indigo-600/50`,
 * `bg-red-900/60`) which would otherwise silently fall through to the
 * page bg and defeat the contrast guarantee.
 */
const ALLOWED_SOLID_BG =
  /(?:^|[\s"'`$:]|hover:|group-hover:|focus:|active:|disabled:|visited:)bg-(?:brand|black|white|amber-[5-8]00|red-[5-8]00|emerald-[5-8]00|sky-[5-8]00|indigo-[5-8]00|amber-900|red-900|emerald-900|sky-900|blue-[6-8]00|purple-[6-8]00|rose-[6-8]00|fuchsia-[6-8]00|teal-[6-8]00)(?![/\w-])/;

/** group-hover pairing: solid bg and banned text must flip together. */
const ALLOWED_GROUP_HOVER_BG =
  /group-hover:bg-(?:brand|black|white|amber-[5-8]00|red-[5-8]00|emerald-[5-8]00|sky-[5-8]00|indigo-[5-8]00|amber-900|red-900|emerald-900|sky-900|blue-[6-8]00|purple-[6-8]00|rose-[6-8]00|fuchsia-[6-8]00|teal-[6-8]00)(?![/\w-])/;

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
 * single-level templates this codebase uses. The single-quoted match
 * excludes newlines so a prose apostrophe (e.g. `don't`) doesn't pair
 * with the next real `'…'` literal and swallow it.
 *
 * Comments are stripped first so a comment mentioning `text-black` or
 * `text-white` doesn't false-positive the zero-tolerance assertions.
 */
function extractStringLiterals(src: string): string[] {
  const out: string[] = [];
  const stripped = src
    .replace(/\/\*[\s\S]*?\*\//g, " ")
    .replace(/(^|[^:\\])\/\/[^\n]*/g, "$1 ");
  const reDouble = /"([^"\\\n]*(?:\\.[^"\\\n]*)*)"/g;
  const reSingle = /'([^'\\\n]*(?:\\.[^'\\\n]*)*)'/g;
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
      /group-hover:(?:text-white|text-(?:amber|red|emerald|sky|indigo|blue|purple|rose|fuchsia|teal|orange|yellow|pink|cyan|lime|violet)-(?:50|100|200))\b/.test(s) &&
      ALLOWED_GROUP_HOVER_BG.test(s);
    if (hasSolidBg || hasGroupHoverPair) {
      allowed.push(`${rel}: ${s.trim().slice(0, 80)}`);
    } else {
      violations.push(`${rel}: ${s.trim()}`);
    }
  }
}

describe("hardcoded theme-unsafe text colors", () => {
  it("bans text-white / pale accent stops without an allowlisted solid background", () => {
    expect(
      violations,
      `theme-unsafe text-color tokens without an allowlisted solid bg (use text-neutral-100 for body highlights, text-{amber,red,emerald,...}-300/400 for chip text):\n${violations.join("\n")}`,
    ).toEqual([]);
  });

  it("keeps the documented bg-X text-white button pattern on the allowlist", () => {
    // Sanity: the scan must not ban every occurrence (empty allowlist
    // would pass the first assertion vacuously if extractStringLiterals
    // broke). Spot-check the documented button pattern stays allowed --
    // sourced from `hardcodedTextColors.fixture.tsx` (which only the
    // scanner reads, not the app), so the assertion doesn't drift when
    // a production file refactors its class string into a const.
    expect(allowed.length).toBeGreaterThan(0);
    expect(
      allowed.some(
        (a) =>
          a.startsWith("styles/hardcodedTextColors.fixture.tsx:") &&
          a.includes(KNOWN_SAFE),
      ),
    ).toBe(true);
  });

  it("never hardcodes text-black", () => {
    expect(textBlackCount).toBe(0);
  });
});
