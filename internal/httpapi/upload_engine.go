package httpapi

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/naming"
	"github.com/s3ntin3l8/branchdam/internal/pipeline"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

// DefaultMaxUploadSizeBytes defines the default upper limit for uploaded files (50 GiB).
const DefaultMaxUploadSizeBytes int64 = 50 * 1024 * 1024 * 1024

func (s *Server) removeFile(path string) error {
	if path == "" {
		return nil
	}
	if s.guard != nil {
		err := s.guard.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		var unknownErr *storage.ErrUnknownLocation
		if errors.As(err, &unknownErr) {
			rmErr := os.Remove(path)
			if rmErr != nil && (errors.Is(rmErr, os.ErrNotExist) || errors.Is(rmErr, fs.ErrNotExist)) {
				return nil
			}
			return rmErr
		}
		return err
	}
	err := os.Remove(path)
	if err != nil && (errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist)) {
		return nil
	}
	return err
}

func (s *Server) mkdirAll(path string, perm os.FileMode) error {
	if s.guard != nil {
		err := s.guard.MkdirAll(path, perm)
		if err == nil {
			return nil
		}
		var unknownErr *storage.ErrUnknownLocation
		if errors.As(err, &unknownErr) {
			return os.MkdirAll(path, perm)
		}
		return err
	}
	return os.MkdirAll(path, perm)
}

// UploadParams contains parameters for ingesting an uploaded file stream.
type UploadParams struct {
	Filename            string
	Body                io.Reader
	StorageLocationID   int64  // 0 means auto-select (prefer writable TIER3_MASTER_ARCHIVE)
	RelativePath        string // custom relative subpath if not using naming template
	ApplyNamingTemplate bool
	CameraModel         string
	CapturedAtUnix      int64
	ExpectedBlake3      string
	SourcePathHash      string
	MaxBytes            int64 // Maximum allowed bytes (<= 0 defaults to DefaultMaxUploadSizeBytes)
}

// UploadResult contains the result of a successful upload.
type UploadResult struct {
	NodeID       int64
	NodeUUID     string
	FilePath     string
	FileName     string
	FileExt      string
	SizeBytes    int64
	FastHash     *string
	FullHash     *string
	Blake3Hash   string
	RelativePath string
	IsDedup      bool
	Asset        assetDTO
}

// resolveTargetStorageLocation finds the target storage location for uploads.
func (s *Server) resolveTargetStorageLocation(ctx context.Context, locationID int64) (*storage.StorageLocationRow, *storage.StorageLocationRow, error) {
	if s.db == nil {
		return nil, nil, ErrDatabaseUnavailable
	}

	if locationID < 0 {
		return nil, nil, fmt.Errorf("%w: location id %d cannot be negative", ErrInvalidStorageLocation, locationID)
	}

	locs, err := s.db.Reader.ListStorageLocations(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("list storage locations: %w", err)
	}

	var targetLoc *storage.StorageLocationRow
	var exportLoc *storage.StorageLocationRow
	var foundSpecified bool

	for _, loc := range locs {
		locCopy := loc
		row := storage.StorageLocationRow{
			ID:       locCopy.ID,
			Name:     locCopy.Name,
			RootPath: locCopy.RootPath,
			Tier:     locCopy.Tier,
			ReadOnly: locCopy.ReadOnly != 0,
		}
		if locCopy.Tier == "TIER2_EXPORTS" && locCopy.ReadOnly == 0 && locCopy.IsActive != 0 {
			exp := row
			exportLoc = &exp
		}
		if locationID > 0 && locCopy.ID == locationID {
			foundSpecified = true
			if locCopy.IsActive == 0 {
				return nil, nil, fmt.Errorf("%w: location %q (id %d) is inactive", ErrStorageLocationInactive, locCopy.Name, locationID)
			}
			if locCopy.ReadOnly != 0 {
				return nil, nil, &StorageLocationReadOnlyError{Location: locCopy.Name, Tier: locCopy.Tier}
			}
			tgt := row
			targetLoc = &tgt
		} else if locationID == 0 && locCopy.ReadOnly == 0 && locCopy.IsActive != 0 {
			if targetLoc == nil || locCopy.Tier == "TIER3_MASTER_ARCHIVE" {
				tgt := row
				targetLoc = &tgt
			}
		}
	}

	if locationID > 0 && !foundSpecified {
		return nil, nil, fmt.Errorf("%w: location with id %d not found", ErrStorageLocationNotFound, locationID)
	}

	if targetLoc == nil {
		return nil, nil, ErrNoWritableLocation
	}
	if targetLoc.ReadOnly {
		return nil, nil, &StorageLocationReadOnlyError{Location: targetLoc.Name, Tier: targetLoc.Tier}
	}

	return targetLoc, exportLoc, nil
}

