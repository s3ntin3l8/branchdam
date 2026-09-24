/**
 * Stable sample for the allowlist spot-check in
 * `hardcodedTextColors.test.ts`. The scanner extracts every string literal
 * from `web/src` and matches the doc-documented button pattern
 * (`bg-brand text-white`) against the allowlist; the spot-check asserts
 * this fixture is recognised as allowed, independent of any production
 * file. If LoginPage, UserMenu, or any other component ever moves its
 * `bg-brand text-white` chip into a different form, the spot-check no
 * longer drifts with it -- it only fails when the fixture's literal
 * itself regresses.
 *
 * Kept minimal: one exported constant, no JSX, no React import, so the
 * scanner sees exactly one literal and the fixture has no side effects
 * if it ever gets accidentally imported elsewhere.
 */
export const KNOWN_SAFE = "bg-brand text-white";
