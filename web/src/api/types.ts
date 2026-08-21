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

export type ThumbState = "PENDING" | "READY" | "UNSUPPORTED" | "FAILED";

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
  thumbState: ThumbState;
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
  nodeUuid: string;
  fileName: string;
  filePath: string;
  capturedAtUnix?: number;
  cameraModel?: string;
  phash?: number;
  thumbState: ThumbState;
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

export type SyncStatus =
  | "NOT_QUEUED"
  | "PENDING_CLOUD_PUSH"
  | "PUSHING"
  | "PUSHED"
  | "PUSH_FAILED";

export interface SyncState {
  remote: string;
  syncStatus: SyncStatus;
  remoteAssetId?: string;
  lastError?: string;
  lastAttemptAt?: number;
  retryCount: number;
  exhausted: boolean;
}

export interface AssetSyncStatus {
  sync: SyncState[];
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

export interface StartScanRequest {
  storageLocationId: number;
  // Differential, if true, uses the touch-only fast path instead of
  // re-hashing every file -- permitted specifically against
  // TIER3_MASTER_ARCHIVE locations (#226). Omitted or false is a full,
  // non-differential scan -- unaffected by this field's addition.
  differential?: boolean;
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

export interface PruneRequest {
  storageLocationId: number;
  nodeIds?: number[];
  // Defaults to false (dry-run) when omitted -- an empty request must
  // plan, never purge.
  execute?: boolean;
}

export interface PruneCandidate {
  nodeId: number;
  filePath: string;
  sizeBytes: number;
  purged: boolean;
  error?: string;
}

export interface PruneResponse {
  executed: boolean;
  candidates: PruneCandidate[];
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

export interface AgentHelloResponse {
  ok: boolean;
  version: string;
}

export interface AgentHandshakeRequest {
  agentId: string;
  clientVersion?: string;
  lastProcessedEventUuid?: string;
}

export interface AgentHandshakeResponse {
  ok: boolean;
  serverVersion: string;
  serverTimeUnix: number;
  acknowledgedEventUuid?: string;
  pendingEventsCount: number;
}

export interface AgentEventRequest {
  agentId: string;
  eventType:
    | "EVENT_NODE_CREATED"
    | "EVENT_EDGE_ATTACHED"
    | "EVENT_NODE_MOVED"
    | "EVENT_NODE_DELETED"
    | "EVENT_PATH_REBASED";
  payload: string;
}

export interface AgentEventResponse {
  eventId: string;
}

export interface AgentRebaseRequest {
  nodeUuid: string;
  targetPath: string;
  mtimeUnix?: number;
  fileName?: string;
  fileExt?: string;
  sizeBytes?: number;
  fastHash?: string;
  storageLocationId?: number;
}

export interface AgentRebaseResponse {
  id: number;
  nodeUuid: string;
  storageLocationId: number;
  filePath: string;
  status: "REBASED" | "CREATED";
}
