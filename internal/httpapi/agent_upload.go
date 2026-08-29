package httpapi

import (
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
	filename = filepath.Base(filename)

	cameraModel := r.Header.Get("X-Camera-Model")
	if cameraModel == "" {
		cameraModel = "unknown_camera"
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

	targetPath := filepath.Join(targetLoc.RootPath, relPath)
	if !strings.HasPrefix(filepath.Clean(targetPath), filepath.Clean(targetLoc.RootPath)) {
		s.writeJSONError(w, http.StatusBadRequest, "path escapes storage location")
		return
	}

	if err := os.MkdirAll(filepath.Dir(targetPath), 0o755); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create directory: %v", err))
		return
	}

	// Handle filename collisions by auto-suffixing
	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	targetFileName := filepath.Base(targetPath)
	collisionCount := 1
	for {
		if _, err := os.Stat(targetPath); errorsIsNotExist(err) {
			break
		}
		collisionCount++
		suffixedName := fmt.Sprintf("%s_%d%s", stem, collisionCount, ext)
		relPath = filepath.Join(filepath.Dir(relPath), suffixedName)
		targetPath = filepath.Join(targetLoc.RootPath, relPath)
		targetFileName = suffixedName
	}

	nodeID, err := uuid.NewV7()
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to mint node uuid: %v", err))
		return
	}
	nodeUUIDStr := nodeID.String()

	outFile, err := os.Create(targetPath)
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create target file: %v", err))
		return
	}

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

	// Register media node in database
	if s.db != nil {
		err = s.db.InTx(r.Context(), func(q *sqlcgen.Queries) error {
			nullFull := &computedBlake3

			_, insErr := q.InsertMediaNode(r.Context(), sqlcgen.InsertMediaNodeParams{
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
			return insErr
		})

		if err != nil {
			_ = os.Remove(targetPath)
			s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to record media node: %v", err))
			return
		}

		// Hardlink non-RAW standalone photos into Tier 2 Immich exports for zero-storage duplication
		if exportLoc != nil && isStandaloneDisplayable(ext) {
			exportDest := filepath.Join(exportLoc.RootPath, "immich", relPath)
			if strings.HasPrefix(filepath.Clean(exportDest), filepath.Clean(exportLoc.RootPath)) {
				_ = os.MkdirAll(filepath.Dir(exportDest), 0o755)
				if linkErr := os.Link(targetPath, exportDest); linkErr != nil {
					// Fallback to copy if on different physical filesystem (EXDEV)
					_ = copyFileContents(targetPath, exportDest)
				}
			}
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

func errorsIsNotExist(err error) bool {
	return os.IsNotExist(err)
}

func isStandaloneDisplayable(ext string) bool {
	switch strings.ToLower(ext) {
	case ".jpg", ".jpeg", ".heic", ".png", ".webp", ".mp4", ".mov":
		return true
	default:
		return false
	}
}

func copyFileContents(src, dst string) error {
	cleanSrc := filepath.Clean(src)
	cleanDst := filepath.Clean(dst)

	in, err := os.Open(cleanSrc)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.Create(cleanDst)
	if err != nil {
		return err
	}
	defer func() { _ = out.Close() }()

	_, err = io.Copy(out, in)
	return err
}
