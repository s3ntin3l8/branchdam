package agent

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// DefaultDrainInterval is how often Start polls event_queue for newly
// PENDING rows between passes, when the caller doesn't override it. DrainAll
// itself drains the queue to empty and returns; nothing keeps polling once
// it does, until the next tick -- an idle tick costs one indexed COUNT
// query (CountPendingAgentEvents), so a short default is cheap.
const DefaultDrainInterval = 2 * time.Second

// Drainer processes PENDING rows in event_queue oldest-first and applies
// the corresponding state changes to media_nodes and media_edges.
type Drainer struct {
	db          *db.DB
	guard       *storage.Guard
	log         *slog.Logger
	maxRetries  int
	backoffWait time.Duration
	mu          sync.Mutex

	// nudge, if set, is called once after a drain pass (ProcessPending)
	// commits at least one event, mirroring pipeline.ScanDeps.Nudge --
	// without it a connected SPA never learns an agent event changed
	// anything. engine, if set, resolves lineage edges for every node an
	// agent event freshly inserts (EVENT_NODE_CREATED, and
	// EVENT_PATH_REBASED's unknown-node-uuid branch), the same join point
	// pipeline.resolveEdgesForBatch is for scanned nodes -- without it an
	// agent-created node sits graph_status='UNLINKED' forever. Both nil by
	// default (existing NewDrainer call sites, mostly tests, are
	// unaffected); set via WithNudge/WithEngine.
	nudge         func()
	engine        *graph.Engine
	immichScanner ImmichScanner

	wg        sync.WaitGroup
	startOnce sync.Once
}

// ImmichScanner defines the client interface for triggering external library scans.
type ImmichScanner interface {
	TriggerScan(ctx context.Context) error
}

// DrainerOption configures optional Drainer behavior not every caller
// needs -- see NewDrainer.
type DrainerOption func(*Drainer)

// WithNudge sets the callback ProcessPending calls after a pass commits at
// least one event. Typically hub.Broadcast (internal/sse).
func WithNudge(nudge func()) DrainerOption {
	return func(d *Drainer) { d.nudge = nudge }
}

// WithEngine sets the graph.Engine used to resolve lineage edges for nodes
// an agent event freshly inserts.
func WithEngine(engine *graph.Engine) DrainerOption {
	return func(d *Drainer) { d.engine = engine }
}

// WithImmichScanner sets the ImmichScanner client used to trigger library rescans on asset deletion.
func WithImmichScanner(scanner ImmichScanner) DrainerOption {
	return func(d *Drainer) { d.immichScanner = scanner }
}

// NewDrainer creates an agent event queue drainer. guard and log may be
// nil (log defaults to discarding); opts are typically WithNudge and/or
// WithEngine in production, omitted in tests that only need
// ProcessPending/DrainAll's own return value.
func NewDrainer(database *db.DB, guard *storage.Guard, log *slog.Logger, opts ...DrainerOption) *Drainer {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	d := &Drainer{
		db:         database,
		guard:      guard,
		log:        log,
		maxRetries: DefaultMaxRetries,
	}
	for _, opt := range opts {
		opt(d)
	}
	return d
}

// Start begins the background drain loop and returns immediately: a
// ticker fires DrainAll every interval (DefaultDrainInterval if <= 0) until
// ctx is cancelled. A second call is a no-op and logs a warning, mirroring
// pipeline.WatcherSupervisor.Start/SweeperSupervisor.Start. Unlike a sweep
// pass, a drain pass needs no context.WithoutCancel escape hatch to shut
// down cleanly: DrainAll processes one event per transaction and checks
// ctx.Err() between each, so cancellation lands it between events rather
// than aborting one mid-write.
func (d *Drainer) Start(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultDrainInterval
	}
	started := false
	d.startOnce.Do(func() {
		started = true
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.run(ctx, interval)
		}()
	})
	if !started {
		d.log.Warn("agent: Drainer.Start called after the first start; ignoring")
	}
}

