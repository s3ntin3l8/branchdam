package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// Drainer processes PENDING rows in event_queue oldest-first and applies
// the corresponding state changes to media_nodes and media_edges.
type Drainer struct {
	db          *db.DB
	guard       *storage.Guard
	log         *slog.Logger
	maxRetries  int
	backoffWait time.Duration
	mu          sync.Mutex
}

// NewDrainer creates an agent event queue drainer.
func NewDrainer(database *db.DB, guard *storage.Guard, log *slog.Logger) *Drainer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Drainer{
		db:         database,
		guard:      guard,
		log:        log,
		maxRetries: DefaultMaxRetries,
	}
}

// SetMaxRetries overrides the default poison-pill retry threshold.
func (d *Drainer) SetMaxRetries(n int) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if n <= 0 {
		n = 1
	}
	d.maxRetries = n
}

// SetRetryBackoff configures the backoff sleep duration in DrainAll when a batch makes no progress.
func (d *Drainer) SetRetryBackoff(delay time.Duration) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.backoffWait = delay
}

// ProcessPending claims up to batchSize PENDING rows oldest-first and applies them.
// Malformed payloads and poison-pill events are marked FAILED with error_log
// and never crash the worker or block the queue head.
func (d *Drainer) ProcessPending(ctx context.Context, batchSize int) (DrainStats, error) {
	if batchSize <= 0 {
		batchSize = 50
	}

	start := time.Now()
	var stats DrainStats

	var events []sqlcgen.ListPendingAgentEventsRow
	err := d.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		events, err = q.ListPendingAgentEvents(ctx, int64(batchSize))
		return err
	})
	if err != nil {
		return stats, fmt.Errorf("list pending agent events: %w", err)
	}

	if len(events) == 0 {
		stats.Duration = time.Since(start)
		return stats, nil
	}

	d.mu.Lock()
	maxRetries := d.maxRetries
	d.mu.Unlock()

	for _, ev := range events {
		if err := ctx.Err(); err != nil {
			stats.Duration = time.Since(start)
			return stats, err
		}

		processErr := d.db.InTx(ctx, func(q *sqlcgen.Queries) error {
			if err := d.applyEvent(ctx, q, ev); err != nil {
				return err
			}
			return q.MarkAgentEventProcessed(ctx, ev.ID)
		})

		if processErr == nil {
			stats.Processed++
			continue
		}

		// Handle error: check if fatal poison-pill or exceeded retry limit.
		// ErrArchivedNode and ErrWouldCreateCycle join the fatal set for the
		// same reason as the rest: retrying doesn't change the outcome --
		// an ARCHIVED node doesn't un-archive itself and the graph doesn't
		// un-cycle itself.
		isFatal := errors.Is(processErr, ErrMalformedPayload) ||
			errors.Is(processErr, ErrUnknownEventType) ||
			errors.Is(processErr, ErrInvalidNodeUUID) ||
			errors.Is(processErr, ErrReadOnlyRebase) ||
			errors.Is(processErr, ErrArchivedNode) ||
			errors.Is(processErr, ErrWouldCreateCycle) ||
			strings.Contains(processErr.Error(), "constraint failed")

		attempts := int(ev.RetryCount) + 1

		if isFatal || attempts >= maxRetries {
			d.log.Warn("agent: event failed permanently",
				"eventID", ev.ID, "eventUUID", ev.EventUuid, "eventType", ev.EventType, "attempts", attempts, "err", processErr)

			// Mark FAILED with error_log in its own transaction so queue head unblocks.
			if err := d.db.InTx(ctx, func(q *sqlcgen.Queries) error {
				return q.MarkAgentEventFailed(ctx, sqlcgen.MarkAgentEventFailedParams{
					ID:       ev.ID,
					ErrorLog: sql.NullString{String: processErr.Error(), Valid: true},
				})
			}); err != nil {
				d.log.Error("agent: failed to mark event FAILED in db", "eventID", ev.ID, "err", err)
			}
			stats.Failed++
		} else {
			d.log.Warn("agent: transient event error, will retry",
				"eventID", ev.ID, "eventUUID", ev.EventUuid, "eventType", ev.EventType, "attempts", attempts, "err", processErr)

			// Persist retry count in DB so retries survive crashes and multi-instance restarts.
			if err := d.db.InTx(ctx, func(q *sqlcgen.Queries) error {
				return q.IncrementAgentEventRetry(ctx, sqlcgen.IncrementAgentEventRetryParams{
					ID:       ev.ID,
					ErrorLog: sql.NullString{String: processErr.Error(), Valid: true},
				})
			}); err != nil {
				d.log.Error("agent: failed to increment event retry in db", "eventID", ev.ID, "err", err)
			}
		}
	}

	stats.Duration = time.Since(start)
	return stats, nil
}

