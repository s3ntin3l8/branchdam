package httpapi

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/agent"
)

// The agent-API contract fixtures (X1): one JSON file per request and
// response body of every /api/v1/agent/* route, generated from the
// server's own DTOs, so branchdam-agent and branchdam-mobile can pin a
// copy and diff their own DTOs against it instead of against a mock.
//
// Every fixture sets every field: requireAllFieldsSet fails when a DTO
// gains a field the fixture leaves at its zero value, so a new field
// always reaches the fixtures -- and from there the clients' pinned
// copies -- instead of drifting silently.
//
// Regenerate with `make contract-fixtures` (go test -run
// TestContractFixtures -update-contract ./internal/httpapi/).
var updateContract = flag.Bool("update-contract", false, "rewrite docs/contract/fixtures from the server DTOs")

const contractFixtureDir = "../../docs/contract/fixtures"

// Low-entropy example values: real hex/base64 shapes without looking like
// secrets to scanners.
const (
	fxFullHash  = "abababababababababababababababababababababababababababababababab"
	fxFastHash  = "cdcdcdcdcdcdcdcd"
	fxPathHash  = "efefefefefefefefefefefefefefefefefefefefefefefefefefefefefefefef"
	fxNodeUUID  = "11111111-1111-4111-8111-111111111111"
	fxNodeUUID2 = "22222222-2222-4222-8222-222222222222"
	fxEventUUID = "33333333-3333-4333-8333-333333333333"
	fxAgentID   = "workstation-01"
	fxScopeID   = "4444444444444444444444444444444444444444444444444444444444444444"
)

func strp(s string) *string   { return &s }
func i64p(n int64) *int64     { return &n }
func f64p(f float64) *float64 { return &f }

