package httpapi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/graph"
	"github.com/s3ntin3l8/branchdam/internal/hashing"
	"github.com/s3ntin3l8/branchdam/internal/probe"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	"github.com/s3ntin3l8/branchdam/internal/storage"
	"github.com/s3ntin3l8/branchdam/internal/sync"
	"github.com/s3ntin3l8/branchdam/internal/workers"
)

const routeTestAgentKey = "01234567890123456789012345678901" // 33 chars

// fullTestServer builds a Server with a live worker pool and a real,
// running goroutine set -- unlike testServer(t) in server_test.go, which
// deliberately skips Pool.Run since the four basic tests never submit
// work. Route tests that trigger a scan need the pool actually consuming
// jobs.
func fullTestServer(t *testing.T) (*Server, *db.DB) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "routes.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	})

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	pool := workers.New[string](2, 16)
	pool.Run(ctx)

	srv := New(Deps{
		Config: &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:     database, Prober: probe.New(), Pool: pool,
		Engine: graph.NewEngine(database, nil), Hub: sse.New(),
		Version: "test",
	})
	return srv, database
}

// doJSON simulates the happy-path browser request: a real deployment always
// sits behind Traefik's ForwardAuth, which asserts X-Authentik-Username
// (see docs/forward-auth.md), so every request that reaches this server has
// one. #164: since RequireAdmin now fails closed on a mutating route when
// that header is absent (Principal.Authenticated), a doJSON call carrying
// no headers at all would no longer represent normal browser traffic.
// Tests that specifically exercise absent-header or non-admin behavior
// (TestMutatingRoutesAuthorization, TestOpenAPIEndpointExposure,
// TestSyncRetryRequiresAdmin) build their requests directly instead of
// through doJSON, so they are unaffected by this default.
func doJSON(t *testing.T, h http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal request body: %v", err)
		}
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, path, reader)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Authentik-Username", "doJSON-test-user")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// inheritTestServer builds a Server whose Guard covers rootPath and seeds a
// Tier-2 location + parent/child nodes + a parent edge.
func inheritTestServer(t *testing.T, rootPath string) (*Server, *db.DB, sqlcgen.MediaNode, sqlcgen.MediaNode) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "inherit.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	resolved, err := filepath.EvalSymlinks(rootPath)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	var locID int64
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "inherit-t2", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		locID = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}
	guard := storage.NewGuard([]storage.Location{{ID: locID, Name: "inherit-t2", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: false}})

	srv := New(Deps{
		Config: &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:     database, Prober: probe.New(), Guard: guard,
		Engine: graph.NewEngine(database, nil), Hub: sse.New(), Version: "test",
	})

	parent := seedInheritNode(t, database, locID, filepath.Join(resolved, "parent.jpg"), "uuid-parent")
	child := seedInheritNode(t, database, locID, filepath.Join(resolved, "child.jpg"), "uuid-child")
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		_, err := q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: parent.ID, TargetNodeID: child.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.9, Tier: 2, Resolver: "test", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed edge: %v", err)
	}
	return srv, database, parent, child
}

func seedInheritNode(t *testing.T, database *db.DB, locID int64, path, uuid string) sqlcgen.MediaNode {
	t.Helper()
	var node sqlcgen.MediaNode
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		n, err := q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: uuid, StorageLocationID: locID, FilePath: path, FileName: filepath.Base(path),
			FileExt: "jpg", SizeBytes: 1, MtimeUnix: time.Now().Unix(),
			FastHash: &[]string{"aaaaaaaaaaaaaaaa"}[0], IndexingStatus: "INDEXED_SHALLOW",
			GraphStatus: "LINKED", LifecycleState: "ACTIVE", FilenameStem: sql.NullString{String: "x", Valid: true},
		})
		node = n
		return err
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}
	return node
}

func TestInheritMetadataConflicts(t *testing.T) {
	t.Run("unknown asset 404s", func(t *testing.T) {
		srv, _, _, _ := inheritTestServer(t, t.TempDir())
		rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/999999/inherit-metadata", nil)
		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rr.Code)
		}
	})

	t.Run("read-only location 409s before exiftool", func(t *testing.T) {
		srv, database, _, child := inheritTestServer(t, t.TempDir())
		roRoot := t.TempDir()
		resolved, _ := filepath.EvalSymlinks(roRoot)
		var roLocID int64
		err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
				Name: "inherit-t3", RootPath: resolved, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: 1, Prunable: 0,
			})
			roLocID = loc.ID
			return err
		})
		if err != nil {
			t.Fatalf("seed ro location: %v", err)
		}
		err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			return q.RebaseMissingNodePath(context.Background(), sqlcgen.RebaseMissingNodePathParams{
				ID: child.ID, FilePath: filepath.Join(resolved, "child.jpg"), FileName: "child.jpg",
				StorageLocationID: roLocID, MtimeUnix: time.Now().Unix(),
			})
		})
		if err != nil {
			t.Fatalf("rebase child: %v", err)
		}
		srv.guard = storage.NewGuard([]storage.Location{{ID: roLocID, Name: "inherit-t3", RootPath: resolved, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true}})

		rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
		if rr.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409", rr.Code)
		}
	})

	t.Run("tier-3-only parent edge 409s", func(t *testing.T) {
		srv, database, _, child := inheritTestServer(t, t.TempDir())
		// Reject the seeded valid Tier-2 edge first, so the only remaining
		// resolved edge is the Tier-3 one seeded below -- pickWinningParent
		// must never fall back to a heuristic match just because nothing
		// better is left.
		var locID int64
		if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			rows, err := q.ListEdgesByTarget(context.Background(), child.ID)
			if err != nil {
				return err
			}
			for _, e := range rows {
				if _, err := q.RejectMediaEdge(context.Background(), sqlcgen.RejectMediaEdgeParams{ID: e.ID, ReviewedBy: sql.NullString{}}); err != nil {
					return err
				}
			}
			locID = child.StorageLocationID
			return nil
		}); err != nil {
			t.Fatalf("reject seeded edge: %v", err)
		}
		t3Parent := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "t3-parent.jpg"), "uuid-t3-parent")
		err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			_, err := q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
				SourceNodeID: t3Parent.ID, TargetNodeID: child.ID, RelationshipType: "DERIVED_FROM",
				Confidence: 0.99, Tier: 3, Resolver: "heuristic", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
			})
			return err
		})
		if err != nil {
			t.Fatalf("seed tier-3 edge: %v", err)
		}

		rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
		if rr.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409, body = %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("DUPLICATE_OF-only parent edge 409s", func(t *testing.T) {
		srv, database, _, child := inheritTestServer(t, t.TempDir())
		// Reject the seeded valid Tier-2 edge, add a DUPLICATE_OF edge as the
		// only remaining resolved candidate -- a duplicate is not ancestry and
		// must never be treated as an identity source.
		var locID int64
		if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			rows, err := q.ListEdgesByTarget(context.Background(), child.ID)
			if err != nil {
				return err
			}
			for _, e := range rows {
				if _, err := q.RejectMediaEdge(context.Background(), sqlcgen.RejectMediaEdgeParams{ID: e.ID, ReviewedBy: sql.NullString{}}); err != nil {
					return err
				}
			}
			locID = child.StorageLocationID
			return nil
		}); err != nil {
			t.Fatalf("reject seeded edge: %v", err)
		}
		dup := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "dup.jpg"), "uuid-dup")
		err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			_, err := q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
				SourceNodeID: dup.ID, TargetNodeID: child.ID, RelationshipType: "DUPLICATE_OF",
				Confidence: 1.0, Tier: 1, Resolver: "manual", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
			})
			return err
		})
		if err != nil {
			t.Fatalf("seed duplicate-of edge: %v", err)
		}

		rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
		if rr.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409, body = %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("ARCHIVED parent 409s", func(t *testing.T) {
		srv, database, parent, child := inheritTestServer(t, t.TempDir())
		if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			return q.ArchiveMediaNode(context.Background(), parent.ID)
		}); err != nil {
			t.Fatalf("archive parent: %v", err)
		}

		rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
		if rr.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409, body = %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("MISSING parent 409s", func(t *testing.T) {
		srv, database, parent, child := inheritTestServer(t, t.TempDir())
		if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			return q.MarkNodeMissing(context.Background(), parent.ID)
		}); err != nil {
			t.Fatalf("mark parent missing: %v", err)
		}

		rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
		if rr.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409, body = %s", rr.Code, rr.Body.String())
		}
	})
	t.Run("needs-review parent edge 409s", func(t *testing.T) {
		srv, database, _, child := inheritTestServer(t, t.TempDir())
		// Reject the seeded AUTO_ACCEPTED edge, then add a NEEDS_REVIEW edge
		// from a second parent -- an unconfirmed lineage must never be used as
		// an identity source.
		var locID int64
		if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			rows, err := q.ListEdgesByTarget(context.Background(), child.ID)
			if err != nil {
				return err
			}
			for _, e := range rows {
				if _, err := q.RejectMediaEdge(context.Background(), sqlcgen.RejectMediaEdgeParams{ID: e.ID, ReviewedBy: sql.NullString{}}); err != nil {
					return err
				}
			}
			locID = child.StorageLocationID
			return nil
		}); err != nil {
			t.Fatalf("reject seeded edge: %v", err)
		}
		nrParent := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "nr-parent.jpg"), "uuid-nr-parent")
		err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			_, err := q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
				SourceNodeID: nrParent.ID, TargetNodeID: child.ID, RelationshipType: "DERIVED_FROM",
				Confidence: 0.8, Tier: 2, Resolver: "test", EvidenceJson: "{}", ReviewState: "NEEDS_REVIEW",
			})
			return err
		})
		if err != nil {
			t.Fatalf("seed needs-review edge: %v", err)
		}

		rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
		if rr.Code != http.StatusConflict {
			t.Errorf("status = %d, want 409", rr.Code)
		}
	})
}

// requireExiftoolAndFFmpeg skips the test when either external binary is
// absent, per internal/probe's convention: this package must still build and
// its non-exiftool tests must still pass without them.
func requireExiftoolAndFFmpeg(t *testing.T) (exiftoolPath string) {
	t.Helper()
	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not installed, skipping")
	}
	_ = ffmpegPath
	exiftoolPath, err = exec.LookPath("exiftool")
	if err != nil {
		t.Skip("exiftool not installed, skipping")
	}
	return exiftoolPath
}

// makeTaggedFixtureJPEG generates a tiny real JPEG via ffmpeg and, if tags is
// non-empty, stamps it with exiftool -- mirroring internal/probe's
// makeFixtureJPEG, kept local to this package rather than shared so the two
// packages' test fixtures can drift independently.
func makeTaggedFixtureJPEG(t *testing.T, exiftoolPath, path string, tags map[string]string) {
	t.Helper()
	ffmpeg := exec.Command("ffmpeg", "-y", "-f", "lavfi", "-i", "color=c=red:s=64x64", "-frames:v", "1", path)
	if out, err := ffmpeg.CombinedOutput(); err != nil {
		t.Fatalf("generate fixture jpeg: %v\n%s", err, out)
	}
	if len(tags) == 0 {
		return
	}
	args := []string{"-overwrite_original"}
	for k, v := range tags {
		args = append(args, "-"+k+"="+v)
	}
	args = append(args, path)
	if out, err := exec.Command(exiftoolPath, args...).CombinedOutput(); err != nil {
		t.Fatalf("tag fixture jpeg: %v\n%s", err, out)
	}
}

// fastHashOf re-hashes a file exactly as internal/pipeline's scan path would,
// so the test can assert the DB's fast_hash actually agrees with the file on
// disk -- the property finding A's fix establishes.
func fastHashOf(t *testing.T, path string) (hash string, size, mtimeUnix int64) {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open for hash: %v", err)
	}
	defer func() { _ = f.Close() }()
	stat, err := f.Stat()
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	hash, err = hashing.FastHash(f, stat.Size())
	if err != nil {
		t.Fatalf("FastHash: %v", err)
	}
	return hash, stat.Size(), stat.ModTime().Unix()
}

// TestInheritMetadataRefusesProjectFileChild backs #199's defense-in-depth
// guard: even when an AUTO_ACCEPTED DERIVED_FROM edge already exists with a
// project file as the child (e.g. created before handleCreateEdge's guard
// shipped, or via a filename-stem/XMP match during automatic resolution),
// handleInheritMetadata must refuse to run exiftool in-place against the
// project archive. The refusal fires before any exiftool call, so this test
// needs neither exiftool nor ffmpeg installed.
func TestInheritMetadataRefusesProjectFileChild(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	var parent, child sqlcgen.MediaNode
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loc", RootPath: t.TempDir(), Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		parent, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-parent", FilePath: "/parent.jpg", FileName: "parent.jpg", FileExt: "jpg", IndexingStatus: "INDEXED_FULL", GraphStatus: "LINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		child, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-child", FilePath: "/edit.dam.json", FileName: "edit.dam.json", FileExt: "dam.json", IndexingStatus: "INDEXED_FULL", GraphStatus: "LINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		_, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: parent.ID, TargetNodeID: child.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 1.0, Tier: 1, Resolver: "manual", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
	if rr.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409, body = %s", rr.Code, rr.Body.String())
	}
	if got := rr.Body.String(); !strings.Contains(got, "project file") {
		t.Errorf("body = %q, want it to mention the project file refusal", got)
	}
}

