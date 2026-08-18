// Mirrors internal/httpapi/routes.go's DTOs exactly -- field names,
// optionality, and shape. Keep in sync by hand until PR 10+ adds an
// OpenAPI-generated client from Huma's /openapi.json.

export type PrincipalKind = "user" | "machine";

export interface Me {
  kind: PrincipalKind;
  name?: string;
  email?: string;
  groups?: string[];
}

export interface PathRewrite {
  from: string;
  to: string;
}

export interface Config {
  version: string;
  pathRewrites?: PathRewrite[];
}

export interface StorageLocation {
  id: number;
  name: string;
  rootPath: string;
  tier:
    | "TIER0_LOCAL_STAGING"
    | "TIER1_LOCAL_SCRATCH"
    | "TIER2_EXPORTS"
    | "TIER3_MASTER_ARCHIVE"
    | "PROJECTS";
  readOnly: boolean;
  prunable: boolean;
}

export interface Asset {
  id: number;
  nodeUuid: string;
  filePath: string;
  fileName: string;
  fileExt: string;
  sizeBytes: number;
  fastHash?: string;
  fullHash?: string;
  indexingStatus: "PENDING" | "INDEXED_SHALLOW" | "INDEXED_FULL" | "INDEX_FAILED";
  graphStatus: "UNLINKED" | "LINKED" | "NEEDS_REVIEW" | "ROOT";
  lifecycleState: "ACTIVE" | "MISSING" | "ARCHIVED" | "HIDDEN";
  storageLocationId: number;
  originalDocumentId?: string;
  cameraModel?: string;
}

export interface Edge {
  id: number;
  sourceNodeId: number;
  targetNodeId: number;
  relationshipType: "DERIVED_FROM" | "FINAL_EXPORT" | "PROXY_OF" | "PROJECT_SIDECAR" | "DUPLICATE_OF";
  confidence: number;
  reviewState: "AUTO_ACCEPTED" | "NEEDS_REVIEW" | "CONFIRMED" | "REJECTED";
  resolver: string;
}

export interface AssetGraph {
  parents: Edge[];
  children: Edge[];
}

export interface AuditNode {
  id: number;
  fileName: string;
  filePath: string;
  capturedAtUnix?: number;
  cameraModel?: string;
  phash?: number;
}

export interface AuditEntry {
  id: number;
  sourceNodeId: number;
  targetNodeId: number;
  relationshipType: Edge["relationshipType"];
  confidence: number;
  tier: number;
  resolver: string;
  evidenceJson: string;
  parentAlive: boolean;
  parentMissing: boolean;
  sourceNode: AuditNode;
  targetNode: AuditNode;
  captureDeltaSeconds?: number;
  phashDistance?: number;
}

export interface CreateEdgeInput {
  sourceNodeId: number;
  targetNodeId: number;
  relationshipType: Edge["relationshipType"];
}

export interface InheritMetadataResponse {
  inherited: Record<string, string>;
}

export interface ScanJob {
  id: number;
  kind: "FULL_SCAN" | "INCREMENTAL" | "WATCH";
  state: "RUNNING" | "COMPLETED" | "FAILED" | "CANCELLED";
  filesSeen: number;
  filesHashed: number;
  filesFailed: number;
  edgesCreated: number;
  lastError?: string;
}

export interface LineageResponse {
  rootId: number;
  nodes: Asset[];
  edges: Edge[];
}

export interface StorageLocationHealth {
  id: number;
  name: string;
  rootPath: string;
  tier: StorageLocation["tier"];
  readOnly: boolean;
  prunable: boolean;
  isActive: boolean;
  nodeCount: number;
  totalBytes: number;
  usedBytes: number;
  freeBytes: number;
  isDegraded: boolean;
  degradedMessage?: string;
}

export interface StorageQueueHealth {
  workerPoolInFlight: number;
  workerPoolQueued: number;
  workerPoolCapacity: number;
  workerCount: number;
  runningScanJobs: number;
}

export interface StorageHealth {
  locations: StorageLocationHealth[];
  queues: StorageQueueHealth;
}

export interface AssetQueryParams {
  limit?: number;
  offset?: number;
  cameraModel?: string;
  graphStatus?: Asset["graphStatus"];
  storageLocationId?: number;
  lifecycleState?: Asset["lifecycleState"];
  unlinkedOnly?: boolean;
}

export interface JobsQueryParams {
  limit?: number;
  offset?: number;
  kind?: ScanJob["kind"];
  state?: ScanJob["state"];
}
