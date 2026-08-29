package httpapi

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/zeebo/blake3"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/naming"
	"github.com/s3ntin3l8/branchdam/internal/storage"
)

type AgentUploadResponse struct {
	NodeUUID     string `json:"nodeUuid"`
	Status       string `json:"status"`
	BytesWritten int64  `json:"bytesWritten"`
	Blake3Hash   string `json:"blake3Hash"`
	RelativePath string `json:"relativePath,omitempty"`
}

func (s *Server) writeJSONError(w http.ResponseWriter, statusCode int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": message})
}

func (s *Server) handleAgentUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok || (p.Kind != auth.KindMachine && !p.Authenticated) {
		s.writeJSONError(w, http.StatusForbidden, "authentication required")
		return
	}

	// Select target storage location: prefer writable TIER3_MASTER_ARCHIVE, otherwise first writable tier
	var targetLoc *storage.StorageLocationRow
	var exportLoc *storage.StorageLocationRow
	if s.db != nil {
		locs, err := s.db.Reader.ListStorageLocations(r.Context())
		if err == nil {
			for _, loc := range locs {
				row := storage.StorageLocationRow{
					ID:       loc.ID,
					Name:     loc.Name,
					RootPath: loc.RootPath,
					Tier:     loc.Tier,
					ReadOnly: loc.ReadOnly != 0,
				}
				if loc.Tier == "TIER2_EXPORTS" && loc.ReadOnly == 0 {
					exportLoc = &row
				}
				if loc.ReadOnly == 0 {
					if targetLoc == nil || loc.Tier == "TIER3_MASTER_ARCHIVE" {
						targetLoc = &row
					}
				}
			}
		}
	}

	if targetLoc == nil {
		s.writeJSONError(w, http.StatusServiceUnavailable, "no writable storage location configured for upload")
		return
	}

	filename := r.Header.Get("X-Filename")
	if filename == "" {
		filename = "upload_" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".bin"
	}
	if strings.Contains(filename, "/") || strings.Contains(filename, "\\") || strings.Contains(filename, "..") {
		s.writeJSONError(w, http.StatusBadRequest, "invalid filename")
		return
	}
	filename = filepath.Base(filepath.Clean(filename))

	cameraModel := r.Header.Get("X-Camera-Model")
	if cameraModel == "" {
		cameraModel = "unknown_camera"
	}
	if strings.Contains(cameraModel, "/") || strings.Contains(cameraModel, "\\") || strings.Contains(cameraModel, "..") {
		s.writeJSONError(w, http.StatusBadRequest, "invalid camera model")
		return
	}

	expectedBlake3 := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Blake3-Hash")))
	capturedAtHeader := r.Header.Get("X-Capture-Timestamp")
	var capturedAtUnix int64
	if capturedAtHeader != "" {
		capturedAtUnix, _ = strconv.ParseInt(capturedAtHeader, 10, 64)
	}
	if capturedAtUnix == 0 {
		capturedAtUnix = time.Now().Unix()
	}

	capturedAt := time.Unix(capturedAtUnix, 0).UTC()
	namingTpl := naming.DefaultPathTemplate
	if cfg := s.cfg(); cfg != nil && cfg.Ingest.NamingTemplate != "" {
		namingTpl = cfg.Ingest.NamingTemplate
	}

	relPath := naming.RenderPath(namingTpl, naming.TemplateVars{
		CapturedAt:   capturedAt,
		CameraModel:  cameraModel,
		OriginalName: filename,
	})
	relPath = filepath.Clean(relPath)
	if strings.HasPrefix(relPath, "..") || filepath.IsAbs(relPath) {
		s.writeJSONError(w, http.StatusBadRequest, "invalid rendered path")
		return
	}

	safeArchiveDir, err := filepath.Abs(targetLoc.RootPath)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, "failed to resolve archive root")
		return
	}
	if !strings.HasSuffix(safeArchiveDir, string(filepath.Separator)) {
		safeArchiveDir += string(filepath.Separator)
	}

	targetPath, err := filepath.Abs(filepath.Join(safeArchiveDir, relPath))
	if err != nil || !strings.HasPrefix(targetPath, safeArchiveDir) {
		s.writeJSONError(w, http.StatusBadRequest, "path escapes storage location")
		return
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create directory: %v", err))
		return
	}

	// Handle filename collisions by atomically attempting O_CREATE|O_EXCL.
	// Using O_EXCL eliminates the TOCTOU gap between a stat-based check and a
	// subsequent Create: whichever concurrent upload wins the exclusive create
	// owns that path; the loser retries with the next collision suffix.
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	targetFileName := filepath.Base(targetPath)
	collisionCount := 0

	var outFile *os.File
	for {
		f, createErr := os.OpenFile(targetPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o644)
		if createErr == nil {
			outFile = f
			break
		}
		if !errors.Is(createErr, os.ErrExist) {
			s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create target file: %v", createErr))
			return
		}
		// Path already exists — pick the next collision suffix and retry.
		collisionCount++
		suffixedName := fmt.Sprintf("%s_%d%s", stem, collisionCount, ext)
		relPath = filepath.Join(filepath.Dir(relPath), suffixedName)
		targetPath, err = filepath.Abs(filepath.Join(safeArchiveDir, relPath))
		if err != nil || !strings.HasPrefix(targetPath, safeArchiveDir) {
			s.writeJSONError(w, http.StatusBadRequest, "path escapes storage location")
			return
		}
		targetFileName = suffixedName
	}

	nodeID, err := uuid.NewV7()
	if err != nil {
		_ = outFile.Close()
		_ = os.Remove(targetPath)
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to mint node uuid: %v", err))
		return
	}
	nodeUUIDStr := nodeID.String()

	hasher := blake3.New()
	multiWriter := io.MultiWriter(outFile, hasher)

	bytesWritten, err := io.Copy(multiWriter, r.Body)
	if err != nil {
		_ = outFile.Close()
		_ = os.Remove(targetPath)
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to write stream: %v", err))
		return
	}

	computedBlake3 := hex.EncodeToString(hasher.Sum(nil))
	if expectedBlake3 != "" && computedBlake3 != expectedBlake3 {
		_ = outFile.Close()
		_ = os.Remove(targetPath)
		s.writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("checksum mismatch: expected %s, got %s", expectedBlake3, computedBlake3))
		return
	}

	var nullFast *string
	if computedFastHash, err := hashing.FastHash(outFile, bytesWritten); err == nil && computedFastHash != "" {
		nullFast = &computedFastHash
	}
	_ = outFile.Close()

	// Hardlink non-RAW standalone photos into Tier 2 Immich exports for zero-storage duplication
	var (
		exportCreated bool
		exportDest    string
		exportUUIDStr string
	)
	if exportLoc != nil && isStandaloneDisplayable(ext) {
		safeExportDir, err := filepath.Abs(exportLoc.RootPath)
		if err == nil {
			if !strings.HasSuffix(safeExportDir, string(filepath.Separator)) {
				safeExportDir += string(filepath.Separator)
			}
			dest, err := filepath.Abs(filepath.Join(safeExportDir, "immich", relPath))
			if err == nil && strings.HasPrefix(dest, safeExportDir) {
				exportDest = dest
				exportDir := filepath.Dir(exportDest)
				if err := os.MkdirAll(exportDir, 0o755); err != nil {
					if s.log != nil {
						s.log.Warn("failed to create Immich export directory", "err", err)
					}
				} else if err := linkOrCopyFile(targetPath, exportDest); err != nil {
					if s.log != nil {
						s.log.Warn("failed to create Immich export file", "err", err)
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

	// Register media node in database
	if s.db != nil {
		err = s.db.InTx(r.Context(), func(q *sqlcgen.Queries) error {
			nullFull := &computedBlake3

			archiveNode, insErr := q.InsertMediaNode(r.Context(), sqlcgen.InsertMediaNodeParams{
				NodeUuid:          nodeUUIDStr,
				StorageLocationID: targetLoc.ID,
				FilePath:          targetPath,
				FileName:          targetFileName,
				FileExt:           ext,
				SizeBytes:         bytesWritten,
				MtimeUnix:         time.Now().Unix(),
				FastHash:          nullFast,
				FullHash:          nullFull,
				IndexingStatus:    "INDEXED_SHALLOW",
				GraphStatus:       "UNLINKED",
				LifecycleState:    "ACTIVE",
				FilenameStem:      sql.NullString{String: stem, Valid: stem != ""},
				CapturedAtUnix:    sql.NullInt64{Int64: capturedAtUnix, Valid: capturedAtUnix > 0},
			})
			if insErr != nil {
				return insErr
			}

			if exportCreated && exportLoc != nil && exportUUIDStr != "" {
				exportNode, expInsErr := q.InsertMediaNode(r.Context(), sqlcgen.InsertMediaNodeParams{
					NodeUuid:          exportUUIDStr,
					StorageLocationID: exportLoc.ID,
					FilePath:          exportDest,
					FileName:          targetFileName,
					FileExt:           ext,
					SizeBytes:         bytesWritten,
					MtimeUnix:         time.Now().Unix(),
					FastHash:          nullFast,
					FullHash:          nullFull,
					IndexingStatus:    "INDEXED_SHALLOW",
					GraphStatus:       "LINKED",
					LifecycleState:    "ACTIVE",
					FilenameStem:      sql.NullString{String: stem, Valid: stem != ""},
					CapturedAtUnix:    sql.NullInt64{Int64: capturedAtUnix, Valid: capturedAtUnix > 0},
				})
				if expInsErr != nil {
					// Roll back the whole transaction so neither the archive node
					// nor the export node land in the DB; the caller's err != nil
					// cleanup block removes both on-disk files.
					return fmt.Errorf("insert export media node: %w", expInsErr)
				}
				if _, edgeErr := q.CreateMediaEdge(r.Context(), sqlcgen.CreateMediaEdgeParams{
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
			_ = os.Remove(targetPath)
			if exportCreated && exportDest != "" {
				_ = os.Remove(exportDest)
			}
			s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to record media node: %v", err))
			return
		}

		if s.engine != nil {
			node, err := s.db.Reader.GetMediaNodeByUUID(r.Context(), nodeUUIDStr)
			if err == nil {
				_, _, _ = s.engine.ResolveAndCommit(r.Context(), graph.ToNode(node))
			}
		}

		if s.hub != nil {
			s.hub.Broadcast()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(AgentUploadResponse{
		NodeUUID:     nodeUUIDStr,
		Status:       "UPLOADED",
		BytesWritten: bytesWritten,
		Blake3Hash:   computedBlake3,
		RelativePath: relPath,
	})
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
	linkErr := os.Link(src, dst)
	if linkErr == nil || errors.Is(linkErr, os.ErrExist) {
		return nil
	}

	// Fallback to safe non-truncating copy on cross-device link failure
	srcFile, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("link error: %w, open src error: %v", linkErr, err)
	}
	defer func() { _ = srcFile.Close() }()

	dstFile, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			return nil
		}
		return fmt.Errorf("link error: %w, create dst error: %v", linkErr, err)
	}
	defer func() { _ = dstFile.Close() }()

	if _, err := io.Copy(dstFile, srcFile); err != nil {
		_ = os.Remove(dst)
		return fmt.Errorf("link error: %w, copy error: %v", linkErr, err)
	}
	return nil
}