// TestInheritMetadataRefreshesNodeStateAfterWrite is the happy-path test:
// exiftool actually rewrites the child's file, and the endpoint must update
// media_nodes.size_bytes/mtime_unix/fast_hash to match the rewritten file --
// otherwise the next scan would see a changed fast_hash at an unchanged path
// and treat it as a version collision (finding A).
func TestInheritMetadataRefreshesNodeStateAfterWrite(t *testing.T) {
	exiftoolPath := requireExiftoolAndFFmpeg(t)

	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	parentPath := filepath.Join(resolved, "parent.jpg")
	childPath := filepath.Join(resolved, "child.jpg")
	makeTaggedFixtureJPEG(t, exiftoolPath, parentPath, map[string]string{
		"EXIF:Make": "SONY", "EXIF:LensModel": "FE 24-70mm F2.8 GM", "EXIF:SerialNumber": "1234567",
	})
	makeTaggedFixtureJPEG(t, exiftoolPath, childPath, nil) // untagged: room to inherit into

	childHashBefore, childSizeBefore, _ := fastHashOf(t, childPath)

	path := filepath.Join(t.TempDir(), "inherit-refresh.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	var locID int64
	var parent, child sqlcgen.MediaNode
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "inherit-refresh", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		locID = loc.ID

		parentHash, parentSize, parentMtime := fastHashOf(t, parentPath)
		parent, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-refresh-parent", StorageLocationID: locID, FilePath: parentPath, FileName: "parent.jpg",
			FileExt: "jpg", SizeBytes: parentSize, MtimeUnix: parentMtime, FastHash: &parentHash,
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "LINKED", LifecycleState: "ACTIVE",
			CameraModel: sql.NullString{String: "ILCE-7M4", Valid: true},
		})
		if err != nil {
			return err
		}
		for _, kv := range []struct{ k, v string }{
			{"EXIF:Make", "SONY"}, {"EXIF:LensModel", "FE 24-70mm F2.8 GM"}, {"EXIF:SerialNumber", "1234567"},
		} {
			if err := q.InsertNodeMetadata(context.Background(), sqlcgen.InsertNodeMetadataParams{
				NodeID: parent.ID, Source: "exiftool", Key: kv.k, Value: kv.v,
			}); err != nil {
				return err
			}
		}

		// Seeded as INDEXED_FULL with a full_hash, as if an earlier scan had
		// escalated this node (a prior fast_hash collision, say) -- the write
		// is about to change the file's bytes, so this stale full_hash must
		// not survive the refresh (see the RefreshMediaNodeAfterInPlaceWrite
		// query comment).
		staleFullHash := "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
		child, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-refresh-child", StorageLocationID: locID, FilePath: childPath, FileName: "child.jpg",
			FileExt: "jpg", SizeBytes: childSizeBefore, MtimeUnix: time.Now().Unix(), FastHash: &childHashBefore,
			FullHash: &staleFullHash, IndexingStatus: "INDEXED_FULL", GraphStatus: "LINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}

		_, err = q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: parent.ID, TargetNodeID: child.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 1.0, Tier: 1, Resolver: "test", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	guard := storage.NewGuard([]storage.Location{{ID: locID, Name: "inherit-refresh", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: false}})
	srv := New(Deps{
		Config: &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:     database, Prober: probe.New(), Guard: guard,
		Engine: graph.NewEngine(database, nil), Hub: sse.New(), Version: "test",
	})

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Inherited map[string]string `json:"inherited"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Inherited["EXIF:Make"] != "SONY" {
		t.Errorf("inherited EXIF:Make = %q, want SONY", body.Inherited["EXIF:Make"])
	}
	if body.Inherited["XMP-xmpMM:DerivedFrom"] != "uuid-refresh-parent" {
		t.Errorf("inherited XMP-xmpMM:DerivedFrom = %q, want uuid-refresh-parent", body.Inherited["XMP-xmpMM:DerivedFrom"])
	}

	wantHash, wantSize, wantMtime := fastHashOf(t, childPath)
	if wantHash == childHashBefore {
		t.Fatalf("test setup: exiftool write did not change the file's bytes, fast_hash comparison is meaningless")
	}

	after, err := database.Reader.GetMediaNodeByID(context.Background(), child.ID)
	if err != nil {
		t.Fatalf("get child after inherit: %v", err)
	}
	if after.LifecycleState != "ACTIVE" {
		t.Errorf("child lifecycle_state = %q, want ACTIVE (must not be a fresh row -- same node)", after.LifecycleState)
	}
	if after.ID != child.ID || after.NodeUuid != child.NodeUuid {
		t.Errorf("child id/uuid changed (id %d->%d, uuid %s->%s) -- inherit-metadata must update the existing row, not fabricate a successor",
			child.ID, after.ID, child.NodeUuid, after.NodeUuid)
	}
	if after.FastHash == nil || *after.FastHash != wantHash {
		got := "nil"
		if after.FastHash != nil {
			got = *after.FastHash
		}
		t.Errorf("child fast_hash = %s, want %s (DB must agree with the rewritten file)", got, wantHash)
	}
	if after.SizeBytes != wantSize {
		t.Errorf("child size_bytes = %d, want %d", after.SizeBytes, wantSize)
	}
	if after.MtimeUnix != wantMtime {
		t.Errorf("child mtime_unix = %d, want %d", after.MtimeUnix, wantMtime)
	}
	if after.FullHash != nil {
		t.Errorf("child full_hash = %q, want nil (stale full_hash from before the write must not survive it)", *after.FullHash)
	}
	if after.IndexingStatus != "INDEXED_SHALLOW" {
		t.Errorf("child indexing_status = %q, want INDEXED_SHALLOW (downgraded from INDEXED_FULL, whose full_hash is now stale)", after.IndexingStatus)
	}
}

// TestInheritMetadataPrefersValidParentOverHigherConfidenceTier3 covers
// finding B end to end: a Tier-3 (heuristic) edge with higher confidence
// than a valid Tier-1/2 ancestry edge must not shadow it -- the request
// should succeed using the valid parent, not 409 because Tier-3 "won".
func TestInheritMetadataPrefersValidParentOverHigherConfidenceTier3(t *testing.T) {
	exiftoolPath := requireExiftoolAndFFmpeg(t)

	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	validParentPath := filepath.Join(resolved, "valid-parent.jpg")
	childPath := filepath.Join(resolved, "child.jpg")
	makeTaggedFixtureJPEG(t, exiftoolPath, validParentPath, map[string]string{"EXIF:Make": "SONY"})
	makeTaggedFixtureJPEG(t, exiftoolPath, childPath, nil)

	path := filepath.Join(t.TempDir(), "inherit-tier3-shadow.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	var locID int64
	var validParent, child sqlcgen.MediaNode
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "inherit-tier3-shadow", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		locID = loc.ID

		validHash, validSize, validMtime := fastHashOf(t, validParentPath)
		validParent, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-valid-parent", StorageLocationID: locID, FilePath: validParentPath, FileName: "valid-parent.jpg",
			FileExt: "jpg", SizeBytes: validSize, MtimeUnix: validMtime, FastHash: &validHash,
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "LINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		if err := q.InsertNodeMetadata(context.Background(), sqlcgen.InsertNodeMetadataParams{
			NodeID: validParent.ID, Source: "exiftool", Key: "EXIF:Make", Value: "SONY",
		}); err != nil {
			return err
		}

		// A Tier-3 competitor at a much higher confidence than the valid
		// parent below -- it must never be selected regardless.
		t3Parent, err := q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-t3-competitor", StorageLocationID: locID, FilePath: filepath.Join(resolved, "t3.jpg"),
			FileName: "t3.jpg", FileExt: "jpg", SizeBytes: 1, MtimeUnix: time.Now().Unix(),
			FastHash: &[]string{"cccccccccccccccc"}[0], IndexingStatus: "INDEXED_SHALLOW",
			GraphStatus: "LINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}

		childHash, childSize, _ := fastHashOf(t, childPath)
		child, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-tier3-shadow-child", StorageLocationID: locID, FilePath: childPath, FileName: "child.jpg",
			FileExt: "jpg", SizeBytes: childSize, MtimeUnix: time.Now().Unix(), FastHash: &childHash,
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "LINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}

		if _, err := q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: t3Parent.ID, TargetNodeID: child.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.99, Tier: 3, Resolver: "heuristic", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		}); err != nil {
			return err
		}
		_, err = q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: validParent.ID, TargetNodeID: child.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.70, Tier: 2, Resolver: "test", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	guard := storage.NewGuard([]storage.Location{{ID: locID, Name: "inherit-tier3-shadow", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: false}})
	srv := New(Deps{
		Config: &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:     database, Prober: probe.New(), Guard: guard,
		Engine: graph.NewEngine(database, nil), Hub: sse.New(), Version: "test",
	})

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (the valid Tier-2 parent must win despite the higher-confidence Tier-3 edge), body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Inherited map[string]string `json:"inherited"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Inherited["XMP-xmpMM:DerivedFrom"] != "uuid-valid-parent" {
		t.Errorf("inherited XMP-xmpMM:DerivedFrom = %q, want uuid-valid-parent (not the Tier-3 competitor)", body.Inherited["XMP-xmpMM:DerivedFrom"])
	}
	if body.Inherited["EXIF:Make"] != "SONY" {
		t.Errorf("inherited EXIF:Make = %q, want SONY", body.Inherited["EXIF:Make"])
	}
}

// TestLoadTagSetReadsNodeMetadataRows covers finding F's "loadTagSet remains
// untested" gap for its primary path: every tag loadTagSet reads out of
// node_metadata (not just the captured_at_unix fallback below), including
// the GPS float-parse branches, and that a non-exiftool-sourced row for the
// same key is ignored rather than silently overriding the exiftool value.
func TestLoadTagSetReadsNodeMetadataRows(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loadtagset-metadata.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()
	var node sqlcgen.MediaNode
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loadtagset-metadata", RootPath: t.TempDir(), Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		hash := "eeeeeeeeeeeeeeee"
		node, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-loadtagset-metadata", StorageLocationID: loc.ID, FilePath: "/metadata/shot.jpg",
			FileName: "shot.jpg", FileExt: "jpg", SizeBytes: 1, MtimeUnix: time.Now().Unix(), FastHash: &hash,
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "LINKED", LifecycleState: "ACTIVE",
			CameraModel: sql.NullString{String: "ILCE-7M4", Valid: true},
		})
		if err != nil {
			return err
		}
		for _, kv := range []struct{ source, key, value string }{
			{"exiftool", "EXIF:Make", "SONY"},
			{"exiftool", "EXIF:LensModel", "FE 24-70mm F2.8 GM"},
			{"exiftool", "EXIF:SerialNumber", "1234567"},
			{"exiftool", "EXIF:DateTimeOriginal", "2024:01:01 12:34:56"},
			{"exiftool", "EXIF:OffsetTimeOriginal", "+02:00"},
			{"exiftool", "Composite:GPSLatitude", "48.858222"},
			{"exiftool", "Composite:GPSLongitude", "2.2945"},
			// A non-exiftool row for a key exiftool also supplies must be
			// ignored, not override the exiftool value.
			{"internal", "EXIF:Make", "WRONG-SOURCE"},
		} {
			if err := q.InsertNodeMetadata(ctx, sqlcgen.InsertNodeMetadataParams{
				NodeID: node.ID, Source: kv.source, Key: kv.key, Value: kv.value,
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	ts, err := loadTagSet(context.Background(), database.Reader, node)
	if err != nil {
		t.Fatalf("loadTagSet: %v", err)
	}
	if ts.Make != "SONY" {
		t.Errorf("Make = %q, want SONY (the exiftool-sourced row, not the heuristic one)", ts.Make)
	}
	if ts.LensModel != "FE 24-70mm F2.8 GM" {
		t.Errorf("LensModel = %q, want FE 24-70mm F2.8 GM", ts.LensModel)
	}
	if ts.SerialNumber != "1234567" {
		t.Errorf("SerialNumber = %q, want 1234567", ts.SerialNumber)
	}
	if ts.DateTimeOriginal != "2024:01:01 12:34:56" {
		t.Errorf("DateTimeOriginal = %q, want 2024:01:01 12:34:56 (node_metadata value, not the captured_at_unix fallback)", ts.DateTimeOriginal)
	}
	if ts.OffsetTimeOriginal != "+02:00" {
		t.Errorf("OffsetTimeOriginal = %q, want +02:00", ts.OffsetTimeOriginal)
	}
	if ts.GPSLatitude == nil || *ts.GPSLatitude != 48.858222 {
		t.Errorf("GPSLatitude = %v, want 48.858222", ts.GPSLatitude)
	}
	if ts.GPSLongitude == nil || *ts.GPSLongitude != 2.2945 {
		t.Errorf("GPSLongitude = %v, want 2.2945", ts.GPSLongitude)
	}
	if ts.Model != "ILCE-7M4" {
		t.Errorf("Model = %q, want ILCE-7M4 (from the promoted camera_model column, not node_metadata)", ts.Model)
	}
	if ts.Identifier != node.NodeUuid {
		t.Errorf("Identifier = %q, want %q (always the node's own uuid)", ts.Identifier, node.NodeUuid)
	}
}

// TestLoadTagSetCapturedAtUnixFallbackEmitsExplicitUTCOffset covers finding
// G directly against loadTagSet: a node with no node_metadata rows for
// EXIF:DateTimeOriginal (a catalog indexed before #54 allowlisted the tag)
// must fall back to the promoted captured_at_unix column, and that fallback
// must pair the UTC-formatted wall clock with an explicit +00:00 offset --
// not a bare, offset-less string that exiftool would misread as local time.
func TestLoadTagSetCapturedAtUnixFallbackEmitsExplicitUTCOffset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loadtagset-fallback.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	// The instant this stands in for is 2024-01-01 12:34:56 +02:00 -- i.e.
	// 2024-01-01 10:34:56 UTC. captured_at_unix only ever stores the
	// absolute instant (see probe.capturedAt), never the original offset.
	captured := time.Date(2024, 1, 1, 10, 34, 56, 0, time.UTC).Unix()

	ctx := context.Background()
	var node sqlcgen.MediaNode
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loadtagset-fallback", RootPath: t.TempDir(), Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		hash := "dddddddddddddddd"
		node, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-loadtagset-fallback", StorageLocationID: loc.ID, FilePath: "/fallback/shot.jpg",
			FileName: "shot.jpg", FileExt: "jpg", SizeBytes: 1, MtimeUnix: time.Now().Unix(), FastHash: &hash,
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "LINKED", LifecycleState: "ACTIVE",
			CapturedAtUnix: sql.NullInt64{Int64: captured, Valid: true},
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	// Deliberately no node_metadata rows for this node -- exercises the
	// captured_at_unix fallback path, not the node_metadata lookup above it.

	ts, err := loadTagSet(context.Background(), database.Reader, node)
	if err != nil {
		t.Fatalf("loadTagSet: %v", err)
	}
	if ts.DateTimeOriginal != "2024:01:01 10:34:56" {
		t.Errorf("DateTimeOriginal = %q, want 2024:01:01 10:34:56 (captured_at_unix formatted as UTC wall clock)", ts.DateTimeOriginal)
	}
	if ts.OffsetTimeOriginal != "+00:00" {
		t.Errorf("OffsetTimeOriginal = %q, want +00:00 -- without an explicit offset, exiftool reads the bare wall clock as local time, silently shifting the instant when this value gets written into a child's file (finding G)", ts.OffsetTimeOriginal)
	}
}

// TestInheritMetadataWritesConsistentUTCTimestampFromCapturedAtUnixFallback
// is finding G's end-to-end regression test: a parent indexed before #54
// (captured_at_unix set, no node_metadata EXIF:DateTimeOriginal row) must
// still cause the child's file to receive a self-consistent
// DateTimeOriginal+OffsetTimeOriginal pair naming the exact same instant --
// not a wall-clock value silently shifted by the original capture's offset.
func TestInheritMetadataWritesConsistentUTCTimestampFromCapturedAtUnixFallback(t *testing.T) {
	exiftoolPath := requireExiftoolAndFFmpeg(t)

	root := t.TempDir()
	resolved, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	parentPath := filepath.Join(resolved, "parent.jpg")
	childPath := filepath.Join(resolved, "child.jpg")
	makeTaggedFixtureJPEG(t, exiftoolPath, parentPath, nil) // untagged: DateTimeOriginal comes from captured_at_unix, not the file
	makeTaggedFixtureJPEG(t, exiftoolPath, childPath, nil)

	captured := time.Date(2024, 1, 1, 10, 34, 56, 0, time.UTC).Unix()

	path := filepath.Join(t.TempDir(), "inherit-utc-fallback.db")
	database, err := db.Open(context.Background(), path)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	var locID int64
	var parent, child sqlcgen.MediaNode
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "inherit-utc-fallback", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		locID = loc.ID

		parentHash, parentSize, parentMtime := fastHashOf(t, parentPath)
		parent, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-utc-fallback-parent", StorageLocationID: locID, FilePath: parentPath, FileName: "parent.jpg",
			FileExt: "jpg", SizeBytes: parentSize, MtimeUnix: parentMtime, FastHash: &parentHash,
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "LINKED", LifecycleState: "ACTIVE",
			CapturedAtUnix: sql.NullInt64{Int64: captured, Valid: true},
		})
		if err != nil {
			return err
		}
		// No node_metadata row for EXIF:DateTimeOriginal -- pre-#54 catalog.

		childHash, childSize, _ := fastHashOf(t, childPath)
		child, err = q.InsertMediaNode(context.Background(), sqlcgen.InsertMediaNodeParams{
			NodeUuid: "uuid-utc-fallback-child", StorageLocationID: locID, FilePath: childPath, FileName: "child.jpg",
			FileExt: "jpg", SizeBytes: childSize, MtimeUnix: time.Now().Unix(), FastHash: &childHash,
			IndexingStatus: "INDEXED_SHALLOW", GraphStatus: "LINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}

		_, err = q.CreateMediaEdge(context.Background(), sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: parent.ID, TargetNodeID: child.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 1.0, Tier: 1, Resolver: "test", EvidenceJson: "{}", ReviewState: "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}

	guard := storage.NewGuard([]storage.Location{{ID: locID, Name: "inherit-utc-fallback", RootPath: resolved, Tier: "TIER2_EXPORTS", ReadOnly: false}})
	srv := New(Deps{
		Config: &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:     database, Prober: probe.New(), Guard: guard,
		Engine: graph.NewEngine(database, nil), Hub: sse.New(), Version: "test",
	})

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(child.ID)+"/inherit-metadata", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	var body struct {
		Inherited map[string]string `json:"inherited"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal response: %v", err)
	}
	if body.Inherited["EXIF:DateTimeOriginal"] != "2024:01:01 10:34:56" {
		t.Errorf("inherited EXIF:DateTimeOriginal = %q, want 2024:01:01 10:34:56", body.Inherited["EXIF:DateTimeOriginal"])
	}
	if body.Inherited["EXIF:OffsetTimeOriginal"] != "+00:00" {
		t.Errorf("inherited EXIF:OffsetTimeOriginal = %q, want +00:00", body.Inherited["EXIF:OffsetTimeOriginal"])
	}

	// Read back what actually landed in the file, not just the response
	// body -- this is what proves the tags reached WriteTags and were
	// accepted by exiftool as a valid, self-consistent pair.
	out, err := exec.Command(exiftoolPath, "-j", "-EXIF:DateTimeOriginal", "-EXIF:OffsetTimeOriginal", childPath).CombinedOutput()
	if err != nil {
		t.Fatalf("read back child tags: %v\n%s", err, out)
	}
	var reread []struct {
		DateTimeOriginal   string `json:"DateTimeOriginal"`
		OffsetTimeOriginal string `json:"OffsetTimeOriginal"`
	}
	if err := json.Unmarshal(out, &reread); err != nil || len(reread) != 1 {
		t.Fatalf("unmarshal exiftool -j output: %v\n%s", err, out)
	}
	if reread[0].DateTimeOriginal != "2024:01:01 10:34:56" {
		t.Errorf("child file's DateTimeOriginal = %q, want 2024:01:01 10:34:56", reread[0].DateTimeOriginal)
	}
	if reread[0].OffsetTimeOriginal != "+00:00" {
		t.Errorf("child file's OffsetTimeOriginal = %q, want +00:00 (a bare wall clock with no offset would be misread as local time by any later reader, silently shifting the instant)", reread[0].OffsetTimeOriginal)
	}
}

func TestMeReflectsBrowserPrincipal(t *testing.T) {
	srv, _ := fullTestServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/me", nil)
	req.Header.Set("X-Authentik-Username", "alice")
	req.Header.Set("X-Authentik-Groups", "dam-admins")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Kind   string   `json:"kind"`
		Name   string   `json:"name"`
		Groups []string `json:"groups"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Kind != string(auth.KindUser) || got.Name != "alice" {
		t.Errorf("got %+v, want user alice", got)
	}
}

func TestMeReflectsMachinePrincipalOnAgentPath(t *testing.T) {
	srv, _ := fullTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAgentRoutesRejectMissingKey(t *testing.T) {
	srv, _ := fullTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/hello", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestConfigReturnsVersion(t *testing.T) {
	srv, _ := fullTestServer(t)
	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/config", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Version string `json:"version"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Version != "test" {
		t.Errorf("version = %q, want test", got.Version)
	}
}

func TestGetAssetNotFound(t *testing.T) {
	srv, _ := fullTestServer(t)
	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/999999", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAuditQueueEmpty(t *testing.T) {
	srv, _ := fullTestServer(t)
	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/edges/audit", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Entries []any `json:"entries"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Entries) != 0 {
		t.Errorf("entries = %v, want empty on a fresh database", got.Entries)
	}
}

func TestListStorageLocations(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	archiveRoot := t.TempDir()
	resolvedArchive, err := filepath.EvalSymlinks(archiveRoot)
	if err != nil {
		t.Fatalf("resolve archive root: %v", err)
	}
	scratchRoot := t.TempDir()
	resolvedScratch, err := filepath.EvalSymlinks(scratchRoot)
	if err != nil {
		t.Fatalf("resolve scratch root: %v", err)
	}

	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		if _, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "archive", RootPath: resolvedArchive, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: 1, Prunable: 0,
		}); err != nil {
			return err
		}
		_, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "scratch", RootPath: resolvedScratch, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 1,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/storage-locations", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/storage-locations status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		Locations []storageLocationDTO `json:"locations"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(got.Locations) != 2 {
		t.Fatalf("got %d locations, want 2: %+v", len(got.Locations), got.Locations)
	}
	archive, scratch := got.Locations[0], got.Locations[1]
	if archive.Tier != "TIER3_MASTER_ARCHIVE" || !archive.ReadOnly {
		t.Errorf("archive = %+v, want TIER3_MASTER_ARCHIVE readOnly", archive)
	}
	if scratch.Tier != "TIER1_LOCAL_SCRATCH" || scratch.ReadOnly {
		t.Errorf("scratch = %+v, want TIER1_LOCAL_SCRATCH not readOnly", scratch)
	}
	if scratch.Name != "scratch" || scratch.RootPath != resolvedScratch || !scratch.Prunable {
		t.Errorf("scratch = %+v, want name/rootPath round-trip and prunable", scratch)
	}
}