// Wait blocks until the background drain loop started by Start has
// returned -- called during shutdown, after ctx is cancelled, before
// pool.Drain and db.Close (see main.go), mirroring
// WatcherSupervisor.Wait/SweeperSupervisor.Wait. A no-op if Start was never
// called.
func (d *Drainer) Wait() { d.wg.Wait() }

func (d *Drainer) run(ctx context.Context, interval time.Duration) {
	// An immediate pass before the ticker's first tick, so an event
	// enqueued right before/during startup isn't left PENDING for up to a
	// full interval before it's first considered. Suggested by Hermes on
	// #189.
	if _, err := d.DrainAll(ctx); err != nil && ctx.Err() == nil {
		d.log.Warn("agent: drain pass failed", "err", err)
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if _, err := d.DrainAll(ctx); err != nil && ctx.Err() == nil {
				d.log.Warn("agent: drain pass failed", "err", err)
			}
		}
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

		var resolveNodeID int64
		processErr := d.db.InTx(ctx, func(q *sqlcgen.Queries) error {
			id, err := d.applyEvent(ctx, q, ev)
			if err != nil {
				return err
			}
			resolveNodeID = id
			return q.MarkAgentEventProcessed(ctx, ev.ID)
		})

		if processErr == nil {
			stats.Processed++
			// Resolution runs in its own transaction, after the event's
			// commits -- graph.Engine.ResolveAndCommit opens its own
			// db.InTx internally and this repo's writer pool is a single
			// connection (docs/schema.md fix #7), so it cannot run nested
			// inside the InTx above. Mirrors
			// pipeline.resolveEdgesForBatch's same after-commit call.
			// Failure here is logged, not fatal to the event: the create/
			// rebase itself already succeeded and committed. Unlike a
			// scanned node (which gets a fresh ResolveAndCommit attempt on
			// every subsequent scan pass that touches it), there is
			// currently no retry path for this one-shot call -- a failure
			// here leaves the node UNLINKED with nothing to re-trigger
			// resolution. Flagged by Hermes on #189; still a strict
			// improvement over never resolving at all, which is the
			// pre-#166 behavior this replaces.
			if resolveNodeID != 0 && d.engine != nil {
				node, err := d.db.Reader.GetMediaNodeByID(ctx, resolveNodeID)
				if err != nil {
					d.log.Warn("agent: re-fetch agent-created node for resolution", "nodeID", resolveNodeID, "err", err)
				} else if _, _, err := d.engine.ResolveAndCommit(ctx, graph.ToNode(node)); err != nil {
					d.log.Warn("agent: resolve edges for agent-created node", "nodeID", resolveNodeID, "err", err)
				}
			}
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

	// Nudge once per batch, not per event -- sse.Hub.Broadcast is already a
	// coalescing signal (internal/sse's own doc comment), so per-event
	// calls would be harmless but wasteful. Only fires when the batch
	// actually changed visible state (Processed > 0): a FAILED event
	// changes nothing in media_nodes/media_edges, so nudging on
	// failures-only would trigger a client refetch for no reason.
	if d.nudge != nil && stats.Processed > 0 {
		d.nudge()
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

// applyEvent applies ev and returns the ID of a media_nodes row it freshly
// inserted, if any -- 0 if the event didn't insert one (an idempotent
// no-op, an edge/move/delete, or a rebase of an already-known node). The
// caller (ProcessPending) uses that ID to resolve lineage edges through
// graph.Engine after this transaction commits.
func (d *Drainer) applyEvent(ctx context.Context, q *sqlcgen.Queries, ev sqlcgen.ListPendingAgentEventsRow) (int64, error) {
	switch ev.EventType {
	case EventNodeCreated:
		return d.applyNodeCreated(ctx, q, ev)
	case EventEdgeAttached:
		return 0, d.applyEdgeAttached(ctx, q, ev)
	case EventNodeMoved:
		return 0, d.applyNodeMoved(ctx, q, ev)
	case EventNodeDeleted:
		return 0, d.applyNodeDeleted(ctx, q, ev)
	case EventPathRebased:
		return d.applyPathRebased(ctx, q, ev)
	default:
		return 0, fmt.Errorf("%w: %s", ErrUnknownEventType, ev.EventType)
	}
}

func (d *Drainer) applyNodeCreated(ctx context.Context, q *sqlcgen.Queries, ev sqlcgen.ListPendingAgentEventsRow) (int64, error) {
	var p NodeCreatedPayload
	if err := json.Unmarshal([]byte(ev.PayloadJson), &p); err != nil {
		return 0, fmt.Errorf("%w: unmarshal node created: %v", ErrMalformedPayload, err)
	}
	if p.NodeUUID == "" {
		return 0, fmt.Errorf("%w: missing nodeUuid in node created payload", ErrInvalidNodeUUID)
	}
	if p.FilePath == "" {
		return 0, fmt.Errorf("%w: missing filePath in node created payload", ErrMalformedPayload)
	}

	// Idempotency: if node already exists with this node_uuid, it's a no-op success.
	if _, err := q.GetMediaNodeByUUID(ctx, p.NodeUUID); err == nil {
		return 0, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("lookup node by uuid: %w", err)
	}

	// Strict content dedup: if a live (non-ARCHIVED) node already has this full_hash,
	// skip insertion and succeed without creating a duplicate media node.
	if p.FullHash != nil && *p.FullHash != "" {
		if existing, err := q.GetMediaNodeByFullHash(ctx, p.FullHash); err == nil {
			d.log.Info("agent: content dedup — EVENT_NODE_CREATED skipped",
				"incomingUuid", p.NodeUUID,
				"existingUuid", existing.NodeUuid,
				"existingPath", existing.FilePath,
				"fullHash", *p.FullHash)
			return 0, nil
		} else if !errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("lookup node by full_hash: %w", err)
		}
	}

	// storage_location_id always comes from Guard.Resolve(FilePath), never
	// from the payload -- storage.Guard exposes no lookup-by-ID, so a
	// payload-supplied StorageLocationID is fundamentally unverifiable and
	// is ignored (see NodeCreatedPayload.StorageLocationID). A nil guard is
	// a server misconfiguration, not a poison-pill payload, so it's a
	// plain (non-fatal) error: the event stays PENDING/retries rather than
	// being marked FAILED for a problem the payload didn't cause.
	if d.guard == nil {
		return 0, fmt.Errorf("agent: storage guard not configured, deferring node create for %q", p.FilePath)
	}
	loc, err := d.guard.Resolve(p.FilePath)
	if err != nil {
		return 0, fmt.Errorf("%w: file path %q does not resolve to any known storage location: %v", ErrMalformedPayload, p.FilePath, err)
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

	var nullSourcePathHash *string
	if p.SourcePathHash != nil && *p.SourcePathHash != "" {
		h := strings.ToLower(strings.TrimSpace(*p.SourcePathHash))
		if len(h) == 64 {
			nullSourcePathHash = &h
		}
	}

	mtime := p.MtimeUnix
	if mtime == 0 {
		mtime = time.Now().Unix()
	}

	inserted, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
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
		SourcePathHash:     nullSourcePathHash,
	})
	if err != nil {
		if p.FullHash != nil && *p.FullHash != "" {
			if existing, lookupErr := q.GetMediaNodeByFullHash(ctx, p.FullHash); lookupErr == nil {
				d.log.Info("agent: content dedup race — winner already inserted",
					"incomingUuid", p.NodeUUID,
					"existingUuid", existing.NodeUuid,
					"existingPath", existing.FilePath)
				return 0, nil
			}
		}
		return 0, err
	}

	// GPS is deliberately not a promoted media_nodes column (see
	// docs/schema.md's promoted-column list) -- it lands in node_metadata
	// the same way a normal scan's exiftool probe would (#229). Written
	// inside the same enclosing transaction as InsertMediaNode (ProcessPending
	// wraps applyEvent in one d.db.InTx call), so an insert failure here rolls
	// back the node too rather than leaving it without its GPS point.
	if err := writeGPSMetadata(ctx, q, inserted.ID, p); err != nil {
		return 0, err
	}

	return inserted.ID, nil
}

// writeGPSMetadata upserts node_metadata rows for p's GPS point, if any,
// under source="exiftool" using exactly the same key format and value
// encoding internal/pipeline/commit.go's exifMetadata writes during a
// normal scan (Composite:GPSLatitude/Composite:GPSLongitude, signed decimal
// degrees via strconv.FormatFloat(_, 'f', -1, 64)) -- so a downstream reader
// like httpapi's loadTagSet, which filters strictly on source=="exiftool",
// sees an agent-ingested node's GPS point exactly the way it would see a
// scanned one, even though no exiftool process ever ran on this path.
// Reuses node_metadata.sql's existing InsertNodeMetadata upsert (ON
// CONFLICT (node_id, source, key) DO UPDATE) rather than a new query.
func writeGPSMetadata(ctx context.Context, q *sqlcgen.Queries, nodeID int64, p NodeCreatedPayload) error {
	if p.GPSLatitude != nil {
		if err := q.InsertNodeMetadata(ctx, sqlcgen.InsertNodeMetadataParams{
			NodeID: nodeID,
			Source: "exiftool",
			Key:    "Composite:GPSLatitude",
			Value:  strconv.FormatFloat(*p.GPSLatitude, 'f', -1, 64),
		}); err != nil {
			return fmt.Errorf("insert node_metadata Composite:GPSLatitude: %w", err)
		}
	}
	if p.GPSLongitude != nil {
		if err := q.InsertNodeMetadata(ctx, sqlcgen.InsertNodeMetadataParams{
			NodeID: nodeID,
			Source: "exiftool",
			Key:    "Composite:GPSLongitude",
			Value:  strconv.FormatFloat(*p.GPSLongitude, 'f', -1, 64),
		}); err != nil {
			return fmt.Errorf("insert node_metadata Composite:GPSLongitude: %w", err)
		}
	}
	return nil
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
	if err := q.MarkNodeMissing(ctx, node.ID); err != nil {
		return err
	}

	// Purge remote sync state for this deleted node
	if err := q.DeleteRemoteSyncStateForNode(ctx, node.ID); err != nil {
		d.log.Warn("agent: delete remote sync state for node", "nodeID", node.ID, "err", err)
	}

	// Move physical master file to .trash/<rel_path> buffer inside storage location
	if d.guard != nil && node.FilePath != "" {
		if loc, err := d.guard.Resolve(node.FilePath); err == nil {
			if relPath, err := filepath.Rel(loc.RootPath, node.FilePath); err == nil && !strings.HasPrefix(relPath, "..") {
				trashPath := filepath.Join(loc.RootPath, ".trash", relPath)
				// Guard against overwriting pre-existing trashed files if the same relPath is trashed again
				trashExt := filepath.Ext(trashPath)
				trashStem := strings.TrimSuffix(trashPath, trashExt)
				uniqueTrashPath := trashPath
				collisionIdx := 0
				for {
					if _, err := os.Stat(uniqueTrashPath); os.IsNotExist(err) {
						break
					}
					collisionIdx++
					uniqueTrashPath = fmt.Sprintf("%s_%d%s", trashStem, collisionIdx, trashExt)
				}
				trashPath = uniqueTrashPath

				now := time.Now().UTC()
				if err := os.MkdirAll(filepath.Dir(trashPath), 0o755); err != nil {
					d.log.Warn("agent: failed to create trash directory", "err", err)
				} else if _, statErr := os.Stat(node.FilePath); statErr == nil {
					if err := os.Rename(node.FilePath, trashPath); err != nil {
						if copyErr := moveFile(node.FilePath, trashPath); copyErr != nil {
							d.log.Warn("agent: failed to move file to trash", "err", copyErr)
						} else if chErr := os.Chtimes(trashPath, now, now); chErr != nil {
							d.log.Warn("agent: failed to stamp trash mtime", "err", chErr)
						}
					} else if chErr := os.Chtimes(trashPath, now, now); chErr != nil {
						d.log.Warn("agent: failed to stamp trash mtime", "err", chErr)
					}
				}
			}
		}
	}

	// Purge any Tier 2 Immich exports linked to this node (vanishes from gallery immediately)
	edges, err := q.ListEdgesBySource(ctx, node.ID)
	if err == nil {
		for _, edge := range edges {
			if edge.RelationshipType == "FINAL_EXPORT" || edge.Resolver == "immich_export" {
				expNode, expErr := q.GetMediaNodeByID(ctx, edge.TargetNodeID)
				if expErr == nil {
					if rmErr := os.Remove(expNode.FilePath); rmErr != nil && !os.IsNotExist(rmErr) {
						d.log.Warn("agent: failed to unlink immich export file", "nodeID", expNode.ID, "err", rmErr)
					} else {
						if markErr := q.MarkNodeMissing(ctx, expNode.ID); markErr != nil {
							d.log.Warn("agent: failed to mark export node missing", "nodeID", expNode.ID, "err", markErr)
						}
						if delErr := q.DeleteRemoteSyncStateForNode(ctx, expNode.ID); delErr != nil {
							d.log.Warn("agent: failed to delete remote sync state for export node", "nodeID", expNode.ID, "err", delErr)
						}
					}
				}
			}
		}
	}

	// If Immich client is wired, trigger external library rescan
	if d.immichScanner != nil {
		if scanErr := d.immichScanner.TriggerScan(ctx); scanErr != nil {
			d.log.Warn("agent: trigger immich scan after node deletion", "nodeID", node.ID, "err", scanErr)
		}
	}

	return nil
}

func moveFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	if _, err := io.Copy(out, in); err != nil {
		_ = os.Remove(dst)
		return err
	}
	_ = in.Close()
	_ = out.Close()
	return os.Remove(src)
}

