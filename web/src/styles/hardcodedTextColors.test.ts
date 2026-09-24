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
 * An unprefixed allowlisted bg also licenses a state-prefixed banned
 * token (`bg-brand hover:text-white`): the base bg is present underneath
 * whenever no state bg overrides it (round-7 S1). The override case is
 * checked separately -- if a bg exists at the token's state prefix, it
 * must itself be allowlisted at that prefix, otherwise it replaces the
 * base bg in that state (`bg-brand hover:bg-white hover:text-white` is
 * still banned). Round-8 extended the same override reasoning to
 * UNPREFIXED tokens: `bg-red-900 hover:bg-white text-white` is banned
 * because on hover the text sits on the unallowlisted hover bg.
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
// any of these sits on a bg that stays dark in both modes (so a `text-white`
// token has the dark-bg precondition satisfied in both modes):
//   - `bg-brand` / `bg-brand-dark` (constant in both themes, registered in
//     the @theme block at theme.css so the utilities actually emit -- before
//     that registration `bg-brand` produced zero CSS rules and 13 CTAs were
//     white-on-transparent; Hermes round-8 critical)
//   - `bg-black` (Tailwind built-in, `#000` in both themes)
//   - the accent stops in BG_STOPS_BY_FAMILY below, derived from
//     ACCENT_FAMILIES so a newly added family cannot ship without bg
//     coverage (a module-level throw enforces it):
//     amber/red/emerald/sky: `[5-8]00|900` (theme.css comments at
//       175-202 explicitly pin 500-800 dark in both themes; the 900s
//       also stay dark in both)
//     indigo: `[5-8]00` only -- indigo-900 flips PALE in light
//       (theme.css:172 `#e0e7ff`) and indigo-950 likewise
//       (theme.css:173 `#eef2ff`); both intentionally excluded
//     blue: `[6-8]00` only -- blue-900 (theme.css:228 `#dbeafe`) and
//       blue-950 (theme.css:229 `#eff6ff`) flip PALE in light; excluded
//     purple/rose/fuchsia/teal: `[6-8]00|900|950` (undeclared in
//       theme.css at all, so their 900/950 stops stay Tailwind-dark in
//       both themes; Hermes round-6 W2 -- the earlier comment lumped
//       them in with indigo/blue and the allowlist wrongly rejected
//       theme-safe pairs like `bg-purple-900 text-purple-200`)
//     orange/yellow/pink/cyan/lime/violet: `[5-8]00|900|950`
//       (undeclared beyond graph-edge 400s, so these stops stay
//       Tailwind-dark in both themes; Hermes round-7 W2 -- the old
//       hand-written pattern covered only 10 of the 16 ACCENT_FAMILIES
//       and hard-failed theme-safe strings like
//       `bg-orange-600 text-white`)
//
// Note: the scanner proves *theme invariance* of the bg, **not** contrast.
// Live sites like `RestartServerButton.tsx:36` (`bg-amber-600 text-white`
// ≈ 3.2:1) and `UsersPage.tsx:610` (`hover:bg-amber-500 text-white` ≈ 2.8:1)
// are below WCAG AA 4.5 but pass the scanner because the bg stays dark in
// both themes -- a separate luminance check is needed for a hard contrast
// guarantee. Tracked as #493. The same applies to the newly allowlisted
// undeclared 500 stops (e.g. `bg-lime-500 text-white` ≈ 2.0:1).
//   - `bg-white` is NOT included: it's `#fff` in both themes (undeclared
//     -> Tailwind default), so `bg-white text-X-pale` is pale-on-pale in
//     both modes. Hermes round-4 R4.1.
const BG_STOPS_BY_FAMILY: Record<string, string> = {
  amber: "[5-8]00|900",
  red: "[5-8]00|900",
  emerald: "[5-8]00|900",
  sky: "[5-8]00|900",
  indigo: "[5-8]00",
  blue: "[6-8]00",
  purple: "[6-8]00|900|950",
  rose: "[6-8]00|900|950",
  fuchsia: "[6-8]00|900|950",
  teal: "[6-8]00|900|950",
  orange: "[5-8]00|900|950",
  yellow: "[5-8]00|900|950",
  pink: "[5-8]00|900|950",
  cyan: "[5-8]00|900|950",
  lime: "[5-8]00|900|950",
  violet: "[5-8]00|900|950",
};

// Fail fast: every ACCENT_FAMILIES entry must have bg coverage, so a new
// family cannot be added to the ban list without deciding which of its
// solid stops are theme-invariant (Hermes round-7 W2).
for (const family of ACCENT_FAMILIES) {
  if (!(family in BG_STOPS_BY_FAMILY)) {
    throw new Error(`BG_STOPS_BY_FAMILY is missing accent family "${family}"`);
  }
}