func TestAgentEventEnqueues(t *testing.T) {
	srv, _ := fullTestServer(t)
	body := map[string]string{
		"agentId":   "workstation-1",
		"eventType": "EVENT_NODE_CREATED",
		"payload":   `{"path":"/tmp/example.jpg"}`,
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/events", bytesOfJSON(t, body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusAccepted {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var got struct {
		EventID string `json:"eventId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.EventID == "" {
		t.Error("eventId is empty")
	}
}

func bytesOfJSON(t *testing.T, v any) *bytes.Reader {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return bytes.NewReader(b)
}

// TestScanEndToEndThroughHTTP is the capstone: POST /api/v1/scan against a
// real directory, poll /api/v1/progress until it completes, then confirm
// the resulting nodes are visible through GET /api/v1/assets -- proving
// the HTTP layer actually wires storage.Guard + workers.Pool + pipeline +
// graph.Engine together, not just that each layer works standalone.
func TestScanEndToEndThroughHTTP(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "two.txt"), []byte("bravo"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}

	var locationID int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "http-scan-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		locationID = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}
	guard := storage.NewGuard([]storage.Location{
		{ID: locationID, Name: "http-scan-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false},
	})
	srv.guard = guard

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/scan", map[string]int64{"storageLocationId": locationID})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST /api/v1/scan status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var scanResp struct {
		JobID int64 `json:"jobId"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &scanResp); err != nil {
		t.Fatalf("unmarshal scan response: %v", err)
	}
	if scanResp.JobID == 0 {
		t.Fatal("jobId is 0")
	}

	deadline := time.Now().Add(10 * time.Second)
	var job sqlcgen.ScanJob
	for time.Now().Before(deadline) {
		job, err = database.Reader.GetScanJob(ctx, scanResp.JobID)
		if err != nil {
			t.Fatalf("GetScanJob: %v", err)
		}
		if job.State != "RUNNING" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.State != "COMPLETED" {
		t.Fatalf("job state = %q, want COMPLETED (last_error=%v)", job.State, job.LastError)
	}
	if job.FilesSeen != 2 {
		t.Errorf("FilesSeen = %d, want 2", job.FilesSeen)
	}

	rr = doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/assets status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var assetsResp struct {
		Assets []assetDTO `json:"assets"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &assetsResp); err != nil {
		t.Fatalf("unmarshal assets response: %v", err)
	}
	if len(assetsResp.Assets) != 2 {
		t.Fatalf("got %d assets, want 2: %+v", len(assetsResp.Assets), assetsResp.Assets)
	}
}

// TestStartScanRejectsDuplicateRunningScan backs #163: POSTing /api/v1/scan
// for a location that already has a RUNNING FULL_SCAN job must return 409,
// not silently start a second walk that starves on workers.Pool.Submit's
// per-path dedup. A terminalized prior job must not block a following scan.
func TestStartScanRejectsDuplicateRunningScan(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	root := t.TempDir()
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}

	var locationID int64
	var runningJobID int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "http-scan-dup-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		locationID = loc.ID
		job, err := q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{
			StorageLocationID: sql.NullInt64{Int64: loc.ID, Valid: true},
			Kind:              "FULL_SCAN",
		})
		runningJobID = job.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location + running job: %v", err)
	}
	srv.guard = storage.NewGuard([]storage.Location{
		{ID: locationID, Name: "http-scan-dup-test", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false},
	})

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/scan", map[string]int64{"storageLocationId": locationID})
	if rr.Code != http.StatusConflict {
		t.Fatalf("POST /api/v1/scan while a FULL_SCAN is RUNNING: status = %d, body = %s", rr.Code, rr.Body.String())
	}

	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		return q.CompleteScanJob(ctx, runningJobID)
	}); err != nil {
		t.Fatalf("terminalize seeded job: %v", err)
	}

	rr = doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/scan", map[string]int64{"storageLocationId": locationID})
	if rr.Code != http.StatusAccepted {
		t.Fatalf("POST /api/v1/scan after prior job terminalized: status = %d, body = %s", rr.Code, rr.Body.String())
	}
}

// TestScanJobOutlivesRequestContext is the C1 regression guard: the scan
// goroutine must outlive the HTTP request context that started it. A real TCP
// server (httptest.NewServer) cancels the request's context the moment the
// handler returns -- unlike doJSON's direct ServeHTTP, which never does -- so
// this test fails if RunScan inherits the request ctx: the walk's per-entry
// check aborts with context.Canceled (job FAILED) or the commit/sweep InTxs
// fail on the canceled ctx (job stuck RUNNING forever).
func TestScanJobOutlivesRequestContext(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "one.txt"), []byte("alpha"), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}

	var locationID int64
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "http-scan-request-ctx", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		locationID = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}
	srv.guard = storage.NewGuard([]storage.Location{
		{ID: locationID, Name: "http-scan-request-ctx", RootPath: resolvedRoot, Tier: "TIER2_EXPORTS", ReadOnly: false},
	})

	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	body, err := json.Marshal(map[string]int64{"storageLocationId": locationID})
	if err != nil {
		t.Fatalf("marshal scan request: %v", err)
	}
	req, err := http.NewRequest(http.MethodPost, ts.URL+"/api/v1/scan", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build scan request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// #164: a real deployment always sits behind Traefik's ForwardAuth,
	// which asserts this header -- see doJSON's doc comment.
	req.Header.Set("X-Authentik-Username", "http-scan-request-ctx-test-user")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /api/v1/scan: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /api/v1/scan status = %d", resp.StatusCode)
	}
	var scanResp struct {
		JobID int64 `json:"jobId"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&scanResp); err != nil {
		t.Fatalf("decode scan response: %v", err)
	}
	if scanResp.JobID == 0 {
		t.Fatal("jobId is 0")
	}

	deadline := time.Now().Add(10 * time.Second)
	var job sqlcgen.ScanJob
	for time.Now().Before(deadline) {
		job, err = database.Reader.GetScanJob(ctx, scanResp.JobID)
		if err != nil {
			t.Fatalf("GetScanJob: %v", err)
		}
		if job.State != "RUNNING" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if job.State != "COMPLETED" {
		t.Fatalf("job state = %q, want COMPLETED (last_error=%v) -- scan did not outlive the request context", job.State, job.LastError)
	}
	if job.FilesSeen != 1 {
		t.Errorf("FilesSeen = %d, want 1", job.FilesSeen)
	}
}

func TestMutatingRoutesAuthorization(t *testing.T) {
	srv, _ := fullTestServer(t)
	srv.cfg.Authz.Groups = []string{"dam-admins"}

	mutatingPaths := []string{
		"/api/v1/scan",
		"/api/v1/edges/1/confirm",
		"/api/v1/edges/1/reject",
		"/api/v1/prune",
	}

	for _, path := range mutatingPaths {
		t.Run("non-admin forbidden: "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Authentik-Username", "alice")
			req.Header.Set("X-Authentik-Groups", "dam-users")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Errorf("status = %d, want %d for non-admin on %s", rr.Code, http.StatusForbidden, path)
			}
		})

		t.Run("admin allowed: "+path, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, path, bytes.NewBufferString("{}"))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Authentik-Username", "bob")
			req.Header.Set("X-Authentik-Groups", "dam-users|dam-admins")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code == http.StatusForbidden {
				t.Errorf("status = %d, want non-403 for admin on %s", rr.Code, path)
			}
		})
	}
}