// contractFixtures returns every fixture by file stem. Values are fully
// populated so the fixture shows every field the server reads or writes.
func contractFixtures(t *testing.T) map[string]any {
	t.Helper()
	fx := map[string]any{}

	var hello AgentHelloOutput
	hello.Body.OK = true
	hello.Body.Version = "1.2.3"
	fx["hello_response"] = hello.Body

	var hsIn AgentHandshakeInput
	hsIn.Body.AgentID = fxAgentID
	hsIn.Body.ClientVersion = "0.9.0"
	hsIn.Body.LastProcessedEventUUID = fxEventUUID
	hsIn.Body.CurrentKeyID = i64p(7)
	fx["handshake_request"] = hsIn.Body

	var hsOut AgentHandshakeOutput
	hsOut.Body.OK = true
	hsOut.Body.ServerVersion = "1.2.3"
	hsOut.Body.ServerTimeUnix = 1760000000
	hsOut.Body.AcknowledgedEventUUID = fxEventUUID
	hsOut.Body.PendingEventsCount = 2
	hsOut.Body.NamingTemplate = "{date:2006}/{date:2006-01-02}/{original}"
	hsOut.Body.CurrentKeyID = 7
	hsOut.Body.PendingRotation = &pendingRotationDTO{
		KeyID:                8,
		APIKey:               "", // never sent in the clear: see contractZeroOK
		SealedAPIKey:         "v1:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA",
		PreviousKeyExpiresAt: 1760086400,
	}
	fx["handshake_response"] = hsOut.Body

	// Event payloads: the inner JSON each eventType carries, double-encoded
	// as a string in the envelope's payload (see events_request).
	nodeCreated := agent.NodeCreatedPayload{
		NodeUUID:           fxNodeUUID,
		StorageLocationID:  3,
		FilePath:           "/data/archive/2025/2025-10-16/A001_C002.MOV",
		FileName:           "A001_C002.MOV",
		FileExt:            "mov",
		SizeBytes:          1048576,
		MtimeUnix:          1760000000,
		FastHash:           strp(fxFastHash),
		FullHash:           strp(fxFullHash),
		SourcePathHash:     strp(fxPathHash),
		Phash:              i64p(-4242424242),
		CameraModel:        strp("FX3"),
		CameraSerial:       strp("5012345"),
		LensModel:          strp("FE 24-70mm F2.8 GM II"),
		CapturedAtUnix:     i64p(1759990000),
		OriginalDocumentID: strp("xmp.did:0001"),
		DocumentID:         strp("xmp.did:0002"),
		DerivedFromID:      strp("xmp.did:0000"),
		FilenameStem:       strp("A001_C002"),
		GPSLatitude:        f64p(52.52),
		GPSLongitude:       f64p(13.405),
	}
	fx["event_node_created"] = nodeCreated
	fx["event_edge_attached"] = agent.EdgeAttachedPayload{
		SourceNodeUUID:   fxNodeUUID,
		TargetNodeUUID:   fxNodeUUID2,
		SourceNodeID:     10,
		TargetNodeID:     11,
		RelationshipType: "PROXY_OF",
		Confidence:       0.95,
		Tier:             2,
		Resolver:         "stem-match",
		EvidenceJSON:     json.RawMessage(`{"stem":"A001_C002"}`),
		ReviewState:      "(rejected when an agent sends it)",
	}
	fx["event_node_moved"] = agent.NodeMovedPayload{
		NodeUUID:             fxNodeUUID,
		NewFilePath:          "/data/archive/2025/2025-10-17/A001_C002.MOV",
		NewFileName:          "A001_C002.MOV",
		NewStorageLocationID: 3,
		MtimeUnix:            1760000500,
	}
	fx["event_node_deleted"] = agent.NodeDeletedPayload{NodeUUID: fxNodeUUID}
	fx["event_path_rebased"] = agent.PathRebasedPayload{
		NodeUUID:                fxNodeUUID,
		TargetFilePath:          "/data/archive/2025/2025-10-16/A001_C002.MOV",
		TargetFileName:          "A001_C002.MOV",
		TargetStorageLocationID: 3,
		MtimeUnix:               1760000000,
		FastHash:                strp(fxFastHash),
		FullHash:                strp(fxFullHash),
		SizeBytes:               1048576,
	}
	fx["event_virtual_node_created"] = agent.VirtualNodeCreated{
		NodeUUID:     fxNodeUUID2,
		FilePath:     "/virtual/resolve/workstation-01/Edit-a2bb3478",
		DisplayName:  "Edit v3",
		ProjectType:  "resolve_project",
		EvidenceJSON: json.RawMessage(`{"project":"Edit"}`),
	}

	payload, err := json.Marshal(nodeCreated)
	if err != nil {
		t.Fatal(err)
	}
	var evIn AgentEventInput
	evIn.Body.EventUUID = fxEventUUID
	evIn.Body.AgentID = fxAgentID
	evIn.Body.EventType = agent.EventNodeCreated
	evIn.Body.Payload = string(payload)
	fx["events_request"] = evIn.Body

	var evOut AgentEventOutput
	evOut.Body.EventID = fxEventUUID // the event UUID, minted by the server when the request has none
	fx["events_response"] = evOut.Body

	var rbIn AgentRebaseInput
	rbIn.Body.NodeUUID = fxNodeUUID
	rbIn.Body.TargetPath = "/data/archive/2025/2025-10-16/A001_C002.MOV"
	rbIn.Body.MtimeUnix = 1760000000
	rbIn.Body.FileName = "A001_C002.MOV"
	rbIn.Body.FileExt = "mov"
	rbIn.Body.SizeBytes = 1048576
	rbIn.Body.FastHash = strp(fxFastHash)
	rbIn.Body.FullHash = strp(fxFullHash)
	rbIn.Body.StorageLocationID = 3
	fx["rebase_request"] = rbIn.Body

	var rbOut AgentRebaseOutput
	rbOut.Body.ID = 10
	rbOut.Body.NodeUUID = fxNodeUUID
	rbOut.Body.StorageLocationID = 3
	rbOut.Body.FilePath = "/data/archive/2025/2025-10-16/A001_C002.MOV"
	rbOut.Body.Status = "REBASED"
	fx["rebase_response"] = rbOut.Body

	var nsIn AgentNodeStatusInput
	nsIn.Body.NodeUUIDs = []string{fxNodeUUID, fxNodeUUID2}
	fx["node_status_request"] = nsIn.Body

	var nsOut AgentNodeStatusOutput
	nsOut.Body.Statuses = []AgentNodeStatusEntry{{
		NodeUUID:          fxNodeUUID2,
		Found:             true,
		CanonicalNodeUUID: fxNodeUUID,
		LifecycleState:    "ACTIVE",
		Tier:              "TIER3_MASTER_ARCHIVE",
		Verified:          true,
		FullHash:          fxFullHash,
	}}
	fx["node_status_response"] = nsOut.Body

	fx["check_content_query"] = queryFixture(t, AgentCheckContentInput{
		FastHash:  fxFastHash,
		FullHash:  fxFullHash,
		SizeBytes: 1048576,
	})
	fx["check_content_response"] = AgentCheckContentResult{
		Found:          true,
		PossibleMatch:  true,
		NodeUUID:       fxNodeUUID,
		FilePath:       "/data/archive/2025/2025-10-16/A001_C002.MOV",
		LifecycleState: "ACTIVE",
		IndexingStatus: "INDEXED_FULL",
	}

	fx["source_status_query"] = queryFixture(t, AgentSourceStatusInput{
		SourcePathHash: fxPathHash,
		SourcePath:     fxPathHash,
	})
	fx["source_status_response"] = AgentSourceStatusResult{
		Tracked:        true,
		NodeUUID:       fxNodeUUID,
		FilePath:       "/data/archive/2025/2025-10-16/A001_C002.MOV",
		IndexingStatus: "INDEXED_FULL",
		LifecycleState: "ACTIVE",
	}

	var rsIn ResolveSnapshotInput
	rsIn.Body.AgentID = fxAgentID
	rsIn.Body.ScopeID = fxScopeID
	rsIn.Body.Timelines = []ResolveSnapshotTimeline{{
		TimelineID:   "tl-0001",
		NodeUUID:     fxNodeUUID2,
		FilePath:     "/virtual/resolve/workstation-01/Edit-a2bb3478",
		DisplayName:  "Edit v3",
		EvidenceJSON: json.RawMessage(`{"project":"Edit"}`),
	}}
	rsIn.Body.Memberships = []ResolveSnapshotMembership{{
		TimelineID:     "tl-0001",
		MediaFilePath:  "/data/archive/2025/2025-10-16/A001_C002.MOV",
		SourceNodeUUID: fxNodeUUID,
		EvidenceJSON:   json.RawMessage(`{"clip":"A001_C002"}`),
	}}
	rsIn.Body.LegacyTimelineNodeUUIDs = []string{fxNodeUUID}
	rsIn.Body.RetireScopeID = fxScopeID
	fx["resolve_snapshot_request"] = rsIn.Body
	fx["resolve_snapshot_response"] = ResolveSnapshotResult{
		Created: 1, Refreshed: 2, Removed: 3, Unchanged: 4, Unresolved: 5, ReviewedConflicts: 6,
	}

	var tIn AgentTelemetryInput
	tIn.Body.AgentID = fxAgentID
	tIn.Body.ClientVersion = "0.9.0"
	tIn.Body.TimestampUnix = 1760000000
	tIn.Body.ScratchStorage = AgentScratchStorageDTO{
		MountPath:            "/Volumes/EditSSD",
		TotalBytes:           1000,
		FreeBytes:            300,
		UsedBytes:            700,
		MirrorsSizeBytes:     100,
		RenderCacheSizeBytes: 50,
		ProxiesSizeBytes:     25,
		PrunableBytes:        10,
	}
	tIn.Body.PruneStats = &AgentPruneStatsDTO{
		LastPruneTimestampUnix: 1759999000,
		LastReclaimedBytes:     4096,
		LastPruneDurationMs:    250,
		PrunedItemCounts:       map[string]int{"mirrors": 3},
	}
	fx["telemetry_request"] = tIn.Body

	var tOut AgentTelemetryOutput
	tOut.Body.OK = true
	tOut.Body.AcknowledgedAtUnix = 1760000001
	fx["telemetry_response"] = tOut.Body

	// Built from agentUploadHeaders by field name, so a header the
	// handler gains without an example here fails the test.
	uploadExamples := map[string]string{
		"Filename":         "A001_C002.MOV",
		"CameraModel":      "FX3",
		"Blake3Hash":       fxFullHash,
		"SourcePathHash":   fxPathHash,
		"CaptureTimestamp": "1759990000",
	}
	headers := map[string]string{}
	hv := reflect.ValueOf(agentUploadHeaders)
	for i := 0; i < hv.NumField(); i++ {
		field := hv.Type().Field(i).Name
		ex, ok := uploadExamples[field]
		if !ok {
			t.Fatalf("agentUploadHeaders.%s has no example in the contract fixture", field)
		}
		headers[hv.Field(i).String()] = ex
	}
	fx["upload_request_headers"] = headers
	fx["upload_response"] = AgentUploadResponse{
		NodeUUID:     fxNodeUUID,
		Status:       "UPLOADED",
		BytesWritten: 1048576,
		Blake3Hash:   fxFullHash,
		RelativePath: "2025/2025-10-16/A001_C002.MOV",
	}
	return fx
}