// applyPathRebased returns the freshly inserted node's ID when NodeUUID was
// unknown (see applyEvent's doc comment) -- 0 when it rebased an existing
// node instead.
func (d *Drainer) applyPathRebased(ctx context.Context, q *sqlcgen.Queries, ev sqlcgen.ListPendingAgentEventsRow) (int64, error) {
	var p PathRebasedPayload
	if err := json.Unmarshal([]byte(ev.PayloadJson), &p); err != nil {
		return 0, fmt.Errorf("%w: unmarshal path rebased: %v", ErrMalformedPayload, err)
	}
	if p.NodeUUID == "" {
		return 0, fmt.Errorf("%w: missing nodeUuid in path rebased payload", ErrInvalidNodeUUID)
	}
	if p.TargetFilePath == "" {
		return 0, fmt.Errorf("%w: missing targetFilePath in path rebased payload", ErrMalformedPayload)
	}

	// storage_location_id always comes from Guard.Resolve(TargetFilePath),
	// never from the payload -- see NodeCreatedPayload.StorageLocationID.
	// Resolve failure and a nil guard are both refused, matching
	// handleAgentRebase's HTTP behavior; there is no locID==0 fallback to a
	// magic default location.
	if d.guard == nil {
		return 0, fmt.Errorf("agent: storage guard not configured, deferring path rebase for %q", p.TargetFilePath)
	}
	loc, err := resolveRebaseTarget(d.guard, p.TargetFilePath)
	if err != nil {
		return 0, err
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
			return 0, fmt.Errorf("%w: node uuid %s", ErrArchivedNode, p.NodeUUID)
		}
		return 0, q.RebaseNodePathByUUID(ctx, sqlcgen.RebaseNodePathByUUIDParams{
			NodeUuid:          p.NodeUUID,
			FilePath:          p.TargetFilePath,
			FileName:          fileName,
			StorageLocationID: locID,
			MtimeUnix:         mtime,
		})
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("lookup node for rebase: %w", err)
	}

	// Unknown node_uuid: agent is the source of truth for an offline staged file; create node!
	fileExt := filepath.Ext(p.TargetFilePath)
	indexingStatus := "INDEXED_SHALLOW"

	inserted, insertErr := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
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
	if insertErr != nil {
		return 0, insertErr
	}
	return inserted.ID, nil
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