// DrainAll continuously processes pending events until the queue is empty or ctx is cancelled.
func (d *Drainer) DrainAll(ctx context.Context) (DrainStats, error) {
	var totalStats DrainStats
	start := time.Now()

	for {
		if err := ctx.Err(); err != nil {
			totalStats.Duration = time.Since(start)
			return totalStats, err
		}

		var pendingCount int64
		err := d.db.InTx(ctx, func(q *sqlcgen.Queries) error {
			var err error
			pendingCount, err = q.CountPendingAgentEvents(ctx)
			return err
		})
		if err != nil {
			totalStats.Duration = time.Since(start)
			return totalStats, err
		}

		if pendingCount == 0 {
			break
		}

		stats, err := d.ProcessPending(ctx, 100)
		if err != nil {
			totalStats.Duration = time.Since(start)
			return totalStats, err
		}

		totalStats.Processed += stats.Processed
		totalStats.Failed += stats.Failed

		if stats.Processed == 0 && stats.Failed == 0 {
			// If all pending events are in retry cooldown or unprogressed, back off or break
			d.mu.Lock()
			backoff := d.backoffWait
			d.mu.Unlock()

			if backoff <= 0 {
				break
			}
			timer := time.NewTimer(backoff)
			select {
			case <-timer.C:
				// retry next iteration
			case <-ctx.Done():
				timer.Stop()
				totalStats.Duration = time.Since(start)
				return totalStats, ctx.Err()
			}
		}
	}

	totalStats.Duration = time.Since(start)
	return totalStats, nil
}

func (d *Drainer) applyEvent(ctx context.Context, q *sqlcgen.Queries, ev sqlcgen.ListPendingAgentEventsRow) error {
	switch ev.EventType {
	case EventNodeCreated:
		return d.applyNodeCreated(ctx, q, ev)
	case EventEdgeAttached:
		return d.applyEdgeAttached(ctx, q, ev)
	case EventNodeMoved:
		return d.applyNodeMoved(ctx, q, ev)
	case EventNodeDeleted:
		return d.applyNodeDeleted(ctx, q, ev)
	case EventPathRebased:
		return d.applyPathRebased(ctx, q, ev)
	default:
		return fmt.Errorf("%w: %s", ErrUnknownEventType, ev.EventType)
	}
}