func TestOpenAPIEndpointExposure(t *testing.T) {
	t.Run("disabled by default", func(t *testing.T) {
		srv, _ := fullTestServer(t)
		srv.cfg.HTTP.ExposeOpenAPI = false

		paths := []string{"/openapi.json", "/openapi.yaml", "/docs"}
		for _, p := range paths {
			req := httptest.NewRequest(http.MethodGet, p, nil)
			req.Header.Set("X-Authentik-Username", "alice")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusNotFound {
				t.Errorf("GET %s when exposeOpenAPI=false: status = %d, want %d", p, rr.Code, http.StatusNotFound)
			}
		}
	})

	t.Run("enabled and gated by admin check", func(t *testing.T) {
		srv, _ := fullTestServer(t)
		srv.cfg.HTTP.ExposeOpenAPI = true
		srv.cfg.Authz.Groups = []string{"dam-admins"}

		paths := []string{"/openapi.json", "/docs"}
		for _, p := range paths {
			// Non-admin -> 403 Forbidden
			req := httptest.NewRequest(http.MethodGet, p, nil)
			req.Header.Set("X-Authentik-Username", "alice")
			req.Header.Set("X-Authentik-Groups", "dam-users")
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Errorf("GET %s non-admin when exposeOpenAPI=true: status = %d, want %d", p, rr.Code, http.StatusForbidden)
			}

			// Admin -> 200 OK
			reqAdmin := httptest.NewRequest(http.MethodGet, p, nil)
			reqAdmin.Header.Set("X-Authentik-Username", "bob")
			reqAdmin.Header.Set("X-Authentik-Groups", "dam-admins")
			rrAdmin := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rrAdmin, reqAdmin)

			if rrAdmin.Code != http.StatusOK {
				t.Errorf("GET %s admin when exposeOpenAPI=true: status = %d, want %d", p, rrAdmin.Code, http.StatusOK)
			}
		}
	})

	// #164: openAPIMiddleware's admin check has its own !ok/group-check
	// logic, separate from RequireAdmin's -- a request with zero identity
	// headers at all must not slip past it via the empty-allowedGroups
	// permits-all path, the same gap this issue closes for mutating routes.
	t.Run("no identity headers denied even with empty allowedGroups", func(t *testing.T) {
		srv, _ := fullTestServer(t)
		srv.cfg.HTTP.ExposeOpenAPI = true

		paths := []string{"/openapi.json", "/docs"}
		for _, p := range paths {
			req := httptest.NewRequest(http.MethodGet, p, nil)
			rr := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rr, req)

			if rr.Code != http.StatusForbidden {
				t.Errorf("GET %s with no identity headers, empty allowedGroups: status = %d, want %d", p, rr.Code, http.StatusForbidden)
			}
		}
	})
}

func TestHandleListPathRewrites(t *testing.T) {
	srv, _ := fullTestServer(t)
	srv.cfg.PathRewrites = []config.PathRewrite{
		{From: "D:\\Footage", To: "/storage/footage"},
		{From: "/Volumes/NAS", To: "/storage/nas"},
	}

	req := httptest.NewRequest(http.MethodGet, "/api/v1/config/path-rewrites", nil)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/config/path-rewrites: status = %d, want %d", rr.Code, http.StatusOK)
	}

	var rewrites []PathRewriteDTO
	if err := json.NewDecoder(rr.Body).Decode(&rewrites); err != nil {
		t.Fatalf("decode json response: %v", err)
	}

	if len(rewrites) != 2 {
		t.Fatalf("got %d path rewrites, want 2", len(rewrites))
	}
	if rewrites[0].From != "D:\\Footage" || rewrites[0].To != "/storage/footage" {
		t.Errorf("rewrite 0 mismatch: %+v", rewrites[0])
	}
	if rewrites[1].From != "/Volumes/NAS" || rewrites[1].To != "/storage/nas" {
		t.Errorf("rewrite 1 mismatch: %+v", rewrites[1])
	}
}

func TestAssetLineageNotFound(t *testing.T) {
	srv, _ := fullTestServer(t)
	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/999999/lineage", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAssetLineageInvalidDepth(t *testing.T) {
	srv, _ := fullTestServer(t)
	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/1/lineage?depth=10", nil)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422 for out-of-range depth, body = %s", rr.Code, rr.Body.String())
	}
}