// queryFixture renders a query-parameter input struct as a JSON object
// keyed by its `query` tags, so a GET route's parameters are pinned the
// same way as a body.
func queryFixture(t *testing.T, v any) map[string]any {
	t.Helper()
	out := map[string]any{}
	rv := reflect.ValueOf(v)
	for i := 0; i < rv.NumField(); i++ {
		name := rv.Type().Field(i).Tag.Get("query")
		if name == "" {
			continue
		}
		out[name] = rv.Field(i).Interface()
	}
	if len(out) == 0 {
		t.Fatalf("%T has no query fields", v)
	}
	return out
}

// contractZeroOK lists the fields whose zero value IS the wire value, so a
// fixture shows them as the server actually sends them.
var contractZeroOK = map[string]string{
	"handshake_response.PendingRotation.APIKey": "always empty: the rotated key is only ever sent sealed",
}

// requireAllFieldsSet fails for any exported field (recursively) still at
// its zero value, so a fixture can't silently miss a new DTO field.
// Booleans must be true for the same reason.
func requireAllFieldsSet(t *testing.T, path string, v reflect.Value) {
	t.Helper()
	if _, ok := contractZeroOK[path]; ok {
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			t.Errorf("%s is nil: set it in the contract fixture", path)
			return
		}
		requireAllFieldsSet(t, path, v.Elem())
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			f := v.Type().Field(i)
			if !f.IsExported() || f.Tag.Get("json") == "-" {
				continue
			}
			requireAllFieldsSet(t, path+"."+f.Name, v.Field(i))
		}
	case reflect.Slice, reflect.Map:
		if v.Len() == 0 {
			t.Errorf("%s is empty: set it in the contract fixture", path)
			return
		}
		if v.Kind() == reflect.Slice && v.Type().Elem().Kind() != reflect.Uint8 {
			for i := 0; i < v.Len(); i++ {
				requireAllFieldsSet(t, fmt.Sprintf("%s[%d]", path, i), v.Index(i))
			}
		}
	default:
		if v.IsZero() {
			t.Errorf("%s is the zero value: set it in the contract fixture", path)
		}
	}
}