const BG_NAME_PATTERN = `(?:brand(?:-dark)?|black|${ACCENT_FAMILIES.map(
  (family) => `${family}-(?:${BG_STOPS_BY_FAMILY[family]})`,
).join("|")})`;

// Effectively-opaque opacity modifiers on an allowlisted bg are allowed by
// the scanner: at >=80% the composite color stays dark enough that the
// theme-invariance heuristic still holds. This is NOT a contrast guarantee
// -- for a mid-luminance bg the page bg bleeds through even at 80%
// (`bg-emerald-600/80` gives white text ~2.9:1 over a light page, vs 3.77
// solid). Hermes round-9 S3. The translucent-chip bug class this scanner
// exists to catch (`bg-amber-950/70`) stays rejected.
const BG_OPACITY = "(?:\\/(?:[89]\\d|100))?";

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
    new RegExp(`${BG_PREFIX_ANCHOR}${prefix}bg-${BG_NAME_PATTERN}${BG_OPACITY}(?![/\\w-])`),
  ]),
);

function bgMatches(s: string, prefix: string): boolean {
  const re = BG_REGEX_BY_PREFIX[prefix] ?? BG_REGEX_BY_PREFIX[""];
  return re.test(s);
}

// True when a bg token exists at this state prefix at all (allowlisted or
// not). An unallowlisted state bg overrides the base bg in that state, so
// it blocks the unprefixed-bg fallback in isStringAllowed (round-7 S1).
const ANY_BG_REGEX_BY_PREFIX: Record<string, RegExp> = Object.fromEntries(
  STATE_PREFIXES.map((prefix) => [prefix, new RegExp(`${BG_PREFIX_ANCHOR}${prefix}bg-`)]),
);

function hasBgAtPrefix(s: string, prefix: string): boolean {
  return ANY_BG_REGEX_BY_PREFIX[prefix]?.test(s) ?? false;
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
  return banned.every((b) => {
    if (b.prefix === "") {
      // Skip the state sweep for template bodies: a `${...}` interpolation
      // may carry a sibling segment's state bg that never coexists with the
      // token's element (live: the IngestJobsPage.tsx kind-filter ternary,
      // where `hover:bg-neutral-750` sits in the INCREMENTAL-fallback
      // branch, never behind the `bg-amber-900 text-amber-200` chip). The
      // per-branch literals are extracted by reDouble/reSingle and checked
      // individually, so nothing escapes. The flat-string S1 leniency
      // (any bg anywhere licenses any token) stays tracked as #493.
      if (!s.includes("${")) {
        for (const prefix of STATE_PREFIXES) {
          if (hasBgAtPrefix(s, prefix) && !bgMatches(s, prefix)) return false;
        }
      }
      return bgMatches(s, "");
    }
    // Same-prefix allowlisted bg always wins (the classic
    // `hover:bg-amber-600 hover:text-white` pairing).
    if (bgMatches(s, b.prefix)) return true;
    // Otherwise fall back to an unprefixed allowlisted base bg -- but only
    // when no bg exists at this token's state prefix, because such a bg
    // would replace the base bg in that state (round-7 S1: fixes
    // `bg-brand hover:text-white` without also licensing
    // `bg-brand hover:bg-white hover:text-white`).
    if (hasBgAtPrefix(s, b.prefix)) return false;
    return bgMatches(s, "");
  });
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

  it("R6: allows undeclared {purple,rose,fuchsia,teal}-900/950 (Tailwind-dark both themes)", () => {
    // Hermes round-6 W2: these families have no theme.css declaration at
    // all, so their 900/950 stops fall back to Tailwind dark defaults in
    // both themes. The old pattern only had [6-8]00 and wrongly rejected
    // theme-safe pairs.
    expect(isStringAllowed("bg-purple-900 text-purple-200")).toBe(true);
    expect(isStringAllowed("bg-purple-950 text-white")).toBe(true);
    expect(isStringAllowed("bg-rose-950 text-white")).toBe(true);
    expect(isStringAllowed("bg-fuchsia-900 text-fuchsia-200")).toBe(true);
    expect(isStringAllowed("bg-teal-950 text-white")).toBe(true);
  });

  it("R6: still bans the two -900 stops that actually flip (indigo, blue)", () => {
    // indigo-900 (#312e81 -> #e0e7ff, theme.css:47/:172) and blue-900
    // (#1e3a8a -> #dbeafe, theme.css:93/:228) are declared in both blocks
    // and flip pale in light -- white text on them is invisible there.
    expect(isStringAllowed("bg-indigo-900 text-white")).toBe(false);
    expect(isStringAllowed("bg-blue-900 text-white")).toBe(false);
    expect(isStringAllowed("bg-indigo-950 text-white")).toBe(false);
    expect(isStringAllowed("bg-blue-950 text-white")).toBe(false);
  });
});

