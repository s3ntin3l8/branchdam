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

type StagingUploadResponse struct {
	NodeUUID     string `json:"nodeUuid"`
	Status       string `json:"status"`
	BytesWritten int64  `json:"bytesWritten"`
	Blake3Hash   string `json:"blake3Hash"`
}

func (s *Server) handleStagingUpload(w http.ResponseWriter, r *http.Request) {
	p, ok := auth.From(r.Context())
	if !ok || (p.Kind != auth.KindMachine && !p.Authenticated) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprintln(w, `{"error":"authentication required"}`)
		return
	}

	filename := r.Header.Get("X-Filename")
	if filename == "" {
		filename = "upload_" + strconv.FormatInt(time.Now().UnixNano(), 10) + ".bin"
	}
	filename = filepath.Base(filename)

	expectedBlake3 := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Blake3-Hash")))
	expectedFastHash := strings.TrimSpace(r.Header.Get("X-Fast-Hash"))
	capturedAtHeader := r.Header.Get("X-Capture-Timestamp")
	var capturedAtUnix int64
	if capturedAtHeader != "" {
		capturedAtUnix, _ = strconv.ParseInt(capturedAtHeader, 10, 64)
	}
	if capturedAtUnix == 0 {
		capturedAtUnix = time.Now().Unix()
	}

	// Determine staging base path and location ID
	stagingBase := os.TempDir()
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

	stagingDir := filepath.Join(stagingBase, "_staging", "mobile", time.Now().Format("2006-01-02"))
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"error":"failed to create staging directory: %v"}`+"\n", err)
		return
	}

	nodeID, err := uuid.NewV7()
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"error":"failed to mint node uuid: %v"}`+"\n", err)
		return
	}
	nodeUUIDStr := nodeID.String()

	ext := filepath.Ext(filename)
	stem := strings.TrimSuffix(filename, ext)
	targetFileName := fmt.Sprintf("%s_%s%s", stem, nodeUUIDStr[:8], ext)
	targetPath := filepath.Join(stagingDir, targetFileName)

	outFile, err := os.Create(targetPath)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"error":"failed to create target file: %v"}`+"\n", err)
		return
	}

	hasher := blake3.New()
	multiWriter := io.MultiWriter(outFile, hasher)

	bytesWritten, err := io.Copy(multiWriter, r.Body)
	if err != nil {
		_ = outFile.Close()
		_ = os.Remove(targetPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = fmt.Fprintf(w, `{"error":"failed to write stream: %v"}`+"\n", err)
		return
	}

	computedBlake3 := hex.EncodeToString(hasher.Sum(nil))
	if expectedBlake3 != "" && computedBlake3 != expectedBlake3 {
		_ = outFile.Close()
		_ = os.Remove(targetPath)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = fmt.Fprintf(w, `{"error":"checksum mismatch: expected %s, got %s"}`+"\n", expectedBlake3, computedBlake3)
		return
	}

	computedFastHash, _ := hashing.FastHash(outFile, bytesWritten)
	_ = outFile.Close()
	if computedFastHash == "" {
		computedFastHash = expectedFastHash
	}

	// Register media node in database
	if s.db != nil && locID > 0 {
		err = s.db.InTx(r.Context(), func(q *sqlcgen.Queries) error {
			var nullFast *string
			if computedFastHash != "" {
				nullFast = &computedFastHash
			}
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
				IndexingStatus:    "INDEXED_FULL",
				GraphStatus:       "UNLINKED",
				LifecycleState:    "ACTIVE",
				FilenameStem:      sql.NullString{String: stem, Valid: stem != ""},
				CapturedAtUnix:    sql.NullInt64{Int64: capturedAtUnix, Valid: capturedAtUnix > 0},
			})
			return insErr
		})

		if err != nil {
			_ = os.Remove(targetPath)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = fmt.Fprintf(w, `{"error":"failed to record media node: %v"}`+"\n", err)
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
	_ = json.NewEncoder(w).Encode(StagingUploadResponse{
		NodeUUID:     nodeUUIDStr,
		Status:       "STAGED",
		BytesWritten: bytesWritten,
		Blake3Hash:   computedBlake3,
	})
}