func (d *Drainer) applyNodeCreated(ctx context.Context, q *sqlcgen.Queries, ev sqlcgen.ListPendingAgentEventsRow) error {
	var p NodeCreatedPayload
	if err := json.Unmarshal([]byte(ev.PayloadJson), &p); err != nil {
		return fmt.Errorf("%w: unmarshal node created: %v", ErrMalformedPayload, err)
	}
	if p.NodeUUID == "" {
		return fmt.Errorf("%w: missing nodeUuid in node created payload", ErrInvalidNodeUUID)
	}
	if p.FilePath == "" {
		return fmt.Errorf("%w: missing filePath in node created payload", ErrMalformedPayload)
	}

	// Idempotency: if node already exists with this node_uuid, it's a no-op success.
	if _, err := q.GetMediaNodeByUUID(ctx, p.NodeUUID); err == nil {
		return nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup node by uuid: %w", err)
	}

	// storage_location_id always comes from Guard.Resolve(FilePath), never
	// from the payload -- storage.Guard exposes no lookup-by-ID, so a
	// payload-supplied StorageLocationID is fundamentally unverifiable and
	// is ignored (see NodeCreatedPayload.StorageLocationID). A nil guard is
	// a server misconfiguration, not a poison-pill payload, so it's a
	// plain (non-fatal) error: the event stays PENDING/retries rather than
	// being marked FAILED for a problem the payload didn't cause.
	if d.guard == nil {
		return fmt.Errorf("agent: storage guard not configured, deferring node create for %q", p.FilePath)
	}
	loc, err := d.guard.Resolve(p.FilePath)
	if err != nil {
		return fmt.Errorf("%w: file path %q does not resolve to any known storage location: %v", ErrMalformedPayload, p.FilePath, err)
	}
	locID := loc.ID

	fileName := p.FileName
	if fileName == "" {
		fileName = filepath.Base(p.FilePath)
	}
	fileExt := p.FileExt
	if fileExt == "" {
		fileExt = filepath.Ext(p.FilePath)
	}

	indexingStatus := "INDEXED_SHALLOW"
	if p.FullHash != nil && *p.FullHash != "" {
		indexingStatus = "INDEXED_FULL"
	}

	var nullFastHash *string
	if p.FastHash != nil && *p.FastHash != "" {
		nullFastHash = p.FastHash
	}
	var nullFullHash *string
	if p.FullHash != nil && *p.FullHash != "" {
		nullFullHash = p.FullHash
	}

	var nullPhash sql.NullInt64
	if p.Phash != nil {
		nullPhash = sql.NullInt64{Int64: *p.Phash, Valid: true}
	}
	var nullCameraModel sql.NullString
	if p.CameraModel != nil && *p.CameraModel != "" {
		nullCameraModel = sql.NullString{String: *p.CameraModel, Valid: true}
	}
	var nullCameraSerial sql.NullString
	if p.CameraSerial != nil && *p.CameraSerial != "" {
		nullCameraSerial = sql.NullString{String: *p.CameraSerial, Valid: true}
	}
	var nullLensModel sql.NullString
	if p.LensModel != nil && *p.LensModel != "" {
		nullLensModel = sql.NullString{String: *p.LensModel, Valid: true}
	}
	var nullCapturedAt sql.NullInt64
	if p.CapturedAtUnix != nil {
		nullCapturedAt = sql.NullInt64{Int64: *p.CapturedAtUnix, Valid: true}
	}
	var nullOrigDocID sql.NullString
	if p.OriginalDocumentID != nil && *p.OriginalDocumentID != "" {
		nullOrigDocID = sql.NullString{String: *p.OriginalDocumentID, Valid: true}
	}
	var nullDocID sql.NullString
	if p.DocumentID != nil && *p.DocumentID != "" {
		nullDocID = sql.NullString{String: *p.DocumentID, Valid: true}
	}
	var nullDerivedFromID sql.NullString
	if p.DerivedFromID != nil && *p.DerivedFromID != "" {
		nullDerivedFromID = sql.NullString{String: *p.DerivedFromID, Valid: true}
	}
	var nullFilenameStem sql.NullString
	if p.FilenameStem != nil && *p.FilenameStem != "" {
		nullFilenameStem = sql.NullString{String: *p.FilenameStem, Valid: true}
	}

	mtime := p.MtimeUnix
	if mtime == 0 {
		mtime = time.Now().Unix()
	}

	_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
		NodeUuid:           p.NodeUUID,
		StorageLocationID:  locID,
		FilePath:           p.FilePath,
		FileName:           fileName,
		FileExt:            fileExt,
		SizeBytes:          p.SizeBytes,
		MtimeUnix:          mtime,
		FastHash:           nullFastHash,
		FullHash:           nullFullHash,
		Phash:              nullPhash,
		IndexingStatus:     indexingStatus,
		GraphStatus:        "UNLINKED",
		LifecycleState:     "ACTIVE",
		OriginalDocumentID: nullOrigDocID,
		DocumentID:         nullDocID,
		DerivedFromID:      nullDerivedFromID,
		CapturedAtUnix:     nullCapturedAt,
		CameraModel:        nullCameraModel,
		FilenameStem:       nullFilenameStem,
		CameraSerial:       nullCameraSerial,
		LensModel:          nullLensModel,
	})
	return err
}

