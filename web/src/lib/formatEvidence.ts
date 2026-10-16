// Pure formatting for an edge's evidence_json, as shown in the audit queue.
//
// The graph engine's mergeCandidates (internal/graph/engine.go) wraps each
// contributing resolver's evidence under the resolver's name, so a typical
// payload is nested one level deep:
//
//   {"filename_stem": {"filename_stem": "dsc01301", "same_capture_day": true}}
//
// Other writers store flat evidence (e.g. {"reason": "immich_hardlink_export"}),
// and agent-written evidence is opaque. Every value is rendered to a string
// here, so nothing can reach the page as "[object Object]".

export interface EvidenceRow {
  key: string;
  label: string;
  value: string;
}

export interface EvidenceGroup {
  key: string;
  label: string;
  rows: EvidenceRow[];
}

export type FormattedEvidence =
  | { kind: "empty" }
  // Unparseable (or non-object) JSON: show the stored string verbatim.
  | { kind: "raw"; raw: string }
  // Top-level scalars stay flat; top-level objects become one group each.
  | { kind: "structured"; flat: EvidenceRow[]; groups: EvidenceGroup[] };

export const RESOLVER_LABELS: Readonly<Record<string, string>> = {
  xmp_original_document_id: "XMP document ID",
  filename_stem: "Filename stem",
  heuristic_spatial_temporal: "Spatial-temporal heuristic",
  project_sidecar: "Project sidecar",
};

export const SIGNAL_LABELS: Readonly<Record<string, string>> = {
  // xmp_original_document_id
  child_original_document_id: "Child original document ID",
  parent_document_id: "Parent document ID",
  // filename_stem
  filename_stem: "Filename stem",
  dji_telemetry: "DJI telemetry",
  same_directory: "Same directory",
  same_capture_day: "Same capture day",
  same_camera_model: "Same camera model",
  gps_latitude: "GPS latitude",
  gps_longitude: "GPS longitude",
  index_suffix_gated: "Index suffix gated",
  anchor_file_name: "Anchor file name",
  proxy_ext_direction_forced: "Proxy direction forced",
  // heuristic_spatial_temporal
  camera_serial: "Camera serial",
  // The resolver's exact float delta; the page's "Timestamp Delta" is a
  // separate whole-second value derived from captured_at_unix.
  capture_delta_seconds: "Capture delta (exact)",
  lens_model: "Lens model",
  hamming_distance: "pHash distance",
  phash_missing: "pHash missing",
  // project_sidecar
  project_file_path: "Project file",
  raw_reference: "Raw reference",
  role: "Role",
  match_type: "Match type",
  // flat evidence
  reason: "Reason",
};

const EMPTY_VALUE = "—";

// lookup reads only own properties, so keys like "constructor" or
// "__proto__" in untrusted evidence never resolve to Object.prototype members.
function lookup(map: Readonly<Record<string, string>>, key: string): string | undefined {
  return Object.hasOwn(map, key) ? map[key] : undefined;
}

// humanizeKey turns snake_case or camelCase into a sentence-case label,
// keeping acronyms intact: "proxy_ext_direction_forced" -> "Proxy ext
// direction forced", "GPSLatitude" -> "GPS latitude".
export function humanizeKey(key: string): string {
  const words = key
    .replace(/([a-z0-9])([A-Z])/g, "$1 $2")
    .replace(/([A-Z]+)([A-Z][a-z])/g, "$1 $2")
    .split(/[_\s-]+/)
    .filter(Boolean)
    .map((w) => (w.length > 1 && w === w.toUpperCase() && /[A-Z]/.test(w) ? w : w.toLowerCase()));
  if (words.length === 0) return key;
  const sentence = words.join(" ");
  return sentence.charAt(0).toUpperCase() + sentence.slice(1);
}

export function resolverLabel(key: string): string {
  return lookup(RESOLVER_LABELS, key) ?? humanizeKey(key);
}

export function signalLabel(key: string): string {
  return lookup(SIGNAL_LABELS, key) ?? humanizeKey(key);
}

// trimNumber rounds to `digits` decimal places and drops trailing zeros.
function trimNumber(n: number, digits: number): string {
  return String(Number(n.toFixed(digits)));
}

function formatNumber(key: string, n: number): string {
  if (!Number.isFinite(n)) return String(n);
  switch (key) {
    case "gps_latitude":
    case "gps_longitude":
      return n.toFixed(5);
    case "capture_delta_seconds":
      return `${trimNumber(n, 1)}s`;
    case "hamming_distance":
      return `${n} bits`;
    default:
      return Number.isInteger(n) ? String(n) : trimNumber(n, 3);
  }
}

export function formatSignalValue(key: string, value: unknown): string {
  if (value === null || value === undefined) return EMPTY_VALUE;
  if (typeof value === "boolean") return value ? "yes" : "no";
  if (typeof value === "number") return formatNumber(key, value);
  if (typeof value === "string") {
    if (value === "") return EMPTY_VALUE;
    // The DJI branch records same_camera_model as `true`; the generic
    // filename-stem branch records the matching model name instead.
    if (key === "same_camera_model") return `yes (${value})`;
    return value;
  }
  // Arrays and deeper nesting: compact JSON, never String(object).
  return JSON.stringify(value) ?? String(value);
}

function isPlainObject(v: unknown): v is Record<string, unknown> {
  return typeof v === "object" && v !== null && !Array.isArray(v);
}

// Short labels for a signal that shares its resolver's name, used only
// inside that resolver's group so the row doesn't repeat the heading
// ("Filename stem" / "Stem: dsc01301"). A flat row keeps the full label.
const SELF_SIGNAL_LABELS: Readonly<Record<string, string>> = {
  filename_stem: "Stem",
};

function toRow(key: string, value: unknown, groupKey?: string): EvidenceRow {
  const label =
    key === groupKey ? (lookup(SELF_SIGNAL_LABELS, key) ?? signalLabel(key)) : signalLabel(key);
  return { key, label, value: formatSignalValue(key, value) };
}

export function formatEvidence(evidenceJson?: string | null): FormattedEvidence {
  if (!evidenceJson || evidenceJson.trim() === "") return { kind: "empty" };

  let parsed: unknown;
  try {
    parsed = JSON.parse(evidenceJson);
  } catch {
    return { kind: "raw", raw: evidenceJson };
  }
  if (!isPlainObject(parsed)) return { kind: "raw", raw: evidenceJson };

  const flat: EvidenceRow[] = [];
  const groups: EvidenceGroup[] = [];
  for (const [key, value] of Object.entries(parsed)) {
    if (isPlainObject(value)) {
      groups.push({
        key,
        label: resolverLabel(key),
        rows: Object.entries(value).map(([k, v]) => toRow(k, v, key)),
      });
    } else {
      flat.push(toRow(key, value));
    }
  }
  if (flat.length === 0 && groups.length === 0) return { kind: "empty" };
  return { kind: "structured", flat, groups };
}
