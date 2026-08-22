# `.dam.json` Manifest Specification

The `.dam.json` manifest is `branchDAM`'s native project manifest format. It allows projects to explicitly document their media references, roles, and output exports.

**The filename must end in `.dam.json`, not just `.json`.** `internal/projectfile`'s parser
registry (`registry.go`) matches on that specific compound suffix -- a manifest named
`manifest.json` is never picked up as a `.dam.json` file, even with an otherwise-valid body.

**Resulting edges are Tier 1, confidence 1.00, relationship `PROJECT_SIDECAR` -- always.**
`ProjectSidecarResolver` (`internal/graph/resolvers.go`) makes every reference in
`media_references`/`files` a *parent* of the manifest node, at fixed confidence 1.00 and tier 1,
regardless of `role`. This is the one fact a manifest producer most needs: listing a rendered
export under `role: "export"` does not make the render a *child* of its sources -- it asserts the
opposite (the render is a parent of the manifest node), which is backwards from the lineage
relationship a producer usually wants. See `docs/roadmap.md` and
`s3ntin3l8/branchdam-agent#5` for how the DaVinci Resolve hook resolves this (short answer: emit
source references only, never `role: "export"`).

---

## Schema Overview

```json
{
  "$schema": "http://json-schema.org/draft-07/schema#",
  "title": "DAMProjectManifest",
  "type": "object",
  "properties": {
    "version": {
      "type": "string",
      "example": "1.0"
    },
    "project_name": {
      "type": "string",
      "example": "Commercial Spot 2026"
    },
    "media_references": {
      "type": "array",
      "items": {
        "type": "object",
        "properties": {
          "raw_path": {
            "type": "string",
            "example": "D:\\Footage\\CameraA\\A001_C001.mov"
          },
          "path": {
            "type": "string",
            "description": "Accepted fallback if raw_path is absent or empty; ignored if raw_path is set."
          },
          "role": {
            "type": "string",
            "enum": ["media", "proxy", "export", "audio"],
            "default": "media",
            "description": "Not enforced by the parser -- any string is accepted and passed through as evidence unchanged; only an empty/missing role defaults to \"media\". The enum above documents intended usage, not a validated constraint."
          }
        },
        "required": ["raw_path"]
      }
    },
    "files": {
      "type": "array",
      "items": {
        "type": "string"
      }
    }
  },
  "required": ["version", "project_name"]
}
```

---

## Worked Example

```json
{
  "version": "1.0",
  "project_name": "Autumn Campaign",
  "media_references": [
    {
      "raw_path": "D:\\Footage\\Day1\\A001_C001_0817.ARW",
      "role": "media"
    },
    {
      "raw_path": "D:\\Footage\\Day1\\A001_C001_0817_proxy.mp4",
      "role": "proxy"
    },
    {
      "raw_path": "Z:\\Renders\\Autumn_Campaign_Final.mp4",
      "role": "export"
    }
  ]
}
```