func (d *Drainer) applyEdgeAttached(ctx context.Context, q *sqlcgen.Queries, ev sqlcgen.ListPendingAgentEventsRow) error {
	var p EdgeAttachedPayload
	if err := json.Unmarshal([]byte(ev.PayloadJson), &p); err != nil {
		return fmt.Errorf("%w: unmarshal edge attached: %v", ErrMalformedPayload, err)
	}

	sourceID := p.SourceNodeID
	if sourceID == 0 {
		if p.SourceNodeUUID == "" {
			return fmt.Errorf("%w: missing sourceNodeUuid in edge attached payload", ErrMalformedPayload)
		}
		srcNode, err := q.GetMediaNodeByUUID(ctx, p.SourceNodeUUID)
		if err != nil {
			return fmt.Errorf("resolve source node %q: %w", p.SourceNodeUUID, err)
		}
		sourceID = srcNode.ID
	}

	targetID := p.TargetNodeID
	if targetID == 0 {
		if p.TargetNodeUUID == "" {
			return fmt.Errorf("%w: missing targetNodeUuid in edge attached payload", ErrMalformedPayload)
		}
		tgtNode, err := q.GetMediaNodeByUUID(ctx, p.TargetNodeUUID)
		if err != nil {
			return fmt.Errorf("resolve target node %q: %w", p.TargetNodeUUID, err)
		}
		targetID = tgtNode.ID
	}

	if p.RelationshipType == "" {
		p.RelationshipType = "DERIVED_FROM"
	}
	tier := p.Tier
	if tier <= 0 {
		tier = 1
	}
	resolver := p.Resolver
	if resolver == "" {
		resolver = "agent"
	}
	evidenceJSON := string(p.EvidenceJSON)
	if evidenceJSON == "" {
		evidenceJSON = "{}"
	}

	// A human review decision (CONFIRMED/REJECTED) is never the agent's to
	// make -- reject it loudly rather than let it silently fail the
	// reviewed_at CHECK constraint as an opaque "constraint failed".
	if p.ReviewState == "CONFIRMED" || p.ReviewState == "REJECTED" {
		return fmt.Errorf("%w: reviewState %q is a human-only review decision, not settable by an agent event", ErrMalformedPayload, p.ReviewState)
	}

	// confidence and review_state are never taken from the payload as-is:
	// an agent minting its own AUTO_ACCEPTED at any confidence would bypass
	// graph.AutoAcceptThresholdForTier, the same per-tier threshold every
	// other resolver is held to. Require an explicit, in-range confidence
	// and derive review_state exactly as graph.Engine does.
	if p.Confidence <= 0 || p.Confidence > 1 {
		return fmt.Errorf("%w: confidence must be in (0, 1], got %v", ErrMalformedPayload, p.Confidence)
	}
	if p.Confidence < graph.NeedsReviewFloor {
		return fmt.Errorf("%w: confidence %v is below needsReviewFloor %v, refusing to create edge", ErrMalformedPayload, p.Confidence, graph.NeedsReviewFloor)
	}
	reviewState := "NEEDS_REVIEW"
	if p.Confidence >= graph.AutoAcceptThresholdForTier(int(tier)) {
		reviewState = "AUTO_ACCEPTED"
	}

	// Cycle guard: applyEdgeAttached is otherwise the only edge-creation
	// path in the codebase without one (graph.Engine has it at the
	// candidate-merge step, the manual-edge HTTP route has it too). The
	// schema only blocks a direct self-loop; a longer cycle is Go-side
	// only. Parent=source, child=target -- verified against both existing
	// callers (graph.Engine and the manual-edge route agree on that
	// mapping).
	wouldCycle, err := q.WouldCreateCycle(ctx, sqlcgen.WouldCreateCycleParams{
		ParentNodeID: sourceID,
		ChildNodeID:  targetID,
	})
	if err != nil {
		return fmt.Errorf("cycle check %d->%d: %w", sourceID, targetID, err)
	}
	if wouldCycle {
		return fmt.Errorf("%w: %d->%d (%s)", ErrWouldCreateCycle, sourceID, targetID, p.RelationshipType)
	}

	// Create edge. CreateMediaEdge has no ON CONFLICT by design: a UNIQUE
	// violation below (already-exists) is swallowed as idempotent success
	// rather than upserted, which is what preserves the human
	// CONFIRMED/REJECTED invariant here -- an agent event can never
	// overwrite a human-reviewed edge's confidence/evidence, only no-op
	// against it. UpsertMediaEdge's ON CONFLICT ... WHERE review_state NOT
	// IN (...) exists for exactly that guarantee; this reaches the same
	// place by a different mechanism. A side effect: an agent re-sending
	// the same edge with different evidence never refreshes it.
	_, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
		SourceNodeID:     sourceID,
		TargetNodeID:     targetID,
		RelationshipType: p.RelationshipType,
		Confidence:       p.Confidence,
		Tier:             tier,
		Resolver:         resolver,
		EvidenceJson:     evidenceJSON,
		ReviewState:      reviewState,
	})
	if err != nil {
		// SQLite UNIQUE constraint violation -> already exists, treat as idempotent success.
		if !isDuplicateEdgeError(err) {
			return fmt.Errorf("insert media edge: %w", err)
		}
	}

	// graph_status must be recomputed from ALL of the target's live parent
	// edges, not just this one -- see RecomputeStatusFromPersistedEdges's
	// doc comment. Runs on both the fresh-insert and idempotent-duplicate
	// paths so a target left UNLINKED by a pre-fix run of this code self-heals
	// on the next retry of the same event.
	return graph.RecomputeStatusFromPersistedEdges(ctx, q, targetID)
}

