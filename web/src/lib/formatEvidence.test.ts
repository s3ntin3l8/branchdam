import { describe, expect, it } from "vitest";
import {
  formatEvidence,
  formatSignalValue,
  humanizeKey,
  resolverLabel,
  signalLabel,
  type FormattedEvidence,
} from "./formatEvidence";

function structured(evidence: FormattedEvidence) {
  if (evidence.kind !== "structured") throw new Error(`expected structured, got ${evidence.kind}`);
  return evidence;
}

// rowsOf returns a {label: value} map of one group's rows.
function rowsOf(evidence: FormattedEvidence, groupKey: string): Record<string, string> {
  const group = structured(evidence).groups.find((g) => g.key === groupKey);
  if (!group) throw new Error(`no group ${groupKey}`);
  return Object.fromEntries(group.rows.map((r) => [r.label, r.value]));
}

function allValues(evidence: FormattedEvidence): string[] {
  const s = structured(evidence);
  return [...s.flat, ...s.groups.flatMap((g) => g.rows)].map((r) => r.value);
}

describe("formatEvidence", () => {
  it("returns empty for missing, blank, and empty-object evidence", () => {
    expect(formatEvidence(undefined)).toEqual({ kind: "empty" });
    expect(formatEvidence(null)).toEqual({ kind: "empty" });
    expect(formatEvidence("")).toEqual({ kind: "empty" });
    expect(formatEvidence("   ")).toEqual({ kind: "empty" });
    expect(formatEvidence("{}")).toEqual({ kind: "empty" });
  });

  it("keeps the raw string when the JSON is invalid", () => {
    expect(formatEvidence("{not json")).toEqual({ kind: "raw", raw: "{not json" });
  });

  it("keeps the raw string when the top level is not an object", () => {
    expect(formatEvidence("[1,2]")).toEqual({ kind: "raw", raw: "[1,2]" });
    expect(formatEvidence('"hello"')).toEqual({ kind: "raw", raw: '"hello"' });
    expect(formatEvidence("null")).toEqual({ kind: "raw", raw: "null" });
  });

  it("keeps flat top-level scalars as flat rows", () => {
    const ev = structured(formatEvidence('{"reason":"immich_hardlink_export"}'));
    expect(ev.groups).toEqual([]);
    expect(ev.flat).toEqual([{ key: "reason", label: "Reason", value: "immich_hardlink_export" }]);
  });

  it("groups xmp_original_document_id evidence", () => {
    const ev = formatEvidence(
      JSON.stringify({
        xmp_original_document_id: {
          child_original_document_id: "xmp.did:abc",
          parent_document_id: "xmp.did:abc",
        },
      }),
    );
    expect(structured(ev).groups[0].label).toBe("XMP document ID");
    expect(rowsOf(ev, "xmp_original_document_id")).toEqual({
      "Child original document ID": "xmp.did:abc",
      "Parent document ID": "xmp.did:abc",
    });
  });

  it("groups filename_stem evidence from the DJI branch", () => {
    const ev = formatEvidence(
      JSON.stringify({
        filename_stem: {
          filename_stem: "dji_0042",
          dji_telemetry: true,
          same_directory: true,
          same_capture_day: true,
          same_camera_model: true,
          gps_latitude: 47.376886123,
          gps_longitude: 8.541694,
        },
      }),
    );
    expect(structured(ev).groups[0].label).toBe("Filename stem");
    expect(rowsOf(ev, "filename_stem")).toEqual({
      Stem: "dji_0042",
      "DJI telemetry": "yes",
      "Same directory": "yes",
      "Same capture day": "yes",
      "Same camera model": "yes",
      "GPS latitude": "47.37689",
      "GPS longitude": "8.54169",
    });
  });

  it("groups filename_stem evidence from the generic branch, where same_camera_model is the model name", () => {
    const ev = formatEvidence(
      JSON.stringify({
        filename_stem: {
          filename_stem: "dsc01301",
          same_capture_day: true,
          same_camera_model: "ILCE-7M4",
          same_directory: true,
          index_suffix_gated: true,
          anchor_file_name: "DSC01301.ARW",
          proxy_ext_direction_forced: true,
        },
      }),
    );
    expect(rowsOf(ev, "filename_stem")).toEqual({
      Stem: "dsc01301",
      "Same capture day": "yes",
      "Same camera model": "yes (ILCE-7M4)",
      "Same directory": "yes",
      "Index suffix gated": "yes",
      "Anchor file name": "DSC01301.ARW",
      "Proxy direction forced": "yes",
    });
  });

  it("groups heuristic_spatial_temporal evidence and rounds numbers", () => {
    const ev = formatEvidence(
      JSON.stringify({
        heuristic_spatial_temporal: {
          camera_serial: "SN123",
          capture_delta_seconds: 1.23456,
          lens_model: "FE 24-70mm F2.8 GM II",
          hamming_distance: 4,
          phash_missing: false,
        },
      }),
    );
    expect(structured(ev).groups[0].label).toBe("Spatial-temporal heuristic");
    expect(rowsOf(ev, "heuristic_spatial_temporal")).toEqual({
      "Camera serial": "SN123",
      "Capture delta (exact)": "1.2s",
      "Lens model": "FE 24-70mm F2.8 GM II",
      "pHash distance (at match)": "4 bits",
      "pHash missing": "no",
    });
  });

  it("groups project_sidecar evidence", () => {
    const ev = formatEvidence(
      JSON.stringify({
        project_sidecar: {
          project_file_path: "/storage/proj/edit.drp",
          raw_reference: "A001.MOV",
          role: "sidecar",
          match_type: "same_stem_sibling",
        },
      }),
    );
    expect(structured(ev).groups[0].label).toBe("Project sidecar");
    expect(rowsOf(ev, "project_sidecar")).toEqual({
      "Project file": "/storage/proj/edit.drp",
      "Raw reference": "A001.MOV",
      Role: "sidecar",
      "Match type": "same_stem_sibling",
    });
  });

  it("shortens a signal named after its resolver only inside that group", () => {
    const ev = structured(formatEvidence(JSON.stringify({ filename_stem: "x", heuristic_spatial_temporal: { filename_stem: "y" } })));
    expect(ev.flat).toEqual([{ key: "filename_stem", label: "Filename stem", value: "x" }]);
    expect(ev.groups[0].rows).toEqual([{ key: "filename_stem", label: "Filename stem", value: "y" }]);
    expect(signalLabel("filename_stem")).toBe("Filename stem");
  });

  it("emits one group per merged resolver, in order", () => {
    const ev = structured(
      formatEvidence(
        JSON.stringify({
          filename_stem: { filename_stem: "a" },
          heuristic_spatial_temporal: { camera_serial: "x" },
        }),
      ),
    );
    expect(ev.groups.map((g) => g.label)).toEqual(["Filename stem", "Spatial-temporal heuristic"]);
  });

  it("mixes flat scalars and resolver groups", () => {
    const ev = structured(formatEvidence(JSON.stringify({ reason: "manual", project_sidecar: { role: "edit" } })));
    expect(ev.flat.map((r) => r.label)).toEqual(["Reason"]);
    expect(ev.groups.map((g) => g.label)).toEqual(["Project sidecar"]);
  });

  it("serialises deeper nesting and arrays as JSON, never [object Object]", () => {
    const ev = formatEvidence(
      JSON.stringify({
        agent_resolver: {
          nested: { a: 1, b: { c: [true] } },
          list: [1, "two", { three: 3 }],
        },
        top_list: [{ x: 1 }],
      }),
    );
    expect(rowsOf(ev, "agent_resolver")).toEqual({
      Nested: '{"a":1,"b":{"c":[true]}}',
      List: '[1,"two",{"three":3}]',
    });
    expect(structured(ev).flat).toEqual([{ key: "top_list", label: "Top list", value: '[{"x":1}]' }]);
    for (const v of allValues(ev)) expect(v).not.toContain("[object Object]");
  });

  it("humanizes unknown resolver and signal names", () => {
    const ev = formatEvidence(JSON.stringify({ some_agent_resolver: { timelineId: "t1", media_file_path: "/m.mov" } }));
    expect(structured(ev).groups[0].label).toBe("Some agent resolver");
    expect(rowsOf(ev, "some_agent_resolver")).toEqual({ "Timeline id": "t1", "Media file path": "/m.mov" });
  });

  it("keeps an empty resolver group", () => {
    const ev = structured(formatEvidence('{"filename_stem":{}}'));
    expect(ev.groups).toEqual([{ key: "filename_stem", label: "Filename stem", rows: [] }]);
  });

  it("does not resolve labels from Object.prototype", () => {
    expect(signalLabel("constructor")).toBe("Constructor");
    expect(resolverLabel("toString")).toBe("To string");
    const ev = structured(formatEvidence('{"__proto__":{"constructor":"x"}}'));
    expect(ev.groups[0].label).toBe("Proto");
    expect(ev.groups[0].rows).toEqual([{ key: "constructor", label: "Constructor", value: "x" }]);
  });
});

