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
	EventNodeCreated        = "EVENT_NODE_CREATED"
	EventEdgeAttached       = "EVENT_EDGE_ATTACHED"
	EventNodeMoved          = "EVENT_NODE_MOVED"
	EventNodeDeleted        = "EVENT_NODE_DELETED"
	EventPathRebased        = "EVENT_PATH_REBASED"
	EventVirtualNodeCreated = "EVENT_VIRTUAL_NODE_CREATED"
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

	// ErrTrashedNode is fatal for the same reason as ErrArchivedNode: a
	// TRASHED node's file sits in .trash awaiting purge, and
	// RebaseNodePathByUUID would flip it back to ACTIVE, undoing a
	// user's delete. Restoring from trash is a deliberate, human action
	// (the web UI's restore), never a side effect of an agent rebase.
	ErrTrashedNode = errors.New("agent: node is trashed, refusing to rebase")

	// ErrWouldCreateCycle is fatal, not transient: the graph does not
	// un-cycle itself by retrying the same edge.
	ErrWouldCreateCycle = errors.New("agent: edge would create a cycle")

	// ErrArchiveFileNotYetPresent is deliberately transient, unlike
	// ErrReadOnlyRebase: unlike a genuinely-read-only, non-Tier-3 tier
	// (which never becomes writable by retrying), a Tier-3 target missing
	// its file today may well have it by the next drain pass -- the
	// workstation agent is still mid-copy. Left out of ProcessPending's
	// isFatal set on purpose, so it goes through the normal retry/backoff
	// path (RetryDelay, maxRetries) instead of failing permanently on the
	// first attempt; only marked FAILED once retries are exhausted with the
	// bytes still absent. Flagged by an independent review pass on #178.
	// The exponential backoff is what lets it outlast a multi-minute copy:
	// retried back to back, its attempts were spent within seconds.
	ErrArchiveFileNotYetPresent = errors.New("agent: rebase target resolves to Tier 3 but the file does not exist there yet")

	// ErrVirtualPathNotVirtual is fatal: the agent sent a filePath that
	// resolves to a physical storage location, not an is_virtual one.
	// Virtual nodes must live under a virtual storage namespace to avoid
	// colliding with real-file ingest paths.
	ErrVirtualPathNotVirtual = errors.New("agent: virtual node filePath resolves to a non-virtual storage location")

	// ErrCrossAgentCollision is fatal: a virtual node with a different
	// agentID already exists at the requested filePath. This means two
	// agents are trying to create the same virtual path — a cross-agent
	// data attribution error that must not be silently merged.
	ErrCrossAgentCollision = errors.New("agent: virtual node file_path already exists for a different agent")
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
	SourcePathHash     *string `json:"sourcePathHash,omitempty"`
	Phash              *int64  `json:"phash,omitempty"`
	CameraModel        *string `json:"cameraModel,omitempty"`
	CameraSerial       *string `json:"cameraSerial,omitempty"`
	LensModel          *string `json:"lensModel,omitempty"`
	CapturedAtUnix     *int64  `json:"capturedAtUnix,omitempty"`
	OriginalDocumentID *string `json:"originalDocumentId,omitempty"`
	DocumentID         *string `json:"documentId,omitempty"`
	DerivedFromID      *string `json:"derivedFromId,omitempty"`
	FilenameStem       *string `json:"filenameStem,omitempty"`
	// GPSLatitude/GPSLongitude are signed decimal degrees, the same
	// hemisphere-corrected convention as probe.ExifResult.GPSLatitude/
	// GPSLongitude (Composite:GPSLatitude/Longitude, not the unsigned
	// EXIF:GPSLatitude magnitude -- see probe.go's doc comment on that
	// distinction). applyNodeCreated writes them into node_metadata with
	// source="exiftool" so they land in exactly the rows a normal scan's
	// exifMetadata would have written, even though no exiftool process ever
	// ran on this path (#229).
	GPSLatitude  *float64 `json:"gpsLatitude,omitempty"`
	GPSLongitude *float64 `json:"gpsLongitude,omitempty"`
}