func encodeFixture(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

// TestContractFixtures pins docs/contract/fixtures to the server DTOs.
func TestContractFixtures(t *testing.T) {
	fx := contractFixtures(t)
	names := make([]string, 0, len(fx))
	for name := range fx {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		requireAllFieldsSet(t, name, reflect.ValueOf(fx[name]))
	}

	if *updateContract {
		if err := os.MkdirAll(contractFixtureDir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range names {
		want := encodeFixture(t, fx[name])
		path := filepath.Join(contractFixtureDir, name+".json")
		if *updateContract {
			if err := os.WriteFile(path, want, 0o644); err != nil {
				t.Fatal(err)
			}
			continue
		}
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v (run make contract-fixtures)", path, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s is stale: the server DTO changed. Run make contract-fixtures and update the clients' pinned copies.\n--- got\n%s\n--- want\n%s", path, got, want)
		}
	}

	// No stray fixture: every file in the directory is generated here, so
	// a removed DTO can't leave a fixture the clients keep trusting.
	entries, err := os.ReadDir(contractFixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		stem := strings.TrimSuffix(e.Name(), ".json")
		if _, ok := fx[stem]; !ok && strings.HasSuffix(e.Name(), ".json") {
			if *updateContract {
				if err := os.Remove(filepath.Join(contractFixtureDir, e.Name())); err != nil {
					t.Fatal(err)
				}
				continue
			}
			t.Errorf("%s/%s is not generated by TestContractFixtures; remove it or add it there", contractFixtureDir, e.Name())
		}
	}
}

// TestContractFixturesDecodeIntoServerDTOs: every request fixture decodes
// strictly (no unknown field) into the DTO the route reads, so a fixture
// is what the server accepts, not just what it would encode.
func TestContractFixturesDecodeIntoServerDTOs(t *testing.T) {
	var (
		hs AgentHandshakeInput
		ev AgentEventInput
		rb AgentRebaseInput
		ns AgentNodeStatusInput
		rs ResolveSnapshotInput
		te AgentTelemetryInput
	)
	targets := map[string]any{
		"handshake_request":          &hs.Body,
		"events_request":             &ev.Body,
		"rebase_request":             &rb.Body,
		"node_status_request":        &ns.Body,
		"resolve_snapshot_request":   &rs.Body,
		"telemetry_request":          &te.Body,
		"event_node_created":         &agent.NodeCreatedPayload{},
		"event_edge_attached":        &agent.EdgeAttachedPayload{},
		"event_node_moved":           &agent.NodeMovedPayload{},
		"event_node_deleted":         &agent.NodeDeletedPayload{},
		"event_path_rebased":         &agent.PathRebasedPayload{},
		"event_virtual_node_created": &agent.VirtualNodeCreated{},
	}
	for name, dst := range targets {
		b, err := os.ReadFile(filepath.Join(contractFixtureDir, name+".json"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		dec := json.NewDecoder(bytes.NewReader(b))
		dec.DisallowUnknownFields()
		if err := dec.Decode(dst); err != nil {
			t.Errorf("%s does not decode into %T: %v", name, dst, err)
		}
	}
}