describe("formatSignalValue", () => {
  it("renders booleans as yes/no", () => {
    expect(formatSignalValue("anything", true)).toBe("yes");
    expect(formatSignalValue("anything", false)).toBe("no");
  });

  it("renders null and empty strings as a dash", () => {
    expect(formatSignalValue("lens_model", null)).toBe("—");
    expect(formatSignalValue("lens_model", "")).toBe("—");
  });

  it("rounds generic numbers to at most three decimals and keeps integers", () => {
    expect(formatSignalValue("score", 0.123456)).toBe("0.123");
    expect(formatSignalValue("score", 2.5)).toBe("2.5");
    expect(formatSignalValue("count", 42)).toBe("42");
  });

  it("formats capture deltas in seconds and gps to five decimals", () => {
    expect(formatSignalValue("capture_delta_seconds", 0)).toBe("0s");
    expect(formatSignalValue("capture_delta_seconds", 12.04)).toBe("12s");
    expect(formatSignalValue("gps_latitude", -33.8)).toBe("-33.80000");
    expect(formatSignalValue("gps_longitude", 151.2093456)).toBe("151.20935");
  });
});

describe("humanizeKey", () => {
  it("converts snake_case and camelCase to sentence case", () => {
    expect(humanizeKey("proxy_ext_direction_forced")).toBe("Proxy ext direction forced");
    expect(humanizeKey("mediaFilePath")).toBe("Media file path");
    expect(humanizeKey("x")).toBe("X");
    expect(humanizeKey("GPSLatitude")).toBe("GPS latitude");
    expect(humanizeKey("gpsLatitude")).toBe("Gps latitude");
    expect(humanizeKey("rawXMPPath")).toBe("Raw XMP path");
    expect(humanizeKey("ID")).toBe("ID");
    expect(humanizeKey("___")).toBe("___");
  });
});
