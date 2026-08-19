// Package agent implements the server-side agent-server contract: event_queue
// draining, SYNC_HANDSHAKE, and path rebasing (Phase 8).
package agent

import (
	"encoding/json"
	"errors"
	"time"
)

// Event type constants matching the event_queue.event_type CHECK constraint.
const (
	EventNodeCreated  = "EVENT_NODE_CREATED"
	EventEdgeAttached = "EVENT_EDGE_ATTACHED"
	EventNodeMoved    = "EVENT_NODE_MOVED"
	EventNodeDeleted  = "EVENT_NODE_DELETED"
	EventPathRebased  = "EVENT_PATH_REBASED"
)

// Common errors.
var (
	ErrUnknownEventType = errors.New("agent: unknown event type")
	ErrMalformedPayload = errors.New("agent: malformed event payload")
	ErrNodeNotFound     = errors.New("agent: media node not found")
	ErrInvalidNodeUUID  = errors.New("agent: invalid or empty node_uuid")
	ErrReadOnlyRebase   = errors.New("agent: rebase target resolves to read-only tier")

	// ErrArchivedNode is fatal, not transient: an ARCHIVED node never
	// becomes un-archived by retrying. Rebasing one in place would resurrect
	// a superseded version -- see RebaseNodePathByUUID's lack of a
	// lifecycle_state guard and media_nodes' superseded_by/ARCHIVED CHECK.
	ErrArchivedNode = errors.New("agent: node is archived, refusing to rebase")

	// ErrWouldCreateCycle is fatal, not transient: the graph does not
	// un-cycle itself by retrying the same edge.
	ErrWouldCreateCycle = errors.New("agent: edge would create a cycle")

	// ErrArchiveFileNotYetPresent is deliberately transient, unlike
	// ErrReadOnlyRebase: unlike a genuinely-read-only, non-Tier-3 tier
	// (which never becomes writable by retrying), a Tier-3 target missing
	// its file today may well have it by the next drain pass -- the
	// workstation agent is still mid-copy. Left out of ProcessPending's
	// isFatal set on purpose, so it goes through the normal retry/backoff
	// path (maxRetries) instead of failing permanently on the first
	// attempt; only marked FAILED once retries are exhausted with the
	// bytes still absent. Flagged by an independent review pass on #178.
	ErrArchiveFileNotYetPresent = errors.New("agent: rebase target resolves to Tier 3 but the file does not exist there yet")
)

// NodeCreatedPayload represents the payload for EVENT_NODE_CREATED.
type NodeCreatedPayload struct {
	NodeUUID string `json:"nodeUuid"`
	// StorageLocationID is deprecated and ignored: storage_location_id is
	// always derived from FilePath via storage.Guard.Resolve, the only
	// verifiable source -- Guard exposes no lookup-by-ID, so an
	// agent-supplied ID is fundamentally unverifiable.
	StorageLocationID  int64   `json:"storageLocationId,omitempty"`
	FilePath           string  `json:"filePath"`
	FileName           string  `json:"fileName,omitempty"`
	FileExt            string  `json:"fileExt,omitempty"`
	SizeBytes          int64   `json:"sizeBytes,omitempty"`
	MtimeUnix          int64   `json:"mtimeUnix,omitempty"`
	FastHash           *string `json:"fastHash,omitempty"`
	FullHash           *string `json:"fullHash,omitempty"`
	Phash              *int64  `json:"phash,omitempty"`
	CameraModel        *string `json:"cameraModel,omitempty"`
	CameraSerial       *string `json:"cameraSerial,omitempty"`
	LensModel          *string `json:"lensModel,omitempty"`
	CapturedAtUnix     *int64  `json:"capturedAtUnix,omitempty"`
	OriginalDocumentID *string `json:"originalDocumentId,omitempty"`
	DocumentID         *string `json:"documentId,omitempty"`
	DerivedFromID      *string `json:"derivedFromId,omitempty"`
	FilenameStem       *string `json:"filenameStem,omitempty"`
}

