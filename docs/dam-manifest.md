# `.dam.json` Manifest Specification

The `.dam.json` manifest is `branchDAM`'s native project manifest format. It allows projects to explicitly document their media references, roles, and output exports.

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
          "role": {
            "type": "string",
            "enum": ["media", "proxy", "export", "audio"],
            "default": "media"
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
