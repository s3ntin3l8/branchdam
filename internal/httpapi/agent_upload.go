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
)

type AgentUploadResponse struct {
	NodeUUID     string `json:"nodeUuid"`
	Status       string `json:"status"`
	BytesWritten int64  `json:"bytesWritten"`
	Blake3Hash   string `json:"blake3Hash"`
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

	// Verify writable storage location before creating files
	var stagingBase string
	var locID int64
	if s.db != nil {
		locs, err := s.db.Reader.ListStorageLocations(r.Context())
		if err == nil {
			for _, loc := range locs {
				if loc.ReadOnly == 0 {
					stagingBase = loc.RootPath
					locID = loc.ID
					break
				}
			}
		}
	}

	if locID == 0 || stagingBase == "" {
		s.writeJSONError(w, http.StatusServiceUnavailable, "no writable storage location configured for staging")
		return
	}

	filename := r.Header.Get("X-Filename")
	if filename == "" {
		filename = "upload_" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".bin"
	}
	filename = filepath.Base(filename)

	expectedBlake3 := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Blake3-Hash")))
	capturedAtHeader := r.Header.Get("X-Capture-Timestamp")
	var capturedAtUnix int64
	if capturedAtHeader != "" {
		capturedAtUnix, _ = strconv.ParseInt(capturedAtHeader, 10, 64)
	}
	if capturedAtUnix == 0 {
		capturedAtUnix = time.Now().Unix()
	}

	stagingDir := filepath.Join(stagingBase, "_staging", "mobile", time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to create staging directory: %v", err))
		return
	}

	nodeID, err := uuid.NewV7()
	if err != nil {
		s.writeJSONError(w, http.StatusInternalServerError, fmt.Sprintf("failed to mint node uuid: %v", err))
		return
	}
	nodeUUIDStr := nodeID.String()

	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	targetFileName := fmt.Sprintf("%s_%s%s", stem, nodeUUIDStr[:8], ext)
	targetPath := filepath.Join(stagingDir, targetFileName)

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
				StorageLocationID: locID,
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
	})
}