// EdgeAttachedPayload represents the payload for EVENT_EDGE_ATTACHED.
//
// Source is the parent, Target the child. When the device did not create
// the target (RequireNodeOwner), the edge is stored NEEDS_REVIEW whatever
// its confidence. See applyEdgeAttached.
type EdgeAttachedPayload struct {
	SourceNodeUUID string `json:"sourceNodeUuid"`
	TargetNodeUUID string `json:"targetNodeUuid"`
	// SourceNodeID/TargetNodeID are refused: a non-zero value fails the
	// event (ErrMalformedPayload). Integer row IDs are server-internal and
	// enumerable, so they let a device name nodes it has never been told
	// about; agents identify nodes by UUID only. Kept in the struct so such
	// a payload is recognised and rejected rather than silently ignored.
	SourceNodeID int64 `json:"sourceNodeId,omitempty"`
	TargetNodeID int64 `json:"targetNodeId,omitempty"`
	// RelationshipType must satisfy graph.IsValidRelationshipType; empty
	// defaults to DERIVED_FROM.
	RelationshipType string  `json:"relationshipType"`
	Confidence       float64 `json:"confidence,omitempty"`
	// Tier is clamped to [1, 2]: devices may assert direct (1) or
	// structural (2) evidence, never Tier 3, whose lower auto-accept
	// threshold is reserved for the server's own heuristics. 0 means 1.
	Tier         int64           `json:"tier,omitempty"`
	Resolver     string          `json:"resolver,omitempty"`
	EvidenceJSON json.RawMessage `json:"evidenceJson,omitempty"`

	// ReviewState is deprecated and ignored: review_state is now always
	// derived from (Confidence, Tier) via graph.AutoAcceptThresholdForTier,
	// the same way every other resolver's candidate is judged. A human
	// review decision (CONFIRMED/REJECTED) is never the agent's to make.
	// The field is kept so an existing agent payload still parses.
	ReviewState string `json:"reviewState,omitempty"`
}

// VirtualNodeCreated represents the payload for EVENT_VIRTUAL_NODE_CREATED.
// Unlike NodeCreatedPayload, it carries no file metadata — the node
// represents an integration project (Resolve timeline, Premiere sequence,
// FCPXML bundle), not a physical file on disk. The FilePath uses a
// conventional absolute prefix (e.g. "/virtual/resolve/<agentID>/My%20Documentary-<hash>")
// that the Guard resolves lexically via an is_virtual storage location.
// The hash suffix is generated by the agent from the raw timeline name
// (see resolve.uniqueTimelineSegment in branchdam-agent) so distinct raw
// names that sanitize to the same form (e.g. "A/B" vs "A-B") never
// collide on the file_path UNIQUE constraint.
type VirtualNodeCreated struct {
	NodeUUID     string          `json:"nodeUuid"`
	FilePath     string          `json:"filePath"`
	DisplayName  string          `json:"displayName"`
	ProjectType  string          `json:"projectType"`
	EvidenceJSON json.RawMessage `json:"evidenceJson,omitempty"`
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
	FullHash                *string `json:"fullHash,omitempty"`
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

// Retry policy for transiently failing events. A retryable failure defers
// the row (event_queue.next_attempt_at) by RetryDelay(attempts) instead of
// retrying it on the very next drain pass, so a condition that clears in
// minutes -- ErrArchiveFileNotYetPresent while the agent is still copying a
// large file into the archive -- no longer exhausts the retry budget in
// seconds. With these defaults an event is retried for roughly ten hours
// (5s, 10s, 20s, ... capped at 1h) before it is marked FAILED.
const (
	// DefaultMaxRetries is the total number of attempts before a
	// retryable event is marked FAILED.
	DefaultMaxRetries = 20
	// DefaultRetryBaseDelay is the deferral after the first failed attempt;
	// each further failure doubles it.
	DefaultRetryBaseDelay = 5 * time.Second
	// DefaultRetryMaxDelay caps a single deferral.
	DefaultRetryMaxDelay = time.Hour
)

// RetryDelay returns how long to defer an event after its attempts-th
// failed attempt (1-based): DefaultRetryBaseDelay doubled per prior
// failure, capped at DefaultRetryMaxDelay.
func RetryDelay(attempts int) time.Duration {
	d := DefaultRetryBaseDelay
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= DefaultRetryMaxDelay {
			return DefaultRetryMaxDelay
		}
	}
	return d
}

// DrainStats tracks batch processing execution metrics.
type DrainStats struct {
	Processed int
	Failed    int
	Duration  time.Duration
}