// dedupUploadResult constructs a unified UploadResult and assetDTO for deduplicated upload responses.
// bytesWritten represents the bytes transferred to disk during this specific request (0 for pre-write hits,
// whereas Asset.SizeBytes always reflects the existing media node's stored size).
func dedupUploadResult(existing sqlcgen.GetMediaNodeByFullHashRow, blake3Hash string, bytesWritten int64) *UploadResult {
	return &UploadResult{
		NodeID:       existing.ID,
		NodeUUID:     existing.NodeUuid,
		FilePath:     existing.FilePath,
		FileName:     filepath.Base(existing.FilePath),
		FileExt:      filepath.Ext(existing.FilePath),
		SizeBytes:    bytesWritten,
		Blake3Hash:   blake3Hash,
		RelativePath: existing.FilePath,
		IsDedup:      true,
		Asset: assetDTO{
			ID:             existing.ID,
			NodeUUID:       existing.NodeUuid,
			FilePath:       existing.FilePath,
			FileName:       filepath.Base(existing.FilePath),
			FileExt:        filepath.Ext(existing.FilePath),
			SizeBytes:      existing.SizeBytes,
			LifecycleState: existing.LifecycleState,
			IndexingStatus: existing.IndexingStatus,
		},
	}
}

// processUploadedStream is the unified ingest pipeline for agent and web uploads.
// It handles storage location resolution, naming, streaming disk writes, Blake3 checksumming,
// inserts the media node and metadata in DB, links lineage, and broadcasts SSE.
func (s *Server) processUploadedStream(ctx context.Context, params UploadParams) (*UploadResult, error) {
	// Pre-write dedup check: trust-the-agent optimization for authenticated clients
	// providing a trusted X-Blake3-Hash pre-flight header. If a live node already has
	// this hash, skip writing bytes to disk entirely and return HTTP 200 + DEDUPLICATED.
	// For streaming uploads without pre-flight headers, BLAKE3 is computed unconditionally
	// during streaming and deduplicated post-write.
	if params.ExpectedBlake3 != "" && s.db != nil {
		cleanHash := strings.ToLower(strings.TrimSpace(params.ExpectedBlake3))
		if existing, err := s.db.Reader.GetMediaNodeByFullHash(ctx, &cleanHash); err == nil {
			return dedupUploadResult(existing, cleanHash, 0), nil
		}
	}

	targetLoc, exportLoc, err := s.resolveTargetStorageLocation(ctx, params.StorageLocationID)
	if err != nil {
		return nil, err
	}

	rawFilename := strings.TrimSpace(params.Filename)
	if strings.Contains(rawFilename, "/") || strings.Contains(rawFilename, "\\") || strings.Contains(rawFilename, "..") {
		return nil, ErrInvalidFilename
	}
	cleanFilename := filepath.Base(filepath.Clean(rawFilename))
	if cleanFilename == "" || cleanFilename == "." || cleanFilename == "/" || cleanFilename == "\\" {
		cleanFilename = "upload_" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".bin"
	}
	if strings.Contains(cleanFilename, "/") || strings.Contains(cleanFilename, "\\") || strings.Contains(cleanFilename, "..") {
		return nil, ErrInvalidFilename
	}

	cameraModel := strings.TrimSpace(params.CameraModel)
	if cameraModel == "" {
		cameraModel = "unknown_camera"
	}
	if strings.Contains(cameraModel, "/") || strings.Contains(cameraModel, "\\") || strings.Contains(cameraModel, "..") {
		return nil, ErrInvalidCameraModel
	}

	capturedAtUnix := params.CapturedAtUnix
	if capturedAtUnix == 0 {
		capturedAtUnix = time.Now().Unix()
	}
	capturedAt := time.Unix(capturedAtUnix, 0).UTC()

	var relPath string
	if params.ApplyNamingTemplate {
		namingTpl := naming.DefaultPathTemplate
		if cfg := s.cfg(); cfg != nil && cfg.Ingest.NamingTemplate != "" {
			namingTpl = cfg.Ingest.NamingTemplate
		}
		relPath = naming.RenderPath(namingTpl, naming.TemplateVars{
			CapturedAt:   capturedAt,
			CameraModel:  cameraModel,
			OriginalName: cleanFilename,
		})
	} else if params.RelativePath != "" {
		cleanedRel := filepath.Clean(params.RelativePath)
		cleanedRel = strings.TrimPrefix(cleanedRel, string(filepath.Separator))
		cleanedRel = strings.TrimPrefix(cleanedRel, "/")
		cleanedRel = strings.TrimPrefix(cleanedRel, "\\")
		if !filepath.IsLocal(cleanedRel) {
			return nil, fmt.Errorf("%w: cannot escape root", ErrInvalidPath)
		}
		if filepath.Base(cleanedRel) == cleanFilename {
			relPath = cleanedRel
		} else {
			relPath = filepath.Join(cleanedRel, cleanFilename)
		}
	} else {
		relPath = cleanFilename
	}

	cleanRel := filepath.Clean(relPath)
	if !filepath.IsLocal(cleanRel) {
		return nil, fmt.Errorf("%w: invalid rendered path", ErrInvalidPath)
	}

	safeArchiveDir, err := filepath.Abs(targetLoc.RootPath)
	if err != nil {
		return nil, fmt.Errorf("failed to resolve archive root: %w", err)
	}

	targetPath := filepath.Join(safeArchiveDir, cleanRel)
	rel, err := filepath.Rel(safeArchiveDir, targetPath)
	if err != nil || !filepath.IsLocal(rel) {
		return nil, ErrPathEscapes
	}
	targetPath = filepath.Join(safeArchiveDir, rel)

	if err := s.mkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		return nil, fmt.Errorf("failed to create directory: %w", err)
	}

	targetFileName := filepath.Base(targetPath)
	ext := filepath.Ext(targetFileName)
	stem := strings.TrimSuffix(targetFileName, ext)
	collisionCount := 0

	var outFile *os.File
	for {
		f, createErr := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
		if createErr == nil {
			outFile = f
			break
		}
		if !errors.Is(createErr, os.ErrExist) {
			return nil, fmt.Errorf("failed to create target file: %w", createErr)
		}
		collisionCount++
		suffixedName := fmt.Sprintf("%s_%d%s", stem, collisionCount, ext)
		nextCleanRel := filepath.Clean(filepath.Join(filepath.Dir(cleanRel), suffixedName))
		if !filepath.IsLocal(nextCleanRel) {
			return nil, ErrPathEscapes
		}
		nextTargetPath := filepath.Join(safeArchiveDir, nextCleanRel)
		nextRel, relErr := filepath.Rel(safeArchiveDir, nextTargetPath)
		if relErr != nil || !filepath.IsLocal(nextRel) {
			return nil, ErrPathEscapes
		}
		cleanRel = nextCleanRel
		targetPath = filepath.Join(safeArchiveDir, nextRel)
		targetFileName = suffixedName
	}

	nodeID, err := uuid.NewV7()
	if err != nil {
		_ = outFile.Close()
		_ = s.removeFile(targetPath)
		return nil, fmt.Errorf("failed to mint node uuid: %w", err)
	}
	nodeUUIDStr := nodeID.String()

	hasher := blake3.New()
	multiWriter := io.MultiWriter(outFile, hasher)

	maxAllowed := params.MaxBytes
	if maxAllowed <= 0 {
		maxAllowed = DefaultMaxUploadSizeBytes
	}

	limitReader := io.LimitReader(params.Body, maxAllowed+1)
	bytesWritten, err := io.Copy(multiWriter, limitReader)
	if err != nil {
		_ = outFile.Close()
		_ = s.removeFile(targetPath)
		return nil, fmt.Errorf("failed to write stream: %w", err)
	}
	if bytesWritten > maxAllowed {
		_ = outFile.Close()
		_ = s.removeFile(targetPath)
		return nil, ErrFileTooLarge
	}

	computedBlake3 := hex.EncodeToString(hasher.Sum(nil))
	expectedBlake3 := strings.ToLower(strings.TrimSpace(params.ExpectedBlake3))
	if expectedBlake3 != "" && computedBlake3 != expectedBlake3 {
		_ = outFile.Close()
		_ = s.removeFile(targetPath)
		return nil, &ChecksumMismatchError{Expected: expectedBlake3, Got: computedBlake3}
	}

	var nullFast *string
	if computedFastHash, err := hashing.FastHash(outFile, bytesWritten); err == nil && computedFastHash != "" {
		nullFast = &computedFastHash
	}
	_ = outFile.Close()

	if s.db != nil {
		if existing, err := s.db.Reader.GetMediaNodeByFullHash(ctx, &computedBlake3); err == nil {
			_ = s.removeFile(targetPath)
			return dedupUploadResult(existing, computedBlake3, bytesWritten), nil
		}
	}

	// Probe metadata if prober is present
	var (
		originalDocID sql.NullString
		docID         sql.NullString
		derivedFromID sql.NullString
		cameraModelDB sql.NullString
		cameraSerial  sql.NullString
		lensModel     sql.NullString
		pHashVal      sql.NullInt64
		exifResult    *probe.ExifResult
		ffprobeResult *probe.FFProbeResult
	)

	if s.prober != nil && s.prober.HasExiftool() {
		probeCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if ex, err := s.prober.Exif(probeCtx, targetPath); err == nil && ex != nil {
			exifResult = ex
			if ex.OriginalDocumentID != "" {
				originalDocID = sql.NullString{String: ex.OriginalDocumentID, Valid: true}
			}
			if ex.DocumentID != "" {
				docID = sql.NullString{String: ex.DocumentID, Valid: true}
			}
			if ex.DerivedFromID != "" {
				derivedFromID = sql.NullString{String: ex.DerivedFromID, Valid: true}
			}
			if ex.Model != "" {
				cameraModelDB = sql.NullString{String: ex.Model, Valid: true}
			}
			if ex.SerialNumber != "" {
				cameraSerial = sql.NullString{String: ex.SerialNumber, Valid: true}
			}
			if ex.LensModel != "" {
				lensModel = sql.NullString{String: ex.LensModel, Valid: true}
			}
			if ex.CapturedAt != nil && !ex.CapturedAt.IsZero() {
				capturedAtUnix = ex.CapturedAt.Unix()
			}
		}
		cancel()
	}

	if !cameraModelDB.Valid && cameraModel != "unknown_camera" {
		cameraModelDB = sql.NullString{String: cameraModel, Valid: true}
	}

	cleanExt := strings.TrimPrefix(strings.ToLower(ext), ".")
	if s.prober != nil && isImageExtension(cleanExt) {
		phCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if ph, err := s.prober.ExtractPHash(phCtx, targetPath); err == nil && ph != nil {
			pHashVal = sql.NullInt64{Int64: *ph, Valid: true}
		}
		cancel()
	}

	if s.prober != nil && s.prober.HasFFProbe() && isVideoExtension(cleanExt) {
		ffCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		if ff, err := s.prober.FFProbe(ffCtx, targetPath); err == nil && ff != nil {
			ffprobeResult = ff
		}
		cancel()
	}

	// Hardlink non-RAW standalone photos into Tier 2 Immich exports
	var (
		exportCreated bool
		exportDest    string
		exportUUIDStr string
	)
	if exportLoc != nil && isStandaloneDisplayable(ext) {
		safeExportDir, err := filepath.Abs(exportLoc.RootPath)
		if err != nil {
			if s.log != nil {
				s.log.Warn("failed to resolve export root directory", "err", err)
			}
		} else {
			exportRel := filepath.Clean(filepath.Join("immich", cleanRel))
			if filepath.IsLocal(exportRel) {
				dest := filepath.Join(safeExportDir, exportRel)
				if rel, err := filepath.Rel(safeExportDir, dest); err == nil && filepath.IsLocal(rel) {
					exportDest = filepath.Join(safeExportDir, rel)
					exportDir := filepath.Dir(exportDest)
					if err := s.mkdirAll(exportDir, 0o755); err != nil {
						if s.log != nil {
							s.log.Warn("failed to create Immich export directory", "err", err)
						}
					} else if _, statErr := os.Lstat(exportDest); statErr == nil {
						// Destination already exists; skip inserting duplicate export node
					} else if err := linkOrCopyFile(targetPath, exportDest); err != nil {
						if s.log != nil {
							s.log.Warn("failed to create Immich export hardlink or copy", "err", err)
						}
					} else {
						exportCreated = true
						if expID, err := uuid.NewV7(); err == nil {
							exportUUIDStr = expID.String()
						}
					}
				}
			}
		}
	}

	var insertedNode sqlcgen.MediaNode
	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		nullFull := &computedBlake3
		var nullSourcePathHash *string
		if params.SourcePathHash != "" {
			h := strings.ToLower(strings.TrimSpace(params.SourcePathHash))
			if hashing.IsValidHex(h, 64) {
				nullSourcePathHash = &h
			}
		}

		archiveNode, insErr := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:           nodeUUIDStr,
			StorageLocationID:  targetLoc.ID,
			FilePath:           targetPath,
			FileName:           targetFileName,
			FileExt:            ext,
			SizeBytes:          bytesWritten,
			MtimeUnix:          time.Now().Unix(),
			FastHash:           nullFast,
			FullHash:           nullFull,
			IndexingStatus:     "INDEXED_SHALLOW",
			GraphStatus:        "UNLINKED",
			LifecycleState:     "ACTIVE",
			FilenameStem:       sql.NullString{String: stem, Valid: stem != ""},
			CapturedAtUnix:     sql.NullInt64{Int64: capturedAtUnix, Valid: capturedAtUnix > 0},
			CameraModel:        cameraModelDB,
			CameraSerial:       cameraSerial,
			LensModel:          lensModel,
			OriginalDocumentID: originalDocID,
			DocumentID:         docID,
			DerivedFromID:      derivedFromID,
			Phash:              pHashVal,
			SourcePathHash:     nullSourcePathHash,
		})
		if insErr != nil {
			return insErr
		}
		insertedNode = archiveNode

		// Persist FFProbe metadata if present
		if ffprobeResult != nil {
			if ffprobeResult.FormatName != "" {
				_ = q.InsertNodeMetadata(ctx, sqlcgen.InsertNodeMetadataParams{
					NodeID: archiveNode.ID, Source: "ffprobe", Key: "format_name", Value: ffprobeResult.FormatName,
				})
			}
			if ffprobeResult.VideoCodec != "" {
				_ = q.InsertNodeMetadata(ctx, sqlcgen.InsertNodeMetadataParams{
					NodeID: archiveNode.ID, Source: "ffprobe", Key: "video_codec", Value: ffprobeResult.VideoCodec,
				})
			}
			if ffprobeResult.AudioCodec != "" {
				_ = q.InsertNodeMetadata(ctx, sqlcgen.InsertNodeMetadataParams{
					NodeID: archiveNode.ID, Source: "ffprobe", Key: "audio_codec", Value: ffprobeResult.AudioCodec,
				})
			}
		}

		if exportCreated && exportLoc != nil && exportUUIDStr != "" {
			exportNode, expInsErr := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
				NodeUuid:          exportUUIDStr,
				StorageLocationID: exportLoc.ID,
				FilePath:          exportDest,
				FileName:          targetFileName,
				FileExt:           ext,
				SizeBytes:         bytesWritten,
				MtimeUnix:         time.Now().Unix(),
				FastHash:          nullFast,
				// FullHash is left nil for export hardlinks/copies so they do not conflict
				// with the ux_media_nodes_live_full_hash unique index on the master archive node.
				FullHash:       nil,
				IndexingStatus: "INDEXED_SHALLOW",
				GraphStatus:    "LINKED",
				LifecycleState: "ACTIVE",
				FilenameStem:   sql.NullString{String: stem, Valid: stem != ""},
				CapturedAtUnix: sql.NullInt64{Int64: capturedAtUnix, Valid: capturedAtUnix > 0},
				CameraModel:    cameraModelDB,
			})
			if expInsErr != nil {
				return fmt.Errorf("insert export media node: %w", expInsErr)
			}
			if _, edgeErr := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
				SourceNodeID:     archiveNode.ID,
				TargetNodeID:     exportNode.ID,
				RelationshipType: "FINAL_EXPORT",
				Confidence:       1.0,
				Tier:             1,
				Resolver:         "immich_export",
				EvidenceJson:     `{"reason":"immich_hardlink_export"}`,
				ReviewState:      "AUTO_ACCEPTED",
			}); edgeErr != nil {
				return fmt.Errorf("create export edge: %w", edgeErr)
			}
		}
		return nil
	})

	if err != nil {
		_ = s.removeFile(targetPath)
		if exportCreated && exportDest != "" {
			_ = s.removeFile(exportDest)
		}
		if s.db != nil && computedBlake3 != "" {
			if existing, lookupErr := s.db.Reader.GetMediaNodeByFullHash(ctx, &computedBlake3); lookupErr == nil {
				return dedupUploadResult(existing, computedBlake3, bytesWritten), nil
			}
		}
		return nil, fmt.Errorf("failed to record media node: %w", err)
	}

	// Persist EXIF metadata outside the main transaction to avoid nested InTx on single writer connection
	if exifResult != nil {
		_ = pipeline.PersistExifMetadata(ctx, s.db, insertedNode.ID, exifResult, s.log)
	}

	if s.engine != nil {
		node, err := s.db.Reader.GetMediaNodeByUUID(ctx, nodeUUIDStr)
		if err == nil {
			_, _, _ = s.engine.ResolveAndCommit(ctx, graph.ToNode(node))
		}
	}

	if s.hub != nil {
		s.hub.Broadcast()
	}

	return &UploadResult{
		NodeID:       insertedNode.ID,
		NodeUUID:     nodeUUIDStr,
		FilePath:     targetPath,
		FileName:     targetFileName,
		FileExt:      ext,
		SizeBytes:    bytesWritten,
		FastHash:     nullFast,
		FullHash:     &computedBlake3,
		Blake3Hash:   computedBlake3,
		RelativePath: cleanRel,
		Asset:        toAssetDTO(insertedNode),
	}, nil
}