func TestAssetLineageTraversalDiamondAndDepth(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	// Seed 4-level deep graph with a diamond and rejected/archived nodes:
	// N1 (Root) -> N2 -> N4 (Diamond)
	// N1 (Root) -> N3 -> N4 (Diamond) -> N5 (Level 4)
	// N6 -> N1 (REJECTED edge)
	// N7 (ARCHIVED node)
	var n1, n2, n3, n4, n5, n6, n7 sqlcgen.MediaNode

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "test-loc", RootPath: "/tmp/loc", Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}

		createNode := func(name string, state string) (sqlcgen.MediaNode, error) {
			return q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
				NodeUuid:          uuid.New().String(),
				StorageLocationID: loc.ID,
				FilePath:          "/tmp/" + name,
				FileName:          name,
				FileExt:           ".jpg",
				SizeBytes:         100,
				MtimeUnix:         time.Now().Unix(),
				IndexingStatus:    "INDEXED_FULL",
				GraphStatus:       "LINKED",
				LifecycleState:    state,
			})
		}

		if n1, err = createNode("n1.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n2, err = createNode("n2.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n3, err = createNode("n3.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n4, err = createNode("n4.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n5, err = createNode("n5.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n6, err = createNode("n6.jpg", "ACTIVE"); err != nil {
			return err
		}
		if n7, err = createNode("n7.jpg", "ARCHIVED"); err != nil {
			return err
		}

		createEdge := func(src, tgt int64, state string) error {
			e, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
				SourceNodeID:     src,
				TargetNodeID:     tgt,
				RelationshipType: "DERIVED_FROM",
				Confidence:       0.9,
				ReviewState:      "AUTO_ACCEPTED",
				Resolver:         "test",
				Tier:             1,
			})
			if err != nil {
				return err
			}
			if state == "CONFIRMED" {
				_, err := q.ConfirmMediaEdge(ctx, sqlcgen.ConfirmMediaEdgeParams{
					ID:         e.ID,
					ReviewedBy: sql.NullString{String: "tester", Valid: true},
				})
				return err
			}
			if state == "REJECTED" {
				_, err := q.RejectMediaEdge(ctx, sqlcgen.RejectMediaEdgeParams{
					ID:         e.ID,
					ReviewedBy: sql.NullString{String: "tester", Valid: true},
				})
				return err
			}
			return nil
		}

		if err := createEdge(n1.ID, n2.ID, "CONFIRMED"); err != nil {
			return err
		}
		if err := createEdge(n1.ID, n3.ID, "CONFIRMED"); err != nil {
			return err
		}
		if err := createEdge(n2.ID, n4.ID, "CONFIRMED"); err != nil {
			return err
		}
		if err := createEdge(n3.ID, n4.ID, "CONFIRMED"); err != nil {
			return err
		}
		if err := createEdge(n4.ID, n5.ID, "CONFIRMED"); err != nil {
			return err
		}
		if err := createEdge(n6.ID, n1.ID, "REJECTED"); err != nil {
			return err
		}
		if err := createEdge(n1.ID, n7.ID, "CONFIRMED"); err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		t.Fatalf("seed test graph: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/"+toStr(n1.ID)+"/lineage?depth=3", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	var got struct {
		RootID int64      `json:"rootId"`
		Nodes  []assetDTO `json:"nodes"`
		Edges  []edgeDTO  `json:"edges"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if got.RootID != n1.ID {
		t.Errorf("rootId = %d, want %d", got.RootID, n1.ID)
	}

	// Verify diamond deduplication: n4 should appear exactly ONCE
	n4Count := 0
	nodeIDs := make(map[int64]bool)
	for _, node := range got.Nodes {
		nodeIDs[node.ID] = true
		if node.ID == n4.ID {
			n4Count++
		}
		if node.ID == n6.ID {
			t.Errorf("node %d (REJECTED edge source) should not be included in lineage", n6.ID)
		}
		if node.ID == n7.ID {
			t.Errorf("node %d (ARCHIVED node) should not be included in lineage", n7.ID)
		}
	}

	if n4Count != 1 {
		t.Errorf("node n4 count = %d, want 1 (diamond deduplication failure)", n4Count)
	}

	if !nodeIDs[n1.ID] || !nodeIDs[n2.ID] || !nodeIDs[n3.ID] || !nodeIDs[n4.ID] || !nodeIDs[n5.ID] {
		t.Errorf("nodes set missing expected graph nodes: got %+v", nodeIDs)
	}

	// Verify querying an ARCHIVED root asset returns 404
	rrArchived := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/"+toStr(n7.ID)+"/lineage", nil)
	if rrArchived.Code != http.StatusNotFound {
		t.Errorf("GET /api/v1/assets/%d/lineage (ARCHIVED) status = %d, want 404", n7.ID, rrArchived.Code)
	}
}

func TestCreateManualEdge(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	var loc sqlcgen.StorageLocation
	var n1, n2 sqlcgen.MediaNode

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		loc, err = q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loc", RootPath: t.TempDir(), Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		n1, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-m1", FilePath: "/m1.raw", FileName: "m1.raw", FileExt: ".raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		n2, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-m2", FilePath: "/m2.jpg", FileName: "m2.jpg", FileExt: ".jpg", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes: %v", err)
	}

	// 1. Create valid manual edge n1 -> n2
	body := map[string]any{
		"sourceNodeId":     n1.ID,
		"targetNodeId":     n2.ID,
		"relationshipType": "DERIVED_FROM",
	}
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/edges status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	var created edgeDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.SourceNodeID != n1.ID || created.TargetNodeID != n2.ID || created.Resolver != "manual" || created.ReviewState != "CONFIRMED" {
		t.Errorf("created edge = %+v, unexpected fields", created)
	}

	// 2. Self-loop attempt n1 -> n1 returns 422
	selfLoopBody := map[string]any{
		"sourceNodeId":     n1.ID,
		"targetNodeId":     n1.ID,
		"relationshipType": "DERIVED_FROM",
	}
	rrSelf := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", selfLoopBody)
	if rrSelf.Code != http.StatusUnprocessableEntity {
		t.Errorf("self-loop status = %d, want 422", rrSelf.Code)
	}

	// 3. Cycle attempt n2 -> n1 (when n1 -> n2 exists) returns 409 Conflict
	cycleBody := map[string]any{
		"sourceNodeId":     n2.ID,
		"targetNodeId":     n1.ID,
		"relationshipType": "DERIVED_FROM",
	}
	rrCycle := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", cycleBody)
	if rrCycle.Code != http.StatusConflict {
		t.Errorf("cycle status = %d, want 409, body = %s", rrCycle.Code, rrCycle.Body.String())
	}
}

// TestCreateManualEdgeRefusesProjectFileNodes backs #199: a manual edge must
// not link a project file (.drp/.fcpxml/.edl/.dam.json/.prproj) as an
// endpoint of an identity-ancestry relationship (DERIVED_FROM/FINAL_EXPORT/
// PROXY_OF) -- the edge is never created CONFIRMED. The child (target) case
// is the dangerous one: a DERIVED_FROM edge targeting a project file would
// pass pickWinningParent and make handleInheritMetadata run exiftool
// in-place against the project archive. Both the target-side and
// source-side refusals are scoped to identity-ancestry relationship types
// (see TestCreateManualEdgeAllowsProjectSidecarTarget for the target side,
// TestCreateManualEdgeAllowsProjectFileAsSourceForNonAncestryRelationship for
// the source side) -- this test only exercises DERIVED_FROM, which is in
// that set on both ends.
func TestCreateManualEdgeRefusesProjectFileNodes(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	var loc sqlcgen.StorageLocation
	var media, media2, proj sqlcgen.MediaNode
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		loc, err = q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loc", RootPath: t.TempDir(), Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		media, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-media", FilePath: "/m.raw", FileName: "m.raw", FileExt: "raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		media2, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-media2", FilePath: "/m2.jpg", FileName: "m2.jpg", FileExt: "jpg", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		proj, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-project", FilePath: "/proj.drp", FileName: "proj.drp", FileExt: "drp", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes: %v", err)
	}

	// 1. DERIVED_FROM with the project file as the CHILD (target) is refused.
	body := map[string]any{
		"sourceNodeId":     media.ID,
		"targetNodeId":     proj.ID,
		"relationshipType": "DERIVED_FROM",
	}
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", body)
	if rr.Code != http.StatusUnprocessableEntity {
		t.Fatalf("project-file-target status = %d, want 422, body = %s", rr.Code, rr.Body.String())
	}
	if got, want := strings.TrimSpace(rr.Body.String()), "cannot create an identity-ancestry edge targeting a project file"; !strings.Contains(got, want) {
		t.Errorf("project-file-target body = %q, want it to contain %q", got, want)
	}

	// 2. DERIVED_FROM with the project file as the SOURCE (parent) is refused.
	bodySource := map[string]any{
		"sourceNodeId":     proj.ID,
		"targetNodeId":     media.ID,
		"relationshipType": "DERIVED_FROM",
	}
	rrSource := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", bodySource)
	if rrSource.Code != http.StatusUnprocessableEntity {
		t.Fatalf("project-file-source status = %d, want 422, body = %s", rrSource.Code, rrSource.Body.String())
	}

	// 3. A normal media->media edge is still accepted (the refusal is scoped
	// to project files, not to the manual-edge path in general).
	rrOK := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", map[string]any{
		"sourceNodeId":     media.ID,
		"targetNodeId":     media2.ID,
		"relationshipType": "DERIVED_FROM",
	})
	if rrOK.Code != http.StatusOK {
		t.Fatalf("media->media status = %d, want 200, body = %s", rrOK.Code, rrOK.Body.String())
	}

	// 4. No edge was created with the project file at either endpoint.
	byTarget, err := database.Reader.ListEdgesByTarget(ctx, proj.ID)
	if err != nil {
		t.Fatalf("list edges by target: %v", err)
	}
	if len(byTarget) != 0 {
		t.Errorf("expected no edges targeting the project file, got %d", len(byTarget))
	}
	bySource, err := database.Reader.ListEdgesBySource(ctx, proj.ID)
	if err != nil {
		t.Fatalf("list edges by source: %v", err)
	}
	if len(bySource) != 0 {
		t.Errorf("expected no edges sourced from the project file, got %d", len(bySource))
	}
}

// TestCreateManualEdgeAllowsProjectSidecarTarget backs #199's scoping of the
// target-side project-file refusal: PROJECT_SIDECAR edges *legitimately*
// target a project file -- ProjectSidecarResolver.Resolve always makes the
// project file the child of such an edge (media node as parent/source). It is
// excluded from validParentRelationships, so it can never reach
// pickWinningParent/handleInheritMetadata's exiftool call, and the manual
// path must keep allowing exactly the shape the automatic resolver emits.
func TestCreateManualEdgeAllowsProjectSidecarTarget(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	var media, proj sqlcgen.MediaNode
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loc", RootPath: t.TempDir(), Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		media, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-media", FilePath: "/m.raw", FileName: "m.raw", FileExt: "raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		proj, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-project", FilePath: "/proj.drp", FileName: "proj.drp", FileExt: "drp", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes: %v", err)
	}

	// PROJECT_SIDECAR with the project file as the CHILD (target) -- the exact
	// shape ProjectSidecarResolver emits -- is accepted, not refused.
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", map[string]any{
		"sourceNodeId":     media.ID,
		"targetNodeId":     proj.ID,
		"relationshipType": "PROJECT_SIDECAR",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("PROJECT_SIDECAR->project-file status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}
	var created edgeDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if created.SourceNodeID != media.ID || created.TargetNodeID != proj.ID || created.RelationshipType != "PROJECT_SIDECAR" {
		t.Errorf("created edge = %+v, unexpected fields", created)
	}

	// The edge really landed (CONFIRMED at insert time, manual resolver).
	byTarget, err := database.Reader.ListEdgesByTarget(ctx, proj.ID)
	if err != nil {
		t.Fatalf("list edges by target: %v", err)
	}
	if len(byTarget) != 1 || byTarget[0].RelationshipType != "PROJECT_SIDECAR" || byTarget[0].ReviewState != "CONFIRMED" {
		t.Errorf("edges targeting the project file = %+v, want exactly one CONFIRMED PROJECT_SIDECAR", byTarget)
	}
}

// TestCreateManualEdgeAllowsProjectFileAsSourceForNonAncestryRelationship
// backs the source-side scoping fix: DUPLICATE_OF is excluded from
// validParentRelationships, so it can never reach pickWinningParent/
// handleInheritMetadata's exiftool call regardless of which endpoint is a
// project file -- a user marking two project files (or a project file and a
// media node) as DUPLICATE_OF poses none of the hazard the source-side
// refusal exists to close, and the web UI's ManualLinkModal lets the caller
// pick source/target freely for exactly this relationship type.
func TestCreateManualEdgeAllowsProjectFileAsSourceForNonAncestryRelationship(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	var media, proj sqlcgen.MediaNode
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loc", RootPath: t.TempDir(), Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		media, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-media", FilePath: "/m.raw", FileName: "m.raw", FileExt: "raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		proj, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-project", FilePath: "/proj.drp", FileName: "proj.drp", FileExt: "drp", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes: %v", err)
	}

	// DUPLICATE_OF with the project file as the SOURCE (parent) -- the shape
	// the old unconditional source-side refusal used to block -- is accepted.
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", map[string]any{
		"sourceNodeId":     proj.ID,
		"targetNodeId":     media.ID,
		"relationshipType": "DUPLICATE_OF",
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("DUPLICATE_OF project-source status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	byTarget, err := database.Reader.ListEdgesByTarget(ctx, media.ID)
	if err != nil {
		t.Fatalf("list edges by target: %v", err)
	}
	if len(byTarget) != 1 || byTarget[0].RelationshipType != "DUPLICATE_OF" || byTarget[0].SourceNodeID != proj.ID {
		t.Errorf("edges targeting media = %+v, want exactly one DUPLICATE_OF sourced from the project file", byTarget)
	}
}

// TestCreateManualEdgeRefusesArchivedEndpoint backs the #115 lifecycle-state
// guard: an ARCHIVED node passes the existence/FK check handleCreateEdge
// already performs, but a manual edge to or from it is never traversed by
// anything (v_media_edges_resolved / lineage walks all key off a live node),
// so it would be a dead write masquerading as a real link.
func TestCreateManualEdgeRefusesArchivedEndpoint(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	var live, archived sqlcgen.MediaNode
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loc", RootPath: t.TempDir(), Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		live, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-live", FilePath: "/live.raw", FileName: "live.raw", FileExt: "raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		archived, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-archived", FilePath: "/archived.jpg", FileName: "archived.jpg", FileExt: "jpg", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ARCHIVED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes: %v", err)
	}

	rrTarget := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", map[string]any{
		"sourceNodeId":     live.ID,
		"targetNodeId":     archived.ID,
		"relationshipType": "DERIVED_FROM",
	})
	if rrTarget.Code != http.StatusNotFound {
		t.Errorf("archived-target status = %d, want 404, body = %s", rrTarget.Code, rrTarget.Body.String())
	}

	rrSource := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", map[string]any{
		"sourceNodeId":     archived.ID,
		"targetNodeId":     live.ID,
		"relationshipType": "DERIVED_FROM",
	})
	if rrSource.Code != http.StatusNotFound {
		t.Errorf("archived-source status = %d, want 404, body = %s", rrSource.Code, rrSource.Body.String())
	}
}

// TestCreateManualEdgeMissingNodeReturns404 covers the node lookups
// handleCreateEdge now performs for its project-file check: a nonexistent
// source or target is a 404, not the 500 a foreign-key violation used to
// produce.
func TestCreateManualEdgeMissingNodeReturns404(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	var node sqlcgen.MediaNode
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loc", RootPath: t.TempDir(), Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		node, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-m", FilePath: "/m.raw", FileName: "m.raw", FileExt: "raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}

	rrMissingTarget := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", map[string]any{
		"sourceNodeId":     node.ID,
		"targetNodeId":     node.ID + 1,
		"relationshipType": "DERIVED_FROM",
	})
	if rrMissingTarget.Code != http.StatusNotFound {
		t.Errorf("missing target status = %d, want 404, body = %s", rrMissingTarget.Code, rrMissingTarget.Body.String())
	}

	rrMissingSource := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", map[string]any{
		"sourceNodeId":     node.ID + 1,
		"targetNodeId":     node.ID,
		"relationshipType": "DERIVED_FROM",
	})
	if rrMissingSource.Code != http.StatusNotFound {
		t.Errorf("missing source status = %d, want 404, body = %s", rrMissingSource.Code, rrMissingSource.Body.String())
	}
}

// TestCreateManualEdgeSetsGraphStatusLinked backs M3's third fix: a manual
// edge is CONFIRMED at insert time (CreateManualMediaEdge), the same
// review_state write confirm/reject makes -- the target node's
// graph_status must reflect that immediately, not stay UNLINKED until an
// unrelated resolve pass happens to touch it.
func TestCreateManualEdgeSetsGraphStatusLinked(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	n1, n2 := seedTwoUnlinkedNodes(t, database)

	body := map[string]any{
		"sourceNodeId":     n1.ID,
		"targetNodeId":     n2.ID,
		"relationshipType": "DERIVED_FROM",
	}
	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges", body)
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /api/v1/edges status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	target, err := database.Reader.GetMediaNodeByID(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get target node: %v", err)
	}
	if target.GraphStatus != "LINKED" {
		t.Errorf("target graph_status = %q, want LINKED", target.GraphStatus)
	}
}

// seedTwoUnlinkedNodes is a small shared fixture for the confirm/reject/
// manual-create tests below: two UNLINKED nodes in one storage location, no
// edge between them yet.
func seedTwoUnlinkedNodes(t *testing.T, database *db.DB) (n1, n2 sqlcgen.MediaNode) {
	t.Helper()
	ctx := context.Background()
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: t.Name(), RootPath: t.TempDir(), Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		n1, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-" + t.Name() + "-1", FilePath: "/p-" + t.Name() + ".raw",
			FileName: "p.raw", FileExt: "raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		n2, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-" + t.Name() + "-2", FilePath: "/c-" + t.Name() + ".jpg",
			FileName: "c.jpg", FileExt: "jpg", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes: %v", err)
	}
	return n1, n2
}

func TestConfirmRejectEdgeNotFoundReturns404(t *testing.T) {
	srv, _ := fullTestServer(t)

	rrConfirm := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges/999999/confirm", nil)
	if rrConfirm.Code != http.StatusNotFound {
		t.Errorf("confirm nonexistent edge status = %d, want 404, body = %s", rrConfirm.Code, rrConfirm.Body.String())
	}

	rrReject := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/edges/999999/reject", nil)
	if rrReject.Code != http.StatusNotFound {
		t.Errorf("reject nonexistent edge status = %d, want 404, body = %s", rrReject.Code, rrReject.Body.String())
	}
}

// TestConfirmEdgeRecomputesGraphStatus backs M3: confirming a NEEDS_REVIEW
// edge must flip the target node's graph_status to LINKED in the same
// request, not leave it stale until the next scan happens to re-resolve it.
func TestConfirmEdgeRecomputesGraphStatus(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	n1, n2 := seedTwoUnlinkedNodes(t, database)

	var edge sqlcgen.MediaEdge
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		edge, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: n1.ID, TargetNodeID: n2.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.60, Tier: 2, Resolver: "test", ReviewState: "NEEDS_REVIEW",
		})
		return err
	}); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	before, err := database.Reader.GetMediaNodeByID(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get target before confirm: %v", err)
	}
	if before.GraphStatus != "UNLINKED" {
		t.Fatalf("target graph_status before confirm = %q, want UNLINKED", before.GraphStatus)
	}

	rr := doJSON(t, srv.Handler(), http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/confirm", edge.ID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("confirm status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	after, err := database.Reader.GetMediaNodeByID(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get target after confirm: %v", err)
	}
	if after.GraphStatus != "LINKED" {
		t.Errorf("target graph_status after confirm = %q, want LINKED", after.GraphStatus)
	}
}

// TestRejectEdgeRecomputesGraphStatus backs M3: rejecting a node's only
// AUTO_ACCEPTED edge must revert graph_status to UNLINKED, not leave it
// stuck at LINKED with nothing in the audit queue to explain why.
func TestRejectEdgeRecomputesGraphStatus(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	n1, n2 := seedTwoUnlinkedNodes(t, database)

	var edge sqlcgen.MediaEdge
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		edge, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: n1.ID, TargetNodeID: n2.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.95, Tier: 1, Resolver: "test", ReviewState: "AUTO_ACCEPTED",
		})
		if err != nil {
			return err
		}
		// Simulate the graph engine having already set LINKED for this
		// AUTO_ACCEPTED edge, the state a real resolve pass would leave.
		return q.UpdateMediaNodeGraphStatus(ctx, sqlcgen.UpdateMediaNodeGraphStatusParams{ID: n2.ID, GraphStatus: "LINKED"})
	}); err != nil {
		t.Fatalf("seed edge: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/reject", edge.ID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("reject status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	after, err := database.Reader.GetMediaNodeByID(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get target after reject: %v", err)
	}
	if after.GraphStatus != "UNLINKED" {
		t.Errorf("target graph_status after reject = %q, want UNLINKED (its only edge was just rejected)", after.GraphStatus)
	}
}

// TestRejectEdgeRecomputesGraphStatus_MixedEdges verifies that when a node
// has multiple parent candidate edges, rejecting one while another remains
// pending (NEEDS_REVIEW) recomputes graph_status to NEEDS_REVIEW (not UNLINKED).
func TestRejectEdgeRecomputesGraphStatus_MixedEdges(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	var loc sqlcgen.StorageLocation
	var n1, n2, n3 sqlcgen.MediaNode
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		loc, err = q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: t.Name(), RootPath: t.TempDir(), Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		n1, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-" + t.Name() + "-1", FilePath: "/p1-" + t.Name() + ".raw",
			FileName: "p1.raw", FileExt: "raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		n2, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-" + t.Name() + "-2", FilePath: "/target-" + t.Name() + ".jpg",
			FileName: "target.jpg", FileExt: "jpg", IndexingStatus: "INDEXED_FULL", GraphStatus: "LINKED", LifecycleState: "ACTIVE",
		})
		if err != nil {
			return err
		}
		n3, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-" + t.Name() + "-3", FilePath: "/p2-" + t.Name() + ".raw",
			FileName: "p2.raw", FileExt: "raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes: %v", err)
	}

	var edge1, edge2 sqlcgen.MediaEdge
	if err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		edge1, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: n1.ID, TargetNodeID: n2.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.95, Tier: 1, Resolver: "test", ReviewState: "AUTO_ACCEPTED",
		})
		if err != nil {
			return err
		}
		edge2, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: n3.ID, TargetNodeID: n2.ID, RelationshipType: "DERIVED_FROM",
			Confidence: 0.60, Tier: 2, Resolver: "test", ReviewState: "NEEDS_REVIEW",
		})
		return err
	}); err != nil {
		t.Fatalf("seed edges: %v", err)
	}

	// Reject edge1 (the AUTO_ACCEPTED one). Edge2 is still pending (NEEDS_REVIEW).
	rr := doJSON(t, srv.Handler(), http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/reject", edge1.ID), nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("reject status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	after, err := database.Reader.GetMediaNodeByID(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get target after reject: %v", err)
	}
	if after.GraphStatus != "NEEDS_REVIEW" {
		t.Errorf("target graph_status after reject = %q, want NEEDS_REVIEW (edge2 is still pending review)", after.GraphStatus)
	}

	// 2. Reject edge2 as well: all parent candidate edges are now REJECTED -> graph_status reverts to UNLINKED.
	rr2 := doJSON(t, srv.Handler(), http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/reject", edge2.ID), nil)
	if rr2.Code != http.StatusOK {
		t.Fatalf("reject edge2 status = %d, want 200, body = %s", rr2.Code, rr2.Body.String())
	}

	afterAllRejected, err := database.Reader.GetMediaNodeByID(ctx, n2.ID)
	if err != nil {
		t.Fatalf("get target after all rejected: %v", err)
	}
	if afterAllRejected.GraphStatus != "UNLINKED" {
		t.Errorf("target graph_status after all edges rejected = %q, want UNLINKED", afterAllRejected.GraphStatus)
	}
}

func TestStorageHealth(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	tmpDir := t.TempDir()
	invalidDir := filepath.Join(tmpDir, "nonexistent-mount-path")

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc1, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "valid", RootPath: tmpDir, Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 1,
		})
		if err != nil {
			return err
		}
		_, err = q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "invalid", RootPath: invalidDir, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: 1, Prunable: 0,
		})
		if err != nil {
			return err
		}
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc1.ID, NodeUuid: "uuid-s1", FilePath: filepath.Join(tmpDir, "f1.raw"), FileName: "f1.raw", FileExt: ".raw", IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed storage health test data: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/storage-health", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/storage-health status = %d, want 200, body = %s", rr.Code, rr.Body.String())
	}

	var got struct {
		Locations []struct {
			Name            string  `json:"name"`
			NodeCount       int64   `json:"nodeCount"`
			TotalBytes      uint64  `json:"totalBytes"`
			IsDegraded      bool    `json:"isDegraded"`
			DegradedMessage *string `json:"degradedMessage"`
		} `json:"locations"`
		Queues struct {
			WorkerPoolCapacity int   `json:"workerPoolCapacity"`
			RunningScanJobs    int64 `json:"runningScanJobs"`
		} `json:"queues"`
	}

	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal storage health response: %v", err)
	}

	if len(got.Locations) != 2 {
		t.Fatalf("got %d locations, want 2", len(got.Locations))
	}

	var validLoc, invalidLoc *struct {
		Name            string  `json:"name"`
		NodeCount       int64   `json:"nodeCount"`
		TotalBytes      uint64  `json:"totalBytes"`
		IsDegraded      bool    `json:"isDegraded"`
		DegradedMessage *string `json:"degradedMessage"`
	}

	for i := range got.Locations {
		switch got.Locations[i].Name {
		case "valid":
			validLoc = &got.Locations[i]
		case "invalid":
			invalidLoc = &got.Locations[i]
		}
	}

	if validLoc == nil || invalidLoc == nil {
		t.Fatalf("locations missing valid or invalid entry: %+v", got.Locations)
	}

	if validLoc.IsDegraded || validLoc.NodeCount != 1 || validLoc.TotalBytes == 0 {
		t.Errorf("valid location unexpected state: %+v", validLoc)
	}

	if !invalidLoc.IsDegraded || invalidLoc.DegradedMessage == nil {
		t.Errorf("invalid location should be degraded: %+v", invalidLoc)
	}

	if got.Queues.WorkerPoolCapacity != 16 {
		t.Errorf("got WorkerPoolCapacity = %d, want 16", got.Queues.WorkerPoolCapacity)
	}

	if got.Queues.RunningScanJobs != 0 {
		t.Errorf("got RunningScanJobs = %d, want 0", got.Queues.RunningScanJobs)
	}

	// Verify server with nil pool handles request cleanly
	nilPoolServer := New(Deps{DB: database})
	rrNil := doJSON(t, nilPoolServer.Handler(), http.MethodGet, "/api/v1/storage-health", nil)
	if rrNil.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/storage-health nil pool status = %d, want 200", rrNil.Code)
	}
}

// TestStatfsWithTimeoutSucceedsOnFastPath is statfsWithTimeout's happy
// path: a real, existing directory with a generous timeout must behave
// exactly like a direct unix.Statfs call.
func TestStatfsWithTimeoutSucceedsOnFastPath(t *testing.T) {
	stat, err := statfsWithTimeout(t.TempDir(), 5*time.Second)
	if err != nil {
		t.Fatalf("statfsWithTimeout: %v", err)
	}
	if stat.Blocks == 0 {
		t.Error("stat.Blocks = 0, want a real filesystem stat for an existing directory")
	}
}

// TestStatfsWithTimeoutReturnsErrorWhenExceeded backs M4: a hung NFS/SMB
// mount must degrade the location, not block the whole /api/v1/
// storage-health response indefinitely. Statfs itself can't be reproduced
// hanging in a portable unit test (it would need a genuinely wedged
// mount), so this uses statfsWithTimeoutFn to substitute a stub that
// blocks forever, forcing the timeout branch deterministically against a
// real, non-degenerate timeout -- an earlier version of this test raced a
// 1ns timeout against the real syscall instead, which was flaky in CI: a
// fast enough real Statfs call occasionally won that race, so the test
// would flip-flop between exercising the timeout branch and the success
// branch depending on runner load, not the code under test.
func TestStatfsWithTimeoutReturnsErrorWhenExceeded(t *testing.T) {
	blockForever := func(_ string, _ *unix.Statfs_t) error {
		select {}
	}
	_, err := statfsWithTimeoutFn(blockForever, "/irrelevant", 20*time.Millisecond)
	if err == nil {
		t.Fatal("statfsWithTimeoutFn with a permanently-blocked syscall returned nil error, want a timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Errorf("err = %q, want it to mention a timeout", err.Error())
	}
}

func TestFilteredAssetsAndFacets(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "scratch", RootPath: "/scratch", Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 1,
		})
		if err != nil {
			return err
		}
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-c1", FilePath: "/scratch/c1.jpg", FileName: "c1.jpg", FileExt: ".jpg",
			IndexingStatus: "INDEXED_FULL", GraphStatus: "UNLINKED", LifecycleState: "ACTIVE", CameraModel: sql.NullString{String: "Sony A7IV", Valid: true},
		})
		if err != nil {
			return err
		}
		_, err = q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			StorageLocationID: loc.ID, NodeUuid: "uuid-c2", FilePath: "/scratch/c2.jpg", FileName: "c2.jpg", FileExt: ".jpg",
			IndexingStatus: "INDEXED_FULL", GraphStatus: "LINKED", LifecycleState: "ACTIVE", CameraModel: sql.NullString{String: "Canon R5", Valid: true},
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed test nodes: %v", err)
	}

	// 1. Facets endpoint
	rrFacets := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/facets", nil)
	if rrFacets.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/assets/facets status = %d, want 200", rrFacets.Code)
	}
	var facets struct {
		CameraModels []string `json:"cameraModels"`
	}
	if err := json.Unmarshal(rrFacets.Body.Bytes(), &facets); err != nil {
		t.Fatalf("unmarshal facets: %v", err)
	}
	if len(facets.CameraModels) != 2 {
		t.Errorf("got %d camera models, want 2", len(facets.CameraModels))
	}

	// 2. Filtered assets: unlinkedOnly=true
	rrUnlinked := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets?unlinkedOnly=true", nil)
	if rrUnlinked.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/assets?unlinkedOnly=true status = %d, want 200", rrUnlinked.Code)
	}
	var unlinkedRes struct {
		Assets []assetDTO `json:"assets"`
		Total  int64      `json:"total"`
	}
	if err := json.Unmarshal(rrUnlinked.Body.Bytes(), &unlinkedRes); err != nil {
		t.Fatalf("unmarshal unlinked res: %v", err)
	}
	if len(unlinkedRes.Assets) != 1 || unlinkedRes.Total != 1 {
		t.Errorf("unlinked filter got %d assets (total %d), want 1", len(unlinkedRes.Assets), unlinkedRes.Total)
	}

	// 3. Filtered assets: cameraModel=Canon R5
	rrCamera := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets?cameraModel=Canon+R5", nil)
	if rrCamera.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/assets?cameraModel=Canon+R5 status = %d, want 200", rrCamera.Code)
	}
	var cameraRes struct {
		Assets []assetDTO `json:"assets"`
		Total  int64      `json:"total"`
	}
	if err := json.Unmarshal(rrCamera.Body.Bytes(), &cameraRes); err != nil {
		t.Fatalf("unmarshal camera res: %v", err)
	}
	if len(cameraRes.Assets) != 1 || cameraRes.Assets[0].CameraModel != "Canon R5" {
		t.Errorf("camera filter got %+v, want Canon R5", cameraRes.Assets)
	}
}

func TestListJobsFiltered(t *testing.T) {
	srv, database := fullTestServer(t)
	ctx := context.Background()

	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(ctx, sqlcgen.CreateStorageLocationParams{
			Name: "loc", RootPath: "/loc", Tier: "TIER1_LOCAL_SCRATCH", ReadOnly: 0, Prunable: 1,
		})
		if err != nil {
			return err
		}
		_, err = q.CreateScanJob(ctx, sqlcgen.CreateScanJobParams{
			StorageLocationID: sql.NullInt64{Int64: loc.ID, Valid: true}, Kind: "FULL_SCAN",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed scan job: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/jobs?kind=FULL_SCAN", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/jobs status = %d, want 200", rr.Code)
	}

	var res struct {
		Jobs  []scanJobDTO `json:"jobs"`
		Total int64        `json:"total"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal jobs res: %v", err)
	}

	if len(res.Jobs) != 1 || res.Total != 1 || res.Jobs[0].Kind != "FULL_SCAN" {
		t.Errorf("jobs filter got %+v (total %d), want 1 FULL_SCAN job", res.Jobs, res.Total)
	}
}

