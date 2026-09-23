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
 * Why `(?<![A-Za-z0-9_'])` lookbehind on `reSingle`: closing the same-line
 * probe `<p>don't</p>; const X = 'real'` -- without the lookbehind, the
 * apostrophe in `don't` opens a single-quoted string and consumes all the
 * text up to the next `'`, swallowing the real `'…'` literal entirely. The
 * lookbehind requires the opening `'` to follow a non-identifier
 * character (or string start), so prose apostrophes are skipped while
 * real JS string literals still match. Real `'`-quoted strings in JS
 * don't span newlines unless explicitly escaped (`'\n'`), so the existing
 * `\n` exclusion in the negated class is kept as a defensive belt.
 *
 * Why prefix-matching between bg and banned token: a state-prefixed bg
 * (`hover:bg-amber-600`) doesn't satisfy an un-prefixed banned token
 * (`text-white`) because at-rest the text still sits on the un-prefixed
 * parent bg. `bg-neutral-900 hover:bg-amber-600 text-white` is banned
 * (unprefixed text-white has no unprefixed allowlisted bg), while
 * `hover:bg-amber-600 hover:text-white` is allowed (both hover-prefixed).
 *
 * Why a `FLIPPING_TEXT_TOKENS` exception set: `--color-indigo-200` is
 * declared in BOTH theme blocks and FLIPS (`#c7d2fe` dark, `#4338ca`
 * light) -- so `text-indigo-200` is theme-aware and stays legible on a
 * dark chip in both modes. The general "ban the 50/100/200 stops"
 * heuristic over-bans it; explicit exception is used instead of parsing
 * theme.css for luminance. If a new flip is added to theme.css, add an
 * explicit exception here AND a fixture probe so the scanner and the
 * theme stay in sync.
 *
 * Runs under vitest like theme.test.ts (jsdom can't resolve CSS custom
 * properties through getComputedStyle, so a source scan is the closest
 * equivalent to a contrast assertion). Node types via triple-slash ref --
 * production tsconfig keeps a browser-shaped lib set.
 */

const here = dirname(fileURLToPath(import.meta.url));
const srcRoot = resolve(here, "..");

const ACCENT_FAMILIES = [
  "amber",
  "red",
  "emerald",
  "sky",
  "indigo",
  "blue",
  "purple",
  "rose",
  "fuchsia",
  "teal",
  "orange",
  "yellow",
  "pink",
  "cyan",
  "lime",
  "violet",
];

// Theme-aware flips: declared in both theme blocks and pale-in-one /
// dark-in-other, so they stay legible on a theme-invariant dark bg in both
// modes. Probe-confirmed for text-indigo-200 against bg-indigo-900. Add a
// new entry here AND a matching `it()` probe in "round-4 scanner probes"
// when theme.css declares another flipper.
const FLIPPING_TEXT_TOKENS = new Set<string>(["text-indigo-200"]);

const BANNED_TOKEN_PATTERNS: string[] = (() => {
  const out: string[] = ["text-white"];
  for (const family of ACCENT_FAMILIES) {
    const stops: string[] = [];
    for (const stop of ["50", "100", "200"]) {
      if (!FLIPPING_TEXT_TOKENS.has(`text-${family}-${stop}`)) {
        stops.push(stop);
      }
    }
    if (stops.length > 0) {
      out.push(`text-${family}-(?:${stops.join("|")})`);
    }
  }
  return out;
})();

// Theme-invariant or dark-in-both-themes solid backgrounds. White text on
// any of these is legible in both modes:
//   - `bg-brand` (constant, theme.css line 24 / 144)
//   - `bg-black` (Tailwind built-in, `#000` in both themes)
//   - `bg-{amber,red,emerald,sky}-[5-8]00` (theme.css comments at
//     175-202 explicitly pin 500-800 dark in both themes)
//   - `bg-indigo-[5-8]00` (theme.css line 168-171, dark both themes;
//     indigo-900 flips PALE in light and is intentionally excluded)
//   - `bg-{blue,purple,rose,fuchsia,teal}-[6-8]00` (undeclared, fall
//     back to Tailwind dark defaults in both themes)
//   - `bg-{amber,red,emerald,sky}-900` is also allowlisted: those four
//     stay dark in both themes; `bg-indigo-900` (and friends) flip pale
//     in light and is intentionally excluded.
//   - `bg-white` is NOT included: it's `#fff` in both themes (undeclared
//     -> Tailwind default), so `bg-white text-X-pale` is pale-on-pale in
//     both modes. Hermes round-4 R4.1.
const BG_NAME_PATTERN =
  "(?:brand|black|amber-[5-8]00|red-[5-8]00|emerald-[5-8]00|sky-[5-8]00|indigo-[5-8]00|amber-900|red-900|emerald-900|sky-900|blue-[6-8]00|purple-[6-8]00|rose-[6-8]00|fuchsia-[6-8]00|teal-[6-8]00)";

// Each banned token's optional state prefix is captured in group 1.
const BANNED_TOKEN_RE = new RegExp(
  String.raw`\b((?:hover:|group-hover:|focus:|active:|disabled:|visited:)?)(?:${BANNED_TOKEN_PATTERNS.join("|")})\b`,
  "g",
);