// EdgeAttachedPayload represents the payload for EVENT_EDGE_ATTACHED.
type EdgeAttachedPayload struct {
	SourceNodeUUID   string          `json:"sourceNodeUuid"`
	TargetNodeUUID   string          `json:"targetNodeUuid"`
	SourceNodeID     int64           `json:"sourceNodeId,omitempty"`
	TargetNodeID     int64           `json:"targetNodeId,omitempty"`
	RelationshipType string          `json:"relationshipType"`
	Confidence       float64         `json:"confidence,omitempty"`
	Tier             int64           `json:"tier,omitempty"`
	Resolver         string          `json:"resolver,omitempty"`
	EvidenceJSON     json.RawMessage `json:"evidenceJson,omitempty"`

	// ReviewState is deprecated and ignored: review_state is now always
	// derived from (Confidence, Tier) via graph.AutoAcceptThresholdForTier,
	// the same way every other resolver's candidate is judged. A human
	// review decision (CONFIRMED/REJECTED) is never the agent's to make.
	// The field is kept so an existing agent payload still parses.
	ReviewState string `json:"reviewState,omitempty"`
}

// NodeMovedPayload represents the payload for EVENT_NODE_MOVED.
type NodeMovedPayload struct {
	NodeUUID    string `json:"nodeUuid"`
	NewFilePath string `json:"newFilePath"`
	NewFileName string `json:"newFileName,omitempty"`
	// NewStorageLocationID is deprecated and ignored -- see
	// NodeCreatedPayload.StorageLocationID.
	NewStorageLocationID int64 `json:"newStorageLocationId,omitempty"`
	MtimeUnix            int64 `json:"mtimeUnix,omitempty"`
}

// NodeDeletedPayload represents the payload for EVENT_NODE_DELETED.
type NodeDeletedPayload struct {
	NodeUUID string `json:"nodeUuid"`
}

// PathRebasedPayload represents the payload for EVENT_PATH_REBASED.
type PathRebasedPayload struct {
	NodeUUID       string `json:"nodeUuid"`
	TargetFilePath string `json:"targetFilePath"`
	TargetFileName string `json:"targetFileName,omitempty"`
	// TargetStorageLocationID is deprecated and ignored -- see
	// NodeCreatedPayload.StorageLocationID.
	TargetStorageLocationID int64   `json:"targetStorageLocationId,omitempty"`
	MtimeUnix               int64   `json:"mtimeUnix,omitempty"`
	FastHash                *string `json:"fastHash,omitempty"`
	SizeBytes               int64   `json:"sizeBytes,omitempty"`
}

// HandshakeRequest represents the input for POST /api/v1/agent/handshake.
type HandshakeRequest struct {
	AgentID                string `json:"agentId"`
	ClientVersion          string `json:"clientVersion,omitempty"`
	LastProcessedEventUUID string `json:"lastProcessedEventUuid,omitempty"`
}

// HandshakeResponse represents the output of POST /api/v1/agent/handshake.
type HandshakeResponse struct {
	OK                    bool   `json:"ok"`
	ServerVersion         string `json:"serverVersion"`
	ServerTimeUnix        int64  `json:"serverTimeUnix"`
	AcknowledgedEventUUID string `json:"acknowledgedEventUuid,omitempty"`
	PendingEventsCount    int64  `json:"pendingEventsCount"`
}

// RebaseRequest represents the input for POST /api/v1/agent/rebase.
type RebaseRequest struct {
	NodeUUID          string  `json:"nodeUuid"`
	TargetPath        string  `json:"targetPath"`
	MtimeUnix         int64   `json:"mtimeUnix,omitempty"`
	FileName          string  `json:"fileName,omitempty"`
	FileExt           string  `json:"fileExt,omitempty"`
	SizeBytes         int64   `json:"sizeBytes,omitempty"`
	FastHash          *string `json:"fastHash,omitempty"`
	StorageLocationID int64   `json:"storageLocationId,omitempty"`
}

// RebaseResponse represents the output of POST /api/v1/agent/rebase.
type RebaseResponse struct {
	ID                int64  `json:"id"`
	NodeUUID          string `json:"nodeUuid"`
	StorageLocationID int64  `json:"storageLocationId"`
	FilePath          string `json:"filePath"`
	Status            string `json:"status"` // "REBASED" or "CREATED"
}

// DefaultMaxRetries is the default poison-pill retry ceiling before an event is marked FAILED.
const DefaultMaxRetries = 3

// DrainStats tracks batch processing execution metrics.
type DrainStats struct {
	Processed int
	Failed    int
	Duration  time.Duration
}