func (d *Drainer) applyNodeMoved(ctx context.Context, q *sqlcgen.Queries, ev sqlcgen.ListPendingAgentEventsRow) error {
	var p NodeMovedPayload
	if err := json.Unmarshal([]byte(ev.PayloadJson), &p); err != nil {
		return fmt.Errorf("%w: unmarshal node moved: %v", ErrMalformedPayload, err)
	}
	if p.NodeUUID == "" {
		return fmt.Errorf("%w: missing nodeUuid in node moved payload", ErrInvalidNodeUUID)
	}
	if p.NewFilePath == "" {
		return fmt.Errorf("%w: missing newFilePath in node moved payload", ErrMalformedPayload)
	}

	node, err := q.GetMediaNodeByUUID(ctx, p.NodeUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: node uuid %s", ErrNodeNotFound, p.NodeUUID)
		}
		return fmt.Errorf("lookup node for move: %w", err)
	}
	// An ARCHIVED node is a superseded version. RebaseNodePathByUUID sets
	// lifecycle_state='ACTIVE' unconditionally with no lifecycle filter of
	// its own -- rebasing here would resurrect it, silently if the target
	// path happens to be free (loudly, via a CHECK/unique-index failure,
	// otherwise). Refuse before that happens.
	if node.LifecycleState == "ARCHIVED" {
		return fmt.Errorf("%w: node uuid %s", ErrArchivedNode, p.NodeUUID)
	}

	// storage_location_id always comes from Guard.Resolve(NewFilePath),
	// never from the payload or the node's prior location -- see
	// NodeCreatedPayload.StorageLocationID for why a payload-supplied ID is
	// unverifiable. Resolve failure and a nil guard are both refused rather
	// than silently defaulted, matching handleAgentRebase's HTTP behavior.
	if d.guard == nil {
		return fmt.Errorf("agent: storage guard not configured, deferring node move for %q", p.NewFilePath)
	}
	loc, err := resolveRebaseTarget(d.guard, p.NewFilePath)
	if err != nil {
		return err
	}
	locID := loc.ID

	fileName := p.NewFileName
	if fileName == "" {
		fileName = filepath.Base(p.NewFilePath)
	}
	mtime := p.MtimeUnix
	if mtime == 0 {
		mtime = node.MtimeUnix
	}

	return q.RebaseNodePathByUUID(ctx, sqlcgen.RebaseNodePathByUUIDParams{
		NodeUuid:          p.NodeUUID,
		FilePath:          p.NewFilePath,
		FileName:          fileName,
		StorageLocationID: locID,
		MtimeUnix:         mtime,
	})
}

