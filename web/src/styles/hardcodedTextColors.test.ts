/// <reference types="node" />
import { describe, expect, it } from "vitest";
import { readdirSync, readFileSync } from "node:fs";
import { fileURLToPath } from "node:url";
import { dirname, join, relative, resolve } from "node:path";

/*
 * Static analysis of JSX className strings: hard bans `text-white` (and
 * `hover:text-white` / `group-hover:text-white`) unless the same className
 * also carries a theme-invariant solid background from the allowlist.
 *
 * Why this exists: light theme inverts the neutral ladder, so
 * `bg-neutral-900` becomes near-white while Tailwind's `text-white` stays
 * `#fff`. White-on-light dialog body text was invisible until fixed; this
 * test keeps the class of bug from coming back. The allowlist matches the
 * button pattern documented in theme.css (`bg-amber-600 text-white`, etc.)
 * where the background token is deliberately pinned per theme.
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
 * Opacity modifiers (`bg-brand/90`) are deliberately not allowed as the
 * *sole* background -- white text needs a solid fill.
 */
const ALLOWED_SOLID_BG =
  /(?:^|[\s"'`$])bg-(?:brand|indigo-600|amber-600|red-[5-9]00|emerald-[67]00|sky-[567]00)(?![/\w-])/;

/** group-hover pairing: solid bg and white text must flip together. */
const ALLOWED_GROUP_HOVER_BG =
  /group-hover:bg-(?:brand|indigo-600|amber-600|red-[5-9]00|emerald-[67]00|sky-[567]00)(?![/\w-])/;

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
 * Pull every `className="..."` and `className={`...`}` value from source.
 * Template literals may span lines and embed `${...}` conditionals; the
 * non-greedy match stops at the first closing backtick, which is correct
 * for the single-level templates this codebase uses.
 */
function extractClassNames(src: string): string[] {
  const out: string[] = [];
  const doubleQuote = /className="([^"]*)"/g;
  const template = /className=\{`([\s\S]*?)`\}/g;
  let m: RegExpExecArray | null;
  while ((m = doubleQuote.exec(src)) !== null) out.push(m[1]);
  while ((m = template.exec(src)) !== null) out.push(m[1]);
  return out;
}

const violations: string[] = [];
const allowed: string[] = [];
let textBlackCount = 0;

for (const file of listSourceFiles(srcRoot)) {
  const rel = relative(srcRoot, file).split("\\").join("/");
  const src = readFileSync(file, "utf8");
  textBlackCount += (src.match(/\btext-black\b/g) ?? []).length;

  for (const cls of extractClassNames(src)) {
    if (!/\b(?:group-hover:|hover:)?text-white\b/.test(cls)) continue;
    const hasSolidBg = ALLOWED_SOLID_BG.test(cls);
    const hasGroupHoverPair =
      /group-hover:text-white/.test(cls) && ALLOWED_GROUP_HOVER_BG.test(cls);
    if (hasSolidBg || hasGroupHoverPair) {
      allowed.push(`${rel}: ${cls.trim().slice(0, 80)}`);
    } else {
      violations.push(`${rel}: ${cls.trim()}`);
    }
  }
}

describe("hardcoded text-white ban (theme contrast)", () => {
  it("only uses text-white with a theme-invariant solid background", () => {
    expect(
      violations,
      `text-white without an allowlisted solid bg (use text-neutral-100 / text-amber-400 for body highlights):\n${violations.join("\n")}`,
    ).toEqual([]);
  });

  it("keeps the documented bg-X text-white button pattern on the allowlist", () => {
    // Sanity: the scan must not ban every occurrence (empty allowlist would
    // pass the first assertion vacuously if extractClassNames broke).
    expect(allowed.length).toBeGreaterThan(0);
    // Spot-check a known button pattern stays allowed. If this regresses
    // the regex stopped recognising solid-bg buttons, which would silently
    // fail-open in the violation check.
    expect(allowed.some((a) => a.startsWith("pages/LoginPage.tsx:") && a.includes("bg-brand") && a.includes("text-white"))).toBe(true);
  });

  it("never hardcodes text-black", () => {
    expect(textBlackCount).toBe(0);
  });
});