func isStandaloneDisplayable(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".heic", ".png", ".webp", ".mp4", ".mov":
		return true
	default:
		return false
	}
}

func linkOrCopyFile(src, dst string) error {
	cleanSrc := filepath.Clean(src)
	cleanDst := filepath.Clean(dst)

	linkErr := os.Link(cleanSrc, cleanDst)
	if linkErr == nil {
		return nil
	}
	if errors.Is(linkErr, os.ErrExist) {
		return linkErr
	}

	// Fallback to safe non-truncating copy on cross-device link failure
	srcFile, err := os.Open(cleanSrc)
	if err != nil {
		return fmt.Errorf("link error: %w, open src error: %v", linkErr, err)
	}
	defer func() { _ = srcFile.Close() }()

	dstFile, err := os.OpenFile(cleanDst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("link error: %w, create dst error: %v", linkErr, err)
	}
	defer func() { _ = dstFile.Close() }()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		_ = os.Remove(cleanDst)
		return fmt.Errorf("link error: %w, copy error: %v", linkErr, err)
	}
	return nil
}

func isImageExtension(ext string) bool {
	switch strings.ToLower(ext) {
	case "jpg", "jpeg", "png", "webp", "heic", "tif", "tiff", "cr2", "cr3", "nef", "arw", "dng":
		return true
	default:
		return false
	}
}

func isVideoExtension(ext string) bool {
	switch strings.ToLower(ext) {
	case "mp4", "mov", "mkv", "avi", "m4v", "webm", "mts", "m2ts":
		return true
	default:
		return false
	}
}