func (d *Drainer) applyNodeDeleted(ctx context.Context, q *sqlcgen.Queries, ev sqlcgen.ListPendingAgentEventsRow) error {
	var p NodeDeletedPayload
	if err := json.Unmarshal([]byte(ev.PayloadJson), &p); err != nil {
		return fmt.Errorf("%w: unmarshal node deleted: %v", ErrMalformedPayload, err)
	}
	if p.NodeUUID == "" {
		return fmt.Errorf("%w: missing nodeUuid in node deleted payload", ErrInvalidNodeUUID)
	}

	node, err := q.GetMediaNodeByUUID(ctx, p.NodeUUID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: node uuid %s", ErrNodeNotFound, p.NodeUUID)
		}
		return fmt.Errorf("lookup node for deletion: %w", err)
	}

	// Schema fix #6 / Spec Pillar 5: never delete row from database; set lifecycle_state='MISSING'.
	return q.MarkNodeMissing(ctx, node.ID)
}

func (d *Drainer) applyPathRebased(ctx context.Context, q *sqlcgen.Queries, ev sqlcgen.ListPendingAgentEventsRow) error {
	var p PathRebasedPayload
	if err := json.Unmarshal([]byte(ev.PayloadJson), &p); err != nil {
		return fmt.Errorf("%w: unmarshal path rebased: %v", ErrMalformedPayload, err)
	}
	if p.NodeUUID == "" {
		return fmt.Errorf("%w: missing nodeUuid in path rebased payload", ErrInvalidNodeUUID)
	}
	if p.TargetFilePath == "" {
		return fmt.Errorf("%w: missing targetFilePath in path rebased payload", ErrMalformedPayload)
	}

	// storage_location_id always comes from Guard.Resolve(TargetFilePath),
	// never from the payload -- see NodeCreatedPayload.StorageLocationID.
	// Resolve failure and a nil guard are both refused, matching
	// handleAgentRebase's HTTP behavior; there is no locID==0 fallback to a
	// magic default location.
	if d.guard == nil {
		return fmt.Errorf("agent: storage guard not configured, deferring path rebase for %q", p.TargetFilePath)
	}
	loc, err := resolveRebaseTarget(d.guard, p.TargetFilePath)
	if err != nil {
		return err
	}
	locID := loc.ID

	fileName := p.TargetFileName
	if fileName == "" {
		fileName = filepath.Base(p.TargetFilePath)
	}
	mtime := p.MtimeUnix
	if mtime == 0 {
		mtime = time.Now().Unix()
	}

	existing, err := q.GetMediaNodeByUUID(ctx, p.NodeUUID)
	if err == nil {
		// Existing node: rebase path in place. Refuse an ARCHIVED node for
		// the same reason as applyNodeMoved -- RebaseNodePathByUUID would
		// resurrect a superseded version.
		if existing.LifecycleState == "ARCHIVED" {
			return fmt.Errorf("%w: node uuid %s", ErrArchivedNode, p.NodeUUID)
		}
		return q.RebaseNodePathByUUID(ctx, sqlcgen.RebaseNodePathByUUIDParams{
			NodeUuid:          p.NodeUUID,
			FilePath:          p.TargetFilePath,
			FileName:          fileName,
			StorageLocationID: locID,
			MtimeUnix:         mtime,
		})
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("lookup node for rebase: %w", err)
	}

	// Unknown node_uuid: agent is the source of truth for an offline staged file; create node!
	fileExt := filepath.Ext(p.TargetFilePath)
	indexingStatus := "INDEXED_SHALLOW"

	_, insertErr := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
		NodeUuid:           p.NodeUUID,
		StorageLocationID:  locID,
		FilePath:           p.TargetFilePath,
		FileName:           fileName,
		FileExt:            fileExt,
		SizeBytes:          p.SizeBytes,
		MtimeUnix:          mtime,
		FastHash:           p.FastHash,
		FullHash:           nil,
		Phash:              sql.NullInt64{},
		IndexingStatus:     indexingStatus,
		GraphStatus:        "UNLINKED",
		LifecycleState:     "ACTIVE",
		OriginalDocumentID: sql.NullString{},
		DocumentID:         sql.NullString{},
		DerivedFromID:      sql.NullString{},
		CapturedAtUnix:     sql.NullInt64{},
		CameraModel:        sql.NullString{},
		FilenameStem:       sql.NullString{},
		CameraSerial:       sql.NullString{},
		LensModel:          sql.NullString{},
	})
	return insertErr
}

