# Project-File Media Reference Path Mapping

This document specifies the path-resolution strategy, configuration schema, ambiguity policy, and missing-node handling for server-side Tier-1 project file introspection (`.dam.json`, `.drp`, `.fcpxml`, `.edl`).

---

## 1. Primary Path-Resolution Strategy

Project files reference media by the path the editing workstation saw when the project was created (e.g. Windows paths like `D:\Footage\Clip01.mov` or macOS paths like `/Volumes/Video/Projects/Clip01.mov`). The `branchDAM` server container mounts local/network storage at container paths (e.g., `/storage/projects/Footage/Clip01.mov`).

To map referenced raw path strings to container paths of live `media_nodes`, `branchDAM` applies a 3-step resolution strategy in order:

### Step 1: Exact Container Path Match
If the reference raw path is already an absolute container path (or relative path under a mounted storage location) and matches a live node's `file_path` exactly in the database, resolve immediately.

### Step 2: Operator-Declared Prefix Rewrite (Primary Strategy)
When raw reference paths originate from external workstations, operator-declared `path_rewrites` rules map host path prefixes (including Windows backslashes) to container path prefixes.
- Convert backslashes `\` to forward slashes `/`.
- Strip trailing/leading slashes as appropriate and evaluate matching `from` prefixes.
- Replace the matching `from` prefix with the target `to` container prefix.
- Perform a direct lookup in `media_nodes` by the rewritten container `file_path`.

### Step 3: Fallback Matching (Basename + Size)
If prefix rewrite does not yield a live node match (e.g., files were moved under a subfolder or path rewrites are partially configured):
- Search live `media_nodes` by exact `file_name` (basename).
- **Not yet implemented / aspirational:** if the project file format includes asset file size metadata (e.g., `.dam.json` or `.fcpxml`), filter candidate nodes to match `size_bytes`. `internal/projectfile.Reference` (`internal/projectfile/types.go`) has no size field today, so no parser currently extracts or supplies one for this filter to consume; this is conservative (fewer auto-resolved edges, not incorrect ones), not unsafe.
- If a single unique candidate node matches, evaluate candidate.
- If multiple candidate nodes exist after filtering, apply the ambiguity policy (Section 3 policy).

---

## 2. Configuration Schema (`config.yaml`)

Operator-declared path rewrites are configured under the `pathRewrites` section in `config.yaml`:

```yaml
# Top-level path rewrites configuration in config.yaml
pathRewrites:
  - from: "D:\\Footage\\"
    to: "/storage/projects/Footage/"
  - from: "Z:/Volumes/Video/"
    to: "/storage/projects/Video/"
  - from: "/Users/editor/Movies/"
    to: "/storage/projects/Movies/"
```

### Go Struct Definition (`internal/config/config.go`)
```go
type PathRewrite struct {
    From string `yaml:"from"`
    To   string `yaml:"to"`
}

type Config struct {
    // ... existing fields ...
    PathRewrites []PathRewrite `yaml:"pathRewrites"`
}
```

### Worked Transformation Example
```yaml
# Input Raw Reference Path:
"D:\\Footage\\Scene01\\ClipA.mov"

# Configured Path Rewrite Rule:
from: "D:\\Footage\\"
to:   "/storage/projects/Footage/"

# Step-by-Step Transformation:
1. Normalize backslashes:   "D:/Footage/Scene01/ClipA.mov"
2. Match prefix "D:/Footage/": Matches rule
3. Replace prefix with "to": "/storage/projects/Footage/Scene01/ClipA.mov"
4. Clean path result:        "/storage/projects/Footage/Scene01/ClipA.mov"
```

### Normalization Algorithm
1. Normalize Windows backslashes `\` in both `From` and `RawPath` to `/`.
2. Perform case-insensitive matching on Windows drive letters (e.g. `d:/` vs `D:/`).
3. If `RawPath` starts with `From`, replace `From` with `To` and clean the resulting path via `filepath.Clean`.

---

## 3. Policy for Ambiguous Matches

> [!WARNING]
> Tier 1 emits edges at confidence **1.00**, which is the highest confidence tier trusted by all downstream graph resolvers and UI views. A wrong Tier-1 edge creates false lineage links that corrupt project history.

If path resolution (such as fallback basename matching) identifies **multiple live `media_nodes` candidates**:
- **Action**: Do **NOT** emit a candidate edge.
- **Logging**: Log a `WARN` message with context (project node ID, raw reference path, candidate node IDs).
- **Rationale**: An unlinked edge is safe and will be flagged for review or resolved by other tiers, whereas an incorrect Tier-1 edge cannot be automatically overridden by lower-tier resolvers.

---

## 4. Policy for Missing Referenced Files

If a project file references a media asset that is **not currently present in `media_nodes`** (e.g., the media file has not been scanned yet, or lives on an unmounted volume):
- **Action**: **Skip** the candidate edge for this reference.
- **No Placeholder Nodes**: Do **NOT** create placeholder or dummy nodes in `media_nodes`.
- **Re-resolution**: When the media file is eventually discovered and indexed on a subsequent scan, running graph resolution again will naturally discover the node and establish the `PROJECT_SIDECAR` edge.