func toStr(v int64) string {
	return fmt.Sprintf("%d", v)
}

func serverWithGuard(t *testing.T) (*Server, *db.DB, *storage.Guard, string, string, string) {
	t.Helper()
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	exports := filepath.Join(root, "exports")
	archive := filepath.Join(root, "archive")
	for _, d := range []string{staging, exports, archive} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}

	resStaging, _ := filepath.EvalSymlinks(staging)
	resExports, _ := filepath.EvalSymlinks(exports)
	resArchive, _ := filepath.EvalSymlinks(archive)

	dbPath := filepath.Join(root, "routes.db")
	database, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()
	var loc1, loc2, loc3 sqlcgen.StorageLocation
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		loc1, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "staging", RootPath: resStaging, Tier: "TIER0_LOCAL_STAGING", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		loc2, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "exports", RootPath: resExports, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		loc3, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "archive", RootPath: resArchive, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: 1, Prunable: 0,
		})
		return err
	})
	if err != nil {
		t.Fatalf("upsert locations: %v", err)
	}

	guard := storage.NewGuard([]storage.Location{
		{ID: loc1.ID, Name: "staging", RootPath: resStaging, Tier: "TIER0_LOCAL_STAGING", ReadOnly: false},
		{ID: loc2.ID, Name: "exports", RootPath: resExports, Tier: "TIER2_EXPORTS", ReadOnly: false},
		{ID: loc3.ID, Name: "archive", RootPath: resArchive, Tier: "TIER3_MASTER_ARCHIVE", ReadOnly: true},
	})

	srv := New(Deps{
		Config:  &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:      database,
		Guard:   guard,
		Prober:  probe.New(),
		Engine:  graph.NewEngine(database, nil),
		Hub:     sse.New(),
		Version: "test",
	})

	return srv, database, guard, resStaging, resExports, resArchive
}

func TestAgentHandshake_Success_And_Auth(t *testing.T) {
	srv, database, _, _, _, _ := serverWithGuard(t)
	ctx := context.Background()

	// Enqueue and mark an event processed for agent-alpha
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		ev, err := q.EnqueueAgentEvent(ctx, sqlcgen.EnqueueAgentEventParams{
			EventUuid:   "018f0000-0000-7000-8000-000000000001",
			AgentID:     "agent-alpha",
			EventType:   "EVENT_NODE_CREATED",
			PayloadJson: `{"filePath":"/test.raw"}`,
		})
		if err != nil {
			return err
		}
		return q.MarkAgentEventProcessed(ctx, ev.ID)
	})
	if err != nil {
		t.Fatalf("setup event: %v", err)
	}

	// 1. Missing auth header -> 401
	reqNoAuth := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytesOfJSON(t, map[string]string{
		"agentId": "agent-alpha",
	}))
	reqNoAuth.Header.Set("Content-Type", "application/json")
	rrNoAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrNoAuth, reqNoAuth)
	if rrNoAuth.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 Unauthorized", rrNoAuth.Code)
	}

	// 2. Valid handshake request -> 200 OK with server version and acknowledged event
	reqAuth := httptest.NewRequest(http.MethodPost, "/api/v1/agent/handshake", bytesOfJSON(t, map[string]string{
		"agentId":       "agent-alpha",
		"clientVersion": "0.1.0",
	}))
	reqAuth.Header.Set("Content-Type", "application/json")
	reqAuth.Header.Set("X-API-Key", routeTestAgentKey)
	rrAuth := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrAuth, reqAuth)

	if rrAuth.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200, body = %s", rrAuth.Code, rrAuth.Body.String())
	}

	var res struct {
		OK                    bool   `json:"ok"`
		ServerVersion         string `json:"serverVersion"`
		ServerTimeUnix        int64  `json:"serverTimeUnix"`
		AcknowledgedEventUUID string `json:"acknowledgedEventUuid"`
		PendingEventsCount    int64  `json:"pendingEventsCount"`
	}
	if err := json.Unmarshal(rrAuth.Body.Bytes(), &res); err != nil {
		t.Fatalf("unmarshal handshake response: %v", err)
	}

	if !res.OK || res.ServerVersion != "test" || res.AcknowledgedEventUUID != "018f0000-0000-7000-8000-000000000001" {
		t.Errorf("unexpected handshake response: %+v", res)
	}
}

func TestAgentRebase_Known_And_Unknown_Nodes_And_Tier3_Safety(t *testing.T) {
	srv, database, _, staging, exports, archive := serverWithGuard(t)
	ctx := context.Background()

	// Insert existing node on staging with an edge
	knownUUID := "018f0000-0000-7000-8000-000000000010"
	childUUID := "018f0000-0000-7000-8000-000000000011"
	stagingPath := filepath.Join(staging, "clip_orig.mov")

	var parentID, childID int64
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		p, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          knownUUID,
			StorageLocationID: 1,
			FilePath:          stagingPath,
			FileName:          "clip_orig.mov",
			LifecycleState:    "ACTIVE",
			GraphStatus:       "UNLINKED",
			IndexingStatus:    "INDEXED_SHALLOW",
		})
		if err != nil {
			return err
		}
		parentID = p.ID

		c, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          childUUID,
			StorageLocationID: 1,
			FilePath:          filepath.Join(staging, "clip_proxy.mov"),
			FileName:          "clip_proxy.mov",
			LifecycleState:    "ACTIVE",
			GraphStatus:       "UNLINKED",
			IndexingStatus:    "INDEXED_SHALLOW",
		})
		if err != nil {
			return err
		}
		childID = c.ID

		_, err = q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID:     parentID,
			TargetNodeID:     childID,
			RelationshipType: "PROXY_OF",
			Confidence:       1.0,
			Tier:             1,
			Resolver:         "manual",
			EvidenceJson:     "{}",
			ReviewState:      "AUTO_ACCEPTED",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed nodes & edge: %v", err)
	}

	// 1. Rebase known node from staging to exports -> preserves node ID and edges
	rebasedExportPath := filepath.Join(exports, "clip_final.mov")
	reqRebaseKnown := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid":   knownUUID,
		"targetPath": rebasedExportPath,
		"fileName":   "clip_final.mov",
	}))
	reqRebaseKnown.Header.Set("Content-Type", "application/json")
	reqRebaseKnown.Header.Set("X-API-Key", routeTestAgentKey)
	rrRebaseKnown := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrRebaseKnown, reqRebaseKnown)

	if rrRebaseKnown.Code != http.StatusOK {
		t.Fatalf("rebase known status = %d, body = %s", rrRebaseKnown.Code, rrRebaseKnown.Body.String())
	}
	var outKnown struct {
		ID       int64  `json:"id"`
		NodeUUID string `json:"nodeUuid"`
		FilePath string `json:"filePath"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(rrRebaseKnown.Body.Bytes(), &outKnown); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if outKnown.ID != parentID || outKnown.Status != "REBASED" || outKnown.FilePath != rebasedExportPath {
		t.Errorf("rebase known unexpected: %+v, want ID=%d status=REBASED", outKnown, parentID)
	}

	// Verify edges survived untouched
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		edges, err := q.ListEdgesBySource(ctx, parentID)
		if err != nil {
			return err
		}
		if len(edges) != 1 || edges[0].TargetNodeID != childID {
			t.Errorf("edges after rebase = %+v, want 1 edge to childID %d", edges, childID)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("check edges: %v", err)
	}

	// 2. Rebase unknown node -> creates new node (agent is source of truth)
	unknownUUID := "018f0000-0000-7000-8000-000000000099"
	unknownExportPath := filepath.Join(exports, "staged_offline.arw")
	reqRebaseUnknown := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid":   unknownUUID,
		"targetPath": unknownExportPath,
		"fileName":   "staged_offline.arw",
		"fileExt":    ".arw",
		"sizeBytes":  2048,
	}))
	reqRebaseUnknown.Header.Set("Content-Type", "application/json")
	reqRebaseUnknown.Header.Set("X-API-Key", routeTestAgentKey)
	rrRebaseUnknown := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrRebaseUnknown, reqRebaseUnknown)

	if rrRebaseUnknown.Code != http.StatusOK {
		t.Fatalf("rebase unknown status = %d, body = %s", rrRebaseUnknown.Code, rrRebaseUnknown.Body.String())
	}
	var outUnknown struct {
		ID       int64  `json:"id"`
		NodeUUID string `json:"nodeUuid"`
		FilePath string `json:"filePath"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(rrRebaseUnknown.Body.Bytes(), &outUnknown); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if outUnknown.Status != "CREATED" || outUnknown.NodeUUID != unknownUUID {
		t.Errorf("rebase unknown unexpected: %+v, want status=CREATED", outUnknown)
	}

	// 3. Rebase target resolving to Tier 3 (read-only archive) -> Refused!
	tier3Path := filepath.Join(archive, "forbidden.raw")
	reqTier3 := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid":   knownUUID,
		"targetPath": tier3Path,
	}))
	reqTier3.Header.Set("Content-Type", "application/json")
	reqTier3.Header.Set("X-API-Key", routeTestAgentKey)
	rrTier3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rrTier3, reqTier3)

	if rrTier3.Code != http.StatusBadRequest {
		t.Fatalf("rebase to tier 3 status = %d, want 400 Bad Request", rrTier3.Code)
	}
}