// resolveRebaseTarget resolves path via guard and applies the Tier-3
// exemption decided in issue #167: a target resolving to a writable
// location is allowed unconditionally; a target resolving to a read-only
// tier is refused *unless* it is specifically TIER3_MASTER_ARCHIVE and the
// file already exists there (checked via guard.Exists, a pure stat -- see
// its doc comment for why this never performs a write against the
// archive). This is the spec §9-required "LOCAL_STAGING -> CENTRAL_TIER3"
// scenario: once the workstation agent has copied the bytes into the
// archive itself, the server may record that location, but branchDAM's own
// code still never writes, renames, or deletes a Tier-3 file. Any other
// read-only tier has no such exemption and stays hard-refused.
func resolveRebaseTarget(guard *storage.Guard, path string) (storage.Location, error) {
	loc, err := guard.Resolve(path)
	if err != nil {
		return storage.Location{}, fmt.Errorf("%w: rebase target %q does not resolve to any known storage location: %v", ErrMalformedPayload, path, err)
	}
	if !loc.ReadOnly && loc.Tier != "TIER3_MASTER_ARCHIVE" {
		return loc, nil
	}
	if loc.Tier != "TIER3_MASTER_ARCHIVE" {
		return storage.Location{}, fmt.Errorf("%w: rebase target %q resolves to read-only tier %s", ErrReadOnlyRebase, path, loc.Tier)
	}
	exists, err := guard.Exists(path)
	if err != nil {
		return storage.Location{}, fmt.Errorf("%w: checking whether rebase target %q already exists: %v", ErrMalformedPayload, path, err)
	}
	if !exists {
		return storage.Location{}, fmt.Errorf("%w: rebase target %q resolves to read-only tier %s and no file exists there yet -- the bytes must already be copied into the archive before the server can rebase the record", ErrArchiveFileNotYetPresent, path, loc.Tier)
	}
	return loc, nil
}

func isDuplicateEdgeError(err error) bool {
	if err == nil {
		return false
	}
	// Scoped to media_edges specifically -- a bare "UNIQUE constraint
	// failed" match here would swallow any unique violation this function
	// might ever see, not just an already-exists edge.
	return strings.Contains(err.Error(), "UNIQUE constraint failed: media_edges")
}