describe("round-7 scanner probes (Hermes review)", () => {
  it("R7 W2: every ACCENT_FAMILIES entry has BG_STOPS coverage (bg-*-600 text-white)", () => {
    // The fail-fast throw above catches a missing map entry at module load;
    // this probe proves each entry actually licenses a solid text-white pair.
    for (const family of ACCENT_FAMILIES) {
      expect(BG_STOPS_BY_FAMILY[family], `missing stops for ${family}`).toBeTruthy();
      expect(isStringAllowed(`bg-${family}-600 text-white`)).toBe(true);
    }
  });

  it("R7 W2: allows the six previously-missing families' 500/900/950 stops", () => {
    // orange/yellow/pink/cyan/lime/violet are undeclared beyond graph-edge
    // 400s, so these stops stay Tailwind-dark in both themes.
    expect(isStringAllowed("bg-orange-600 text-white")).toBe(true);
    expect(isStringAllowed("bg-lime-500 text-white")).toBe(true);
    expect(isStringAllowed("bg-pink-600 text-white")).toBe(true);
    expect(isStringAllowed("bg-cyan-900 text-cyan-200")).toBe(true);
    expect(isStringAllowed("bg-yellow-950 text-yellow-200")).toBe(true);
    expect(isStringAllowed("bg-violet-900 text-white")).toBe(true);
    // The two flipping families stay banned (regression guard for W2).
    expect(isStringAllowed("bg-indigo-900 text-white")).toBe(false);
    expect(isStringAllowed("bg-blue-950 text-white")).toBe(false);
  });

  it("R7 S1: unprefixed allowlisted bg licenses a state-prefixed banned token", () => {
    expect(isStringAllowed("bg-brand hover:text-white")).toBe(true);
    expect(isStringAllowed("bg-amber-600 focus:text-white")).toBe(true);
    // Regression: unprefixed banned token still needs an unprefixed
    // allowlisted bg (R4.2).
    expect(isStringAllowed("bg-neutral-900 hover:bg-amber-600 text-white")).toBe(false);
    // An unallowlisted state bg overrides the base bg in that state, so it
    // must not be rescued by the unprefixed fallback.
    expect(isStringAllowed("bg-brand hover:bg-white hover:text-white")).toBe(false);
    // Same-prefix allowlisted pairing still wins over the override check.
    expect(isStringAllowed("hover:bg-amber-600 hover:text-white")).toBe(true);
  });
});

describe("round-8 scanner probes (Hermes review)", () => {
  it("R8: bg-brand / bg-brand-dark are registered and license text-white", () => {
    // The round-8 critical: `bg-brand` emitted NO CSS before the @theme
    // registration in theme.css (verified: zero rules in a built bundle).
    // Live strings (login/MFA/password-reset CTAs, round-9 S1):
    expect(isStringAllowed("bg-brand-dark text-white hover:bg-indigo-700")).toBe(true);
    // ...and the opacity gate must still reject translucent chips:
    expect(isStringAllowed("bg-brand/70 text-white")).toBe(false);
    expect(isStringAllowed("bg-amber-950/70 text-amber-200")).toBe(false);
  });

  it("R8: unprefixed banned token fails when a state bg would replace the base bg", () => {
    // No live instance, but the exact shape Hermes probed: on hover the
    // white text sits on the white hover bg.
    expect(isStringAllowed("bg-red-900 hover:bg-white text-white")).toBe(false);
    // Regression: state bg that IS allowlisted keeps the live buttons green
    // (UsersPage.tsx:610 shape).
    expect(isStringAllowed("bg-brand text-white hover:bg-amber-500")).toBe(true);
    // A state bg on a sibling in the same literal is a documented
    // heuristic limit (string-level scan can't prove element identity).
  });

  it("R8: effectively-opaque (>=80%) allowlisted bgs license text-white", () => {
    // Folds the #493 S2 follow-up into the allowlist: the bg stays dark in
    // both modes at >=80% opacity, so white text keeps its dark backdrop.
    expect(isStringAllowed("bg-emerald-600/80 text-white")).toBe(true);
    expect(isStringAllowed("bg-brand/90 text-white")).toBe(true);
    // Below the threshold the page bg bleeds through enough to matter.
    expect(isStringAllowed("bg-emerald-600/60 text-white")).toBe(false);
  });

  it("R8: IngestJobsPage kind-filter hover shape passes the scanner", () => {
    // Note: this probe only proves the string carries no banned tokens
    // (neutral-750 is not a banned text stop). The "utility actually emits
    // CSS" half of the round-8 fix is guarded by theme.test.ts's @theme
    // registration block, not here.
    expect(isStringAllowed("bg-neutral-800 text-neutral-400 hover:bg-neutral-750 hover:text-neutral-200")).toBe(true);
  });
});