// TestAgentRebase_Spec9Scenario_LocalStagingToCentralTier3 is the spec
// §9-named test issue #58 quoted but did not implement, resolved by issue
// #167: "path rebasing when an offline node's location changes from
// LOCAL_STAGING to CENTRAL_TIER3". Once the workstation agent has already
// copied the node's bytes into the archive, POST /api/v1/agent/rebase must
// succeed -- this call only records the new location in the database, it
// never writes to Tier 3 itself.
func TestAgentRebase_Spec9Scenario_LocalStagingToCentralTier3(t *testing.T) {
	srv, database, _, staging, _, archive := serverWithGuard(t)
	ctx := context.Background()

	nodeUUID := "018f0000-0000-7000-8000-0000000000bb"
	stagingPath := filepath.Join(staging, "master.raw")
	var nodeID int64
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          nodeUUID,
			StorageLocationID: 1,
			FilePath:          stagingPath,
			FileName:          "master.raw",
			LifecycleState:    "ACTIVE",
			GraphStatus:       "UNLINKED",
			IndexingStatus:    "INDEXED_SHALLOW",
		})
		nodeID = n.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}

	// The workstation agent has already copied the bytes into the archive.
	tier3Path := filepath.Join(archive, "master.raw")
	if err := os.WriteFile(tier3Path, []byte("archived bytes"), 0o644); err != nil {
		t.Fatalf("seed archived file: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid":   nodeUUID,
		"targetPath": tier3Path,
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("rebase LOCAL_STAGING -> CENTRAL_TIER3 status = %d, want 200 OK, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		ID       int64  `json:"id"`
		NodeUUID string `json:"nodeUuid"`
		FilePath string `json:"filePath"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ID != nodeID || out.Status != "REBASED" || out.FilePath != tier3Path {
		t.Errorf("rebase to Tier 3 unexpected: %+v, want ID=%d status=REBASED filePath=%q", out, nodeID, tier3Path)
	}

	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		node, err := q.GetMediaNodeByUUID(ctx, nodeUUID)
		if err != nil {
			return err
		}
		if node.FilePath != tier3Path {
			t.Errorf("node file_path = %q, want %q", node.FilePath, tier3Path)
		}
		if node.LifecycleState != "ACTIVE" {
			t.Errorf("node lifecycle_state = %q, want ACTIVE", node.LifecycleState)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify rebased node: %v", err)
	}
}

// seedSyncLocation creates a storage location for seeding nodes.
func seedSyncLocation(t *testing.T, database *db.DB) int64 {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatalf("resolve root: %v", err)
	}
	var locID int64
	err = database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		loc, err := q.CreateStorageLocation(context.Background(), sqlcgen.CreateStorageLocationParams{
			Name: "sync-loc", RootPath: root, Tier: "TIER2_EXPORTS", ReadOnly: 0, Prunable: 0,
		})
		locID = loc.ID
		return err
	})
	if err != nil {
		t.Fatalf("seed location: %v", err)
	}
	return locID
}

// seedSyncState inserts a remote_sync_state row via UpsertRemoteSyncState (the
// only sanctioned row-creation path). Returns the upserted row.
func seedSyncState(t *testing.T, database *db.DB, nodeID int64, remote, status string, lastError string, lastAttemptAt int64) sqlcgen.RemoteSyncState {
	t.Helper()
	var row sqlcgen.RemoteSyncState
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		var errStr sql.NullString
		if lastError != "" {
			errStr = sql.NullString{String: lastError, Valid: true}
		}
		var attempt sql.NullInt64
		if lastAttemptAt != 0 {
			attempt = sql.NullInt64{Int64: lastAttemptAt, Valid: true}
		}
		var err error
		row, err = q.UpsertRemoteSyncState(context.Background(), sqlcgen.UpsertRemoteSyncStateParams{
			NodeID: nodeID, Remote: remote, SyncStatus: status,
			RemoteAssetID: sql.NullString{}, LastError: errStr, LastAttemptAt: attempt,
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed sync state: %v", err)
	}
	return row
}

func TestAssetSyncStatus(t *testing.T) {
	srv, database := fullTestServer(t)
	locID := seedSyncLocation(t, database)
	node := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "asset.jpg"), "uuid-sync-status")

	now := time.Now().Unix()
	seedSyncState(t, database, node.ID, "IMMICH", "PUSH_FAILED", "boom: 500", now)
	seedSyncState(t, database, node.ID, "GOOGLE_PHOTOS", "PUSHED", "", now)

	t.Run("returns both remotes with expected fields", func(t *testing.T) {
		rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/"+fmt.Sprint(node.ID)+"/sync-status", nil)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
		}
		var got struct {
			Sync []syncStateDTO `json:"sync"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if len(got.Sync) != 2 {
			t.Fatalf("got %d sync rows, want 2: %+v", len(got.Sync), got.Sync)
		}
		byRemote := map[string]syncStateDTO{}
		for _, r := range got.Sync {
			byRemote[r.Remote] = r
		}
		immich, ok := byRemote["IMMICH"]
		if !ok {
			t.Fatalf("missing IMMICH row: %+v", byRemote)
		}
		if immich.SyncStatus != "PUSH_FAILED" {
			t.Errorf("IMMICH syncStatus = %q, want PUSH_FAILED", immich.SyncStatus)
		}
		if immich.LastError == nil || *immich.LastError != "boom: 500" {
			t.Errorf("IMMICH lastError = %v, want boom: 500", immich.LastError)
		}
		if immich.LastAttemptAt == nil || *immich.LastAttemptAt != now {
			t.Errorf("IMMICH lastAttemptAt = %v, want %d", immich.LastAttemptAt, now)
		}

		gp, ok := byRemote["GOOGLE_PHOTOS"]
		if !ok {
			t.Fatalf("missing GOOGLE_PHOTOS row: %+v", byRemote)
		}
		if gp.SyncStatus != "PUSHED" {
			t.Errorf("GOOGLE_PHOTOS syncStatus = %q, want PUSHED", gp.SyncStatus)
		}
		if gp.LastError != nil {
			t.Errorf("GOOGLE_PHOTOS lastError = %v, want nil", gp.LastError)
		}
	})

	t.Run("surfaces retryCount and exhausted once a row hits the retry bound", func(t *testing.T) {
		below := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "below-bound.jpg"), "uuid-sync-status-below")
		atBound := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "at-bound.jpg"), "uuid-sync-status-at-bound")
		seedSyncState(t, database, below.ID, "IMMICH", "PUSH_FAILED", "boom", now)
		seedSyncState(t, database, atBound.ID, "IMMICH", "PUSH_FAILED", "boom", now)

		markFailed := func(t *testing.T, nodeID int64, times int) {
			t.Helper()
			for i := 0; i < times; i++ {
				if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
					return q.MarkRemoteSyncStateFailed(context.Background(), sqlcgen.MarkRemoteSyncStateFailedParams{
						NodeID: nodeID, Remote: "IMMICH", LastError: sql.NullString{String: "boom", Valid: true},
					})
				}); err != nil {
					t.Fatalf("mark failed (%d): %v", i, err)
				}
			}
		}
		markFailed(t, below.ID, sync.DefaultMaxSyncRetries-1)
		markFailed(t, atBound.ID, sync.DefaultMaxSyncRetries)

		get := func(t *testing.T, nodeID int64) syncStateDTO {
			t.Helper()
			rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/"+fmt.Sprint(nodeID)+"/sync-status", nil)
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
			}
			var got struct {
				Sync []syncStateDTO `json:"sync"`
			}
			if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			for _, r := range got.Sync {
				if r.Remote == "IMMICH" {
					return r
				}
			}
			t.Fatalf("missing IMMICH row: %+v", got.Sync)
			return syncStateDTO{}
		}

		belowDTO := get(t, below.ID)
		if belowDTO.RetryCount != int64(sync.DefaultMaxSyncRetries-1) {
			t.Errorf("below-bound retryCount = %d, want %d", belowDTO.RetryCount, sync.DefaultMaxSyncRetries-1)
		}
		if belowDTO.Exhausted {
			t.Errorf("below-bound exhausted = true, want false")
		}

		atBoundDTO := get(t, atBound.ID)
		if atBoundDTO.RetryCount != int64(sync.DefaultMaxSyncRetries) {
			t.Errorf("at-bound retryCount = %d, want %d", atBoundDTO.RetryCount, sync.DefaultMaxSyncRetries)
		}
		if !atBoundDTO.Exhausted {
			t.Errorf("at-bound exhausted = false, want true")
		}
	})

	t.Run("unknown asset 404s", func(t *testing.T) {
		rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/999999/sync-status", nil)
		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", rr.Code)
		}
	})

	// Finding K: handleSyncRetry and handleInheritMetadata both 404 an
	// ARCHIVED node; handleAssetSyncStatus previously didn't, despite the
	// three handlers sitting adjacent.
	t.Run("ARCHIVED asset 404s", func(t *testing.T) {
		archived := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "archived-status.jpg"), "uuid-sync-status-archived")
		if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			return q.ArchiveMediaNode(context.Background(), archived.ID)
		}); err != nil {
			t.Fatalf("archive node: %v", err)
		}
		rr := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/"+fmt.Sprint(archived.ID)+"/sync-status", nil)
		if rr.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404 for ARCHIVED node", rr.Code)
		}
	})
}

func TestSyncRetryRequeuesFailed(t *testing.T) {
	srv, database := fullTestServer(t)
	locID := seedSyncLocation(t, database)
	node := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "retry.jpg"), "uuid-sync-retry")

	now := time.Now().Unix()
	seedSyncState(t, database, node.ID, "IMMICH", "PUSH_FAILED", "boom: 500", now)
	seedSyncState(t, database, node.ID, "GOOGLE_PHOTOS", "PUSHED", "", now)

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(node.ID)+"/sync/retry", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Requeued int64 `json:"requeued"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Requeued != 1 {
		t.Errorf("requeued = %d, want 1 (only the PUSH_FAILED row)", out.Requeued)
	}

	rows, err := database.Reader.ListRemoteSyncStateByNode(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("list sync state: %v", err)
	}
	byRemote := map[string]sqlcgen.RemoteSyncState{}
	for _, r := range rows {
		byRemote[r.Remote] = r
	}
	immich, ok := byRemote["IMMICH"]
	if !ok {
		t.Fatalf("missing IMMICH row after retry: %+v", byRemote)
	}
	if immich.SyncStatus != "PENDING_CLOUD_PUSH" {
		t.Errorf("IMMICH syncStatus after retry = %q, want PENDING_CLOUD_PUSH", immich.SyncStatus)
	}
	if immich.LastError.Valid {
		t.Errorf("IMMICH lastError after retry = %q, want cleared", immich.LastError.String)
	}
	if gp, ok := byRemote["GOOGLE_PHOTOS"]; !ok || gp.SyncStatus != "PUSHED" {
		t.Errorf("GOOGLE_PHOTOS should stay PUSHED and uncounted: %+v", gp)
	}
}

// TestSyncRetryResetsRetryCount covers the gap an independent review of #200
// found: a manual retry must restore a full automatic-retry budget, not just
// buy one more attempt before immediately re-hitting
// ResetRemoteSyncStateFailed's bound on the very next failure.
func TestSyncRetryResetsRetryCount(t *testing.T) {
	srv, database := fullTestServer(t)
	locID := seedSyncLocation(t, database)
	node := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "retry-count.jpg"), "uuid-sync-retry-count")

	now := time.Now().Unix()
	seedSyncState(t, database, node.ID, "IMMICH", "PUSH_FAILED", "boom", now)
	// Drive retry_count above 0 via the real failure transition, mirroring
	// internal/sync's own tests -- not a direct column poke.
	for i := 0; i < 3; i++ {
		if err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
			return q.MarkRemoteSyncStateFailed(context.Background(), sqlcgen.MarkRemoteSyncStateFailedParams{
				NodeID: node.ID, Remote: "IMMICH", LastError: sql.NullString{String: "boom", Valid: true},
			})
		}); err != nil {
			t.Fatalf("mark failed (%d): %v", i, err)
		}
	}
	before, err := database.Reader.GetRemoteSyncState(context.Background(), sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: "IMMICH"})
	if err != nil {
		t.Fatalf("get before retry: %v", err)
	}
	if before.RetryCount != 3 {
		t.Fatalf("test setup: retry_count = %d, want 3", before.RetryCount)
	}

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(node.ID)+"/sync/retry", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	after, err := database.Reader.GetRemoteSyncState(context.Background(), sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: "IMMICH"})
	if err != nil {
		t.Fatalf("get after retry: %v", err)
	}
	if after.RetryCount != 0 {
		t.Errorf("retry_count after manual retry = %d, want 0 (operator's fix must restore a full retry budget)", after.RetryCount)
	}
	if after.SyncStatus != "PENDING_CLOUD_PUSH" {
		t.Errorf("sync_status after manual retry = %q, want PENDING_CLOUD_PUSH", after.SyncStatus)
	}
}

// TestSyncRetryLeavesLastAttemptAtUntouched covers finding K: a manual retry
// must not bump last_attempt_at, or ListRemoteSyncStateByStatus's
// oldest-attempt-first claim ordering sends the row an operator just
// explicitly asked to retry to the BACK of the queue -- behind every row the
// automatic ResetRemoteSyncStateFailed recovery already reset (which leaves
// last_attempt_at alone). This is the property that makes the two retry
// paths for the same transition agree.
func TestSyncRetryLeavesLastAttemptAtUntouched(t *testing.T) {
	srv, database := fullTestServer(t)
	locID := seedSyncLocation(t, database)
	node := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "retry-attempt-at.jpg"), "uuid-sync-retry-attempt-at")

	oldAttempt := time.Now().Add(-1 * time.Hour).Unix()
	seedSyncState(t, database, node.ID, "IMMICH", "PUSH_FAILED", "boom", oldAttempt)

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(node.ID)+"/sync/retry", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}

	after, err := database.Reader.GetRemoteSyncState(context.Background(), sqlcgen.GetRemoteSyncStateParams{NodeID: node.ID, Remote: "IMMICH"})
	if err != nil {
		t.Fatalf("get after retry: %v", err)
	}
	if !after.LastAttemptAt.Valid || after.LastAttemptAt.Int64 != oldAttempt {
		t.Errorf("last_attempt_at after manual retry = %+v, want unchanged at %d -- a manual retry must claim promptly, not sort behind rows the automatic recovery already reset",
			after.LastAttemptAt, oldAttempt)
	}
	if after.SyncStatus != "PENDING_CLOUD_PUSH" {
		t.Errorf("sync_status after manual retry = %q, want PENDING_CLOUD_PUSH", after.SyncStatus)
	}
}

func TestSyncRetrySkipsInFlightAndQueued(t *testing.T) {
	// A PUSHING (in-flight) or PENDING_CLOUD_PUSH (already queued) row must
	// never be regressed by a manual retry -- only PUSH_FAILED rows are
	// re-claimed. The remote CHECK constraint allows only IMMICH /
	// GOOGLE_PHOTOS, so use a separate node to hold these statuses.
	srv, database := fullTestServer(t)
	locID := seedSyncLocation(t, database)
	node := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "inflight.jpg"), "uuid-sync-inflight")

	now := time.Now().Unix()
	seedSyncState(t, database, node.ID, "IMMICH", "PUSHING", "", now)
	seedSyncState(t, database, node.ID, "GOOGLE_PHOTOS", "PENDING_CLOUD_PUSH", "", now)

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(node.ID)+"/sync/retry", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Requeued int64 `json:"requeued"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Requeued != 0 {
		t.Errorf("requeued = %d, want 0 (PUSHING/PENDING rows are never re-claimed)", out.Requeued)
	}

	rows, err := database.Reader.ListRemoteSyncStateByNode(context.Background(), node.ID)
	if err != nil {
		t.Fatalf("list sync state: %v", err)
	}
	byRemote := map[string]sqlcgen.RemoteSyncState{}
	for _, r := range rows {
		byRemote[r.Remote] = r
	}
	if imm, ok := byRemote["IMMICH"]; !ok || imm.SyncStatus != "PUSHING" {
		t.Errorf("IMMICH should stay PUSHING (in flight): %+v", imm)
	}
	if gp, ok := byRemote["GOOGLE_PHOTOS"]; !ok || gp.SyncStatus != "PENDING_CLOUD_PUSH" {
		t.Errorf("GOOGLE_PHOTOS should stay PENDING_CLOUD_PUSH (already queued): %+v", gp)
	}
}