// Anchor for an unprefixed bg: start of string, whitespace, quote, or `$`
// (template-literal end). `:` is intentionally excluded because `:` is part
// of state prefixes (`hover:`, `focus:`, etc.) -- the prefix-matching logic
// requires the bg prefix to match the token prefix, so an unprefixed bg
// must NOT match the prefix portion of a state-prefixed bg.
const BG_PREFIX_ANCHOR = "(?:^|[\\s\"'`$])";
const STATE_PREFIXES = ["", "hover:", "group-hover:", "focus:", "active:", "disabled:", "visited:"];

const BG_REGEX_BY_PREFIX: Record<string, RegExp> = Object.fromEntries(
  STATE_PREFIXES.map((prefix) => [
    prefix,
    new RegExp(`${BG_PREFIX_ANCHOR}${prefix}bg-${BG_NAME_PATTERN}(?![/\\w-])`),
  ]),
);

function bgMatches(s: string, prefix: string): boolean {
  const re = BG_REGEX_BY_PREFIX[prefix] ?? BG_REGEX_BY_PREFIX[""];
  return re.test(s);
}

function findBannedTokens(s: string): Array<{ prefix: string; token: string }> {
  const out: Array<{ prefix: string; token: string }> = [];
  for (const m of s.matchAll(BANNED_TOKEN_RE)) {
    out.push({ prefix: m[1] ?? "", token: m[0] });
  }
  return out;
}

function isStringAllowed(s: string): boolean {
  const banned = findBannedTokens(s);
  if (banned.length === 0) return true;
  return banned.every((b) => bgMatches(s, b.prefix));
}

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
 * has a negative lookbehind for the opening quote so a prose apostrophe
 * (e.g. `don't`) doesn't pair with the next real `'…'` literal and
 * swallow it; the negated class also excludes `\n` as a defensive
 * belt.
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
  // Opening `'` must follow a non-identifier char (or string start), so a
  // prose apostrophe (`don't`) doesn't pair with the next real `'…'`
  // literal and swallow it. The lookbehind is anchored to the explicit
  // opening `'` (not the regex start position) so it can't be bypassed by
  // greedy content matching across multiple `'`s on the same line.
  const reSingle = /(?<![A-Za-z0-9_'])'((?:[^'\\\n]|\\.)*)'/g;
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
    if (findBannedTokens(s).length === 0) continue;
    if (isStringAllowed(s)) {
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

describe("round-4 scanner probes (Hermes review)", () => {
  it("R4.1: bans bg-white text-X-pale (bg-white is undeclared -> Tailwind pale both themes)", () => {
    expect(isStringAllowed("bg-white text-white")).toBe(false);
    expect(isStringAllowed("bg-white text-amber-200")).toBe(false);
  });

  it("R4.2: requires matching state prefix on bg and token", () => {
    // Unprefixed text-white has no unprefixed allowlisted bg -> banned.
    expect(isStringAllowed("bg-neutral-900 hover:bg-amber-600 text-white")).toBe(false);
    // Both hover-prefixed -> allowed.
    expect(isStringAllowed("hover:bg-amber-600 hover:text-white")).toBe(true);
    // group-hover pairing survives the refactor (live at NodePickerModal.tsx:238).
    expect(
      isStringAllowed(
        "border-indigo-600/50 bg-indigo-600/20 text-indigo-300 group-hover:bg-indigo-600 group-hover:text-white",
      ),
    ).toBe(true);
  });

  it("R4.3: skips same-line apostrophe before a single-quoted literal", () => {
    // The `'` in `don't` is preceded by `n` (lookbehind fires at the opening
    // quote), so it doesn't open a string. The real literal
    // `bg-neutral-900 text-white` is the only one captured (preceded by ` `)
    // and is flagged because unprefixed text-white has no unprefixed
    // allowlisted bg in the string.
    const input = "<p>don't</p>; const X = 'bg-neutral-900 text-white';";
    const literals = extractStringLiterals(input);
    expect(literals).toEqual(["bg-neutral-900 text-white"]);
    expect(isStringAllowed("bg-neutral-900 text-white")).toBe(false);
  });

  it("R4.4: allows text-indigo-200 as a theme-aware flip (theme.css:40 / :165)", () => {
    expect(isStringAllowed("bg-indigo-900 text-indigo-200")).toBe(true);
    expect(isStringAllowed("bg-indigo-950/60 text-indigo-200")).toBe(true);
  });

  it("still flags text-amber-200 / text-red-200 (family rule, no flip)", () => {
    // amber-200 and red-200 stay pale in both themes per theme.css:181 / :194,
    // so the family rule still bans them without an allowlisted bg.
    expect(isStringAllowed("text-amber-200")).toBe(false);
    expect(isStringAllowed("text-red-200")).toBe(false);
    expect(isStringAllowed("bg-amber-900 text-amber-200")).toBe(true);
    expect(isStringAllowed("bg-red-900 text-red-200")).toBe(true);
  });
});