func TestSyncRetryArchived404(t *testing.T) {
	srv, database := fullTestServer(t)
	locID := seedSyncLocation(t, database)
	node := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "archived.jpg"), "uuid-sync-archived")
	err := database.InTx(context.Background(), func(q *sqlcgen.Queries) error {
		return q.ArchiveMediaNode(context.Background(), node.ID)
	})
	if err != nil {
		t.Fatalf("archive node: %v", err)
	}

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(node.ID)+"/sync/retry", nil)
	if rr.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for ARCHIVED node", rr.Code)
	}
}

func TestSyncRetryNoRowsRequeuesZero(t *testing.T) {
	srv, database := fullTestServer(t)
	locID := seedSyncLocation(t, database)
	node := seedInheritNode(t, database, locID, filepath.Join(t.TempDir(), "norows.jpg"), "uuid-sync-norows")

	rr := doJSON(t, srv.Handler(), http.MethodPost, "/api/v1/assets/"+fmt.Sprint(node.ID)+"/sync/retry", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var out struct {
		Requeued int64 `json:"requeued"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Requeued != 0 {
		t.Errorf("requeued = %d, want 0 for a node with no remote_sync_state rows", out.Requeued)
	}

	rrGet := doJSON(t, srv.Handler(), http.MethodGet, "/api/v1/assets/"+fmt.Sprint(node.ID)+"/sync-status", nil)
	if rrGet.Code != http.StatusOK {
		t.Fatalf("sync-status status = %d, body = %s", rrGet.Code, rrGet.Body.String())
	}
	var got struct {
		Sync []syncStateDTO `json:"sync"`
	}
	if err := json.Unmarshal(rrGet.Body.Bytes(), &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Sync == nil || len(got.Sync) != 0 {
		t.Errorf("sync array = %+v, want empty", got.Sync)
	}
}

func TestSyncRetryRequiresAdmin(t *testing.T) {
	srv, _ := fullTestServer(t)
	srv.cfg.Authz.Groups = []string{"dam-admins"}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/assets/1/sync/retry", bytes.NewBufferString("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Authentik-Username", "alice")
	req.Header.Set("X-Authentik-Groups", "dam-users")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for non-admin on sync/retry", rr.Code)
	}
}

func TestPickWinningParentTieBreaksByID(t *testing.T) {
	edges := []sqlcgen.MediaEdge{
		{ID: 5, Confidence: 0.9, ReviewState: "AUTO_ACCEPTED", RelationshipType: "DERIVED_FROM"},
		{ID: 3, Confidence: 0.9, ReviewState: "CONFIRMED", RelationshipType: "DERIVED_FROM"},
		{ID: 7, Confidence: 0.9, ReviewState: "REJECTED", RelationshipType: "DERIVED_FROM"},
		{ID: 9, Confidence: 0.9, ReviewState: "NEEDS_REVIEW", RelationshipType: "DERIVED_FROM"},
		{ID: 2, Confidence: 0.99, ReviewState: "REJECTED", RelationshipType: "DERIVED_FROM"},
	}

	got := pickWinningParent(edges)
	if got == nil {
		t.Fatal("pickWinningParent = nil, want the lowest-ID eligible edge")
	}
	if got.ID != 3 {
		t.Errorf("winning edge id = %d, want 3 (id 2's 0.99 REJECTED edge must be excluded by the review_state filter, not the tie-break)", got.ID)
	}
	if got.ReviewState != "CONFIRMED" {
		t.Errorf("winning review state = %q, want CONFIRMED", got.ReviewState)
	}

	edges = append(edges, sqlcgen.MediaEdge{ID: 1, Confidence: 0.95, ReviewState: "AUTO_ACCEPTED", RelationshipType: "DERIVED_FROM"})
	got = pickWinningParent(edges)
	if got == nil {
		t.Fatal("pickWinningParent = nil, want the higher-confidence edge")
	}
	if got.ID != 1 {
		t.Errorf("winning edge id = %d, want 1 (higher confidence beats the tie-break)", got.ID)
	}
}

// TestPickWinningParentExcludesIneligibleCandidates covers findings B: a
// Tier-3 (heuristic) edge and a DUPLICATE_OF/PROJECT_SIDECAR edge are never
// eligible identity sources, regardless of confidence -- and a lower-
// confidence valid Tier-1/2 ancestry edge must win over them, not be
// shadowed by them.
func TestPickWinningParentExcludesIneligibleCandidates(t *testing.T) {
	t.Run("tier-3 alone is not eligible", func(t *testing.T) {
		edges := []sqlcgen.MediaEdge{
			{ID: 1, Confidence: 0.99, ReviewState: "AUTO_ACCEPTED", RelationshipType: "DERIVED_FROM", Tier: 3},
		}
		if got := pickWinningParent(edges); got != nil {
			t.Errorf("pickWinningParent = %+v, want nil (Tier-3 is never an identity source)", got)
		}
		if !hasResolvedButIneligibleParent(edges) {
			t.Error("hasResolvedButIneligibleParent = false, want true")
		}
	})

	t.Run("DUPLICATE_OF alone is not eligible", func(t *testing.T) {
		edges := []sqlcgen.MediaEdge{
			{ID: 1, Confidence: 1.0, ReviewState: "CONFIRMED", RelationshipType: "DUPLICATE_OF", Tier: 1},
		}
		if got := pickWinningParent(edges); got != nil {
			t.Errorf("pickWinningParent = %+v, want nil (a duplicate is not ancestry)", got)
		}
		if !hasResolvedButIneligibleParent(edges) {
			t.Error("hasResolvedButIneligibleParent = false, want true")
		}
	})

	t.Run("PROJECT_SIDECAR alone is not eligible", func(t *testing.T) {
		edges := []sqlcgen.MediaEdge{
			{ID: 1, Confidence: 1.0, ReviewState: "AUTO_ACCEPTED", RelationshipType: "PROJECT_SIDECAR", Tier: 1},
		}
		if got := pickWinningParent(edges); got != nil {
			t.Errorf("pickWinningParent = %+v, want nil", got)
		}
	})

	t.Run("a lower-confidence valid parent wins over a higher-confidence Tier-3 edge", func(t *testing.T) {
		edges := []sqlcgen.MediaEdge{
			{ID: 1, Confidence: 0.99, ReviewState: "AUTO_ACCEPTED", RelationshipType: "DERIVED_FROM", Tier: 3},
			{ID: 2, Confidence: 0.70, ReviewState: "AUTO_ACCEPTED", RelationshipType: "DERIVED_FROM", Tier: 2},
		}
		got := pickWinningParent(edges)
		if got == nil {
			t.Fatal("pickWinningParent = nil, want edge 2 (the Tier-3 edge must not shadow the valid one)")
		}
		if got.ID != 2 {
			t.Errorf("winning edge id = %d, want 2", got.ID)
		}
	})

	t.Run("no edges at all is distinct from a resolved-but-ineligible edge", func(t *testing.T) {
		if hasResolvedButIneligibleParent(nil) {
			t.Error("hasResolvedButIneligibleParent(nil) = true, want false")
		}
	})
}

func TestAgentRebase_RefusesArchivedNode(t *testing.T) {
	srv, database, _, staging, exports, _ := serverWithGuard(t)
	ctx := context.Background()

	archivedUUID := "018f0000-0000-7000-8000-0000000000aa"
	originalPath := filepath.Join(staging, "old_version.mov")
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          archivedUUID,
			StorageLocationID: 1,
			FilePath:          originalPath,
			FileName:          "old_version.mov",
			LifecycleState:    "ACTIVE",
			GraphStatus:       "UNLINKED",
			IndexingStatus:    "INDEXED_SHALLOW",
		})
		if err != nil {
			return err
		}
		return q.ArchiveMediaNode(ctx, n.ID)
	})
	if err != nil {
		t.Fatalf("seed archived node: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid":   archivedUUID,
		"targetPath": filepath.Join(exports, "resurrected.mov"),
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("rebase of archived node status = %d, want 404 Not Found, body = %s", rr.Code, rr.Body.String())
	}

	// The row must be untouched: still ARCHIVED, still at its original path.
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		node, err := q.GetMediaNodeByUUID(ctx, archivedUUID)
		if err != nil {
			return err
		}
		if node.LifecycleState != "ARCHIVED" {
			t.Errorf("archived node lifecycle_state = %q, want ARCHIVED", node.LifecycleState)
		}
		if node.FilePath != originalPath {
			t.Errorf("archived node file_path = %q, want unchanged %q", node.FilePath, originalPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify archived node untouched: %v", err)
	}
}

// TestAgentRebase_NonTier3ReadOnlyRefusedEvenWithFile is handleAgentRebase's
// half of the same scoping guarantee covered on the drainer side by
// TestDrainer_NonTier3ReadOnlyStaysRefusedEvenWithFile (issue #167): the
// file-already-present exemption applies only to TIER3_MASTER_ARCHIVE, not
// to read-only locations in general, even when the target file genuinely
// exists.
func TestAgentRebase_NonTier3ReadOnlyRefusedEvenWithFile(t *testing.T) {
	root := t.TempDir()
	staging := filepath.Join(root, "staging")
	roDir := filepath.Join(root, "readonly-import")
	for _, d := range []string{staging, roDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	resStaging, _ := filepath.EvalSymlinks(staging)
	resRoDir, _ := filepath.EvalSymlinks(roDir)

	dbPath := filepath.Join(root, "routes.db")
	database, err := db.Open(context.Background(), dbPath)
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })

	ctx := context.Background()
	var stagingLoc, roLoc sqlcgen.StorageLocation
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		stagingLoc, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "staging", RootPath: resStaging, Tier: "TIER0_LOCAL_STAGING", ReadOnly: 0, Prunable: 0,
		})
		if err != nil {
			return err
		}
		roLoc, err = q.UpsertStorageLocation(ctx, sqlcgen.UpsertStorageLocationParams{
			Name: "readonly_import", RootPath: resRoDir, Tier: "TIER2_EXPORTS", ReadOnly: 1, Prunable: 0,
		})
		return err
	})
	if err != nil {
		t.Fatalf("upsert locations: %v", err)
	}

	guard := storage.NewGuard([]storage.Location{
		{ID: stagingLoc.ID, Name: "staging", RootPath: resStaging, Tier: "TIER0_LOCAL_STAGING", ReadOnly: false},
		{ID: roLoc.ID, Name: "readonly_import", RootPath: resRoDir, Tier: "TIER2_EXPORTS", ReadOnly: true},
	})
	srv := New(Deps{
		Config:  &config.Config{Agent: config.Agent{APIKey: routeTestAgentKey}},
		DB:      database,
		Guard:   guard,
		Prober:  probe.New(),
		Engine:  graph.NewEngine(database, nil),
		Hub:     sse.New(),
		Version: "test",
	})

	nodeUUID := "018f0000-0000-7000-8000-0000000000cc"
	originalPath := filepath.Join(resStaging, "orig.raw")
	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		_, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
			NodeUuid:          nodeUUID,
			StorageLocationID: stagingLoc.ID,
			FilePath:          originalPath,
			FileName:          "orig.raw",
			LifecycleState:    "ACTIVE",
			GraphStatus:       "UNLINKED",
			IndexingStatus:    "INDEXED_SHALLOW",
		})
		return err
	})
	if err != nil {
		t.Fatalf("seed node: %v", err)
	}

	// The file genuinely exists at the read-only target -- must still be
	// refused, unlike the Tier-3 case.
	targetPath := filepath.Join(resRoDir, "already_here.raw")
	if err := os.WriteFile(targetPath, []byte("bytes"), 0o644); err != nil {
		t.Fatalf("seed existing file: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/v1/agent/rebase", bytesOfJSON(t, map[string]any{
		"nodeUuid":   nodeUUID,
		"targetPath": targetPath,
	}))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", routeTestAgentKey)
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Fatalf("rebase to non-Tier3 read-only location with existing file status = %d, want 400 Bad Request, body = %s", rr.Code, rr.Body.String())
	}

	err = database.InTx(ctx, func(q *sqlcgen.Queries) error {
		node, err := q.GetMediaNodeByUUID(ctx, nodeUUID)
		if err != nil {
			return err
		}
		if node.FilePath != originalPath {
			t.Errorf("node file_path = %q, want unchanged %q -- refused rebase must not take effect", node.FilePath, originalPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("verify node untouched: %v", err)
	}
}
