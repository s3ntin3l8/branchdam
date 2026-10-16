package httpapi

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/settings"
	"github.com/s3ntin3l8/branchdam/internal/sse"
	attributionusers "github.com/s3ntin3l8/branchdam/internal/users"
)

// newReviewTestServer wires admin groups (so the admin gate is real, not
// the empty-group "permit all" fallback) plus attribution + audit, so a
// reset's actor_audit row can be read back.
func newReviewTestServer(t *testing.T) (*Server, *db.DB, *audit.Service) {
	t.Helper()
	database, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "review.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = database.Close() })
	usersSvc := attributionusers.NewService(database)
	if _, err := usersSvc.EnsureSystemUser(context.Background()); err != nil {
		t.Fatalf("EnsureSystemUser: %v", err)
	}
	store, err := settings.NewStore(context.Background(), database,
		config.Config{Authz: config.Authz{Groups: []string{"dam-admins"}}}, settingsTestKey(t), nil)
	if err != nil {
		t.Fatalf("settings.NewStore: %v", err)
	}
	auditSvc := audit.NewService(database, usersSvc)
	srv := New(Deps{
		Settings: store, DB: database, Hub: sse.New(), Version: "test",
		Attribution: usersSvc, Audit: auditSvc,

		agentKeyLookup: DefaultTestAgentKeyLookup(routeTestAgentKey)})
	return srv, database, auditSvc
}

type reviewEdgeSeed struct {
	relationship string
	tier         int64
	resolver     string
	confidence   float64
	state        string
}

// seedReviewEdge inserts a parent/child node pair and one edge between them
// in the given review state. CONFIRMED/REJECTED rows get the reviewed_at /
// reviewed_by a human review leaves (the schema's CHECK requires
// reviewed_at for both).
func seedReviewEdge(t *testing.T, database *db.DB, locID int64, e reviewEdgeSeed) (edgeID, targetID int64) {
	t.Helper()
	ctx := context.Background()
	if e.relationship == "" {
		e.relationship = "DERIVED_FROM"
	}
	if e.state == "" {
		e.state = "NEEDS_REVIEW"
	}
	err := database.InTx(ctx, func(q *sqlcgen.Queries) error {
		var ids [2]int64
		for i, suffix := range []string{"-p", "-c"} {
			u := uuid.New().String() + suffix
			n, err := q.InsertMediaNode(ctx, sqlcgen.InsertMediaNodeParams{
				NodeUuid: u, StorageLocationID: locID,
				FilePath: "/seed/" + u + ".jpg", FileName: u + ".jpg", FileExt: ".jpg",
				SizeBytes: 1, MtimeUnix: 1, IndexingStatus: "INDEXED_SHALLOW",
				GraphStatus: "UNLINKED", LifecycleState: "ACTIVE",
			})
			if err != nil {
				return err
			}
			ids[i] = n.ID
		}
		state := e.state
		if state == "CONFIRMED" || state == "REJECTED" {
			state = "NEEDS_REVIEW"
		}
		row, err := q.CreateMediaEdge(ctx, sqlcgen.CreateMediaEdgeParams{
			SourceNodeID: ids[0], TargetNodeID: ids[1], RelationshipType: e.relationship,
			Confidence: e.confidence, Tier: e.tier, Resolver: e.resolver, EvidenceJson: "{}",
			ReviewState: state,
		})
		if err != nil {
			return err
		}
		edgeID, targetID = row.ID, ids[1]
		switch e.state {
		case "CONFIRMED":
			_, err = q.ConfirmMediaEdge(ctx, sqlcgen.ConfirmMediaEdgeParams{ID: row.ID, ReviewedBy: reviewerNameOf("carol")})
		case "REJECTED":
			_, err = q.RejectMediaEdge(ctx, sqlcgen.RejectMediaEdgeParams{ID: row.ID, ReviewedBy: reviewerNameOf("carol")})
		}
		if err != nil {
			return err
		}
		return recomputeGraphStatus(ctx, q, ids[1])
	})
	if err != nil {
		t.Fatalf("seed edge %+v: %v", e, err)
	}
	return edgeID, targetID
}

func reviewerNameOf(name string) sql.NullString {
	return sql.NullString{String: name, Valid: true}
}

func userReq(method, path, username, groups string) *http.Request {
	r := httptest.NewRequest(method, path, nil)
	r.Header.Set("X-Authentik-Username", username)
	r.Header.Set("X-Authentik-Groups", groups)
	return r
}

func serve(srv *Server, r *http.Request) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, r)
	return rr
}

// TestResetEdgeReview_RejectedEdgeReturnsToQueue: an admin can undo a
// human REJECTED decision -- the edge goes back to NEEDS_REVIEW, the
// target's graph_status is recomputed, the edge is back in the audit
// queue, and the reset is recorded in actor_audit with what it undid.
func TestResetEdgeReview_RejectedEdgeReturnsToQueue(t *testing.T) {
	srv, database, auditSvc := newReviewTestServer(t)
	ctx := context.Background()
	locID := seedAuditLocation(t, database)
	edgeID, targetID := seedReviewEdge(t, database, locID, reviewEdgeSeed{tier: 2, resolver: "stem", confidence: 0.7})

	if rr := serve(srv, adminReq(http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/reject", edgeID), nil)); rr.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rr.Code, rr.Body.String())
	}
	if n, _ := database.Reader.GetMediaNodeByID(ctx, targetID); n.GraphStatus != "UNLINKED" {
		t.Fatalf("after reject graph_status = %q, want UNLINKED", n.GraphStatus)
	}

	rr := serve(srv, adminReq(http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/reset-review", edgeID), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("reset-review: %d %s", rr.Code, rr.Body.String())
	}
	var body struct {
		OK            bool   `json:"ok"`
		PreviousState string `json:"previousState"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !body.OK || body.PreviousState != "REJECTED" {
		t.Fatalf("body = %+v, want ok + previousState REJECTED", body)
	}

	edge, err := database.Reader.GetMediaEdge(ctx, edgeID)
	if err != nil {
		t.Fatalf("GetMediaEdge: %v", err)
	}
	if edge.ReviewState != "NEEDS_REVIEW" || edge.ReviewedAt.Valid || edge.ReviewedBy.Valid {
		t.Fatalf("edge after reset = %s reviewedAt=%v reviewedBy=%v, want NEEDS_REVIEW with review stamps cleared",
			edge.ReviewState, edge.ReviewedAt, edge.ReviewedBy)
	}
	if n, _ := database.Reader.GetMediaNodeByID(ctx, targetID); n.GraphStatus != "NEEDS_REVIEW" {
		t.Errorf("after reset graph_status = %q, want NEEDS_REVIEW", n.GraphStatus)
	}

	q := serve(srv, adminReq(http.MethodGet, "/api/v1/edges/audit", nil))
	var queue struct {
		Entries []struct {
			ID int64 `json:"id"`
		} `json:"entries"`
		Total int64 `json:"total"`
	}
	if err := json.Unmarshal(q.Body.Bytes(), &queue); err != nil {
		t.Fatalf("unmarshal queue: %v", err)
	}
	if queue.Total != 1 || len(queue.Entries) != 1 || queue.Entries[0].ID != edgeID {
		t.Fatalf("queue after reset = %+v, want edge %d back", queue, edgeID)
	}

	entries, _, err := auditSvc.ListActivity(ctx, audit.Filter{Event: audit.EventEdgeReviewReset}, 10, 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("edge.review_reset audit rows = %d, want 1", len(entries))
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(entries[0].DetailsJSON), &details); err != nil {
		t.Fatalf("details: %v", err)
	}
	if details["previousState"] != "REJECTED" || details["previousReviewedBy"] != "admin" ||
		entries[0].ResourceType != "edge" || entries[0].ResourceID.String != fmt.Sprint(edgeID) || entries[0].ActorName != "admin" {
		t.Errorf("audit row = %+v details=%v", entries[0], details)
	}
}

// TestResetEdgeReview_ConfirmedEdge: a CONFIRMED decision resets too, and
// the target loses the LINKED status that decision gave it.
func TestResetEdgeReview_ConfirmedEdge(t *testing.T) {
	srv, database, _ := newReviewTestServer(t)
	ctx := context.Background()
	locID := seedAuditLocation(t, database)
	edgeID, targetID := seedReviewEdge(t, database, locID, reviewEdgeSeed{tier: 3, resolver: "phash", confidence: 0.6, state: "CONFIRMED"})
	if n, _ := database.Reader.GetMediaNodeByID(ctx, targetID); n.GraphStatus != "LINKED" {
		t.Fatalf("seeded CONFIRMED target graph_status = %q, want LINKED", n.GraphStatus)
	}

	rr := serve(srv, adminReq(http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/reset-review", edgeID), nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("reset-review: %d %s", rr.Code, rr.Body.String())
	}
	if e, _ := database.Reader.GetMediaEdge(ctx, edgeID); e.ReviewState != "NEEDS_REVIEW" {
		t.Errorf("review_state = %s, want NEEDS_REVIEW", e.ReviewState)
	}
	if n, _ := database.Reader.GetMediaNodeByID(ctx, targetID); n.GraphStatus != "NEEDS_REVIEW" {
		t.Errorf("graph_status = %q, want NEEDS_REVIEW", n.GraphStatus)
	}
}

// TestResetEdgeReview_OnlyHumanDecisions: resolver states are not a human
// decision to undo (409), unknown ids are 404, and the edge is untouched.
func TestResetEdgeReview_OnlyHumanDecisions(t *testing.T) {
	srv, database, _ := newReviewTestServer(t)
	ctx := context.Background()
	locID := seedAuditLocation(t, database)
	for _, state := range []string{"NEEDS_REVIEW", "AUTO_ACCEPTED"} {
		edgeID, _ := seedReviewEdge(t, database, locID, reviewEdgeSeed{tier: 2, resolver: "stem", confidence: 0.9, state: state})
		rr := serve(srv, adminReq(http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/reset-review", edgeID), nil))
		if rr.Code != http.StatusConflict {
			t.Errorf("%s: status = %d, want 409 (%s)", state, rr.Code, rr.Body.String())
		}
		if e, _ := database.Reader.GetMediaEdge(ctx, edgeID); e.ReviewState != state {
			t.Errorf("%s edge changed to %s", state, e.ReviewState)
		}
	}
	if rr := serve(srv, adminReq(http.MethodPost, "/api/v1/edges/999999/reset-review", nil)); rr.Code != http.StatusNotFound {
		t.Errorf("unknown edge: status = %d, want 404", rr.Code)
	}
}

// TestResetEdgeReview_AdminHumansOnly: a non-admin person, a machine
// (agent) principal and an admin's personal access token (a script, no
// person present) are all refused, and the human decision stands.
func TestResetEdgeReview_AdminHumansOnly(t *testing.T) {
	srv, database, _ := newReviewTestServer(t)
	ctx := context.Background()
	locID := seedAuditLocation(t, database)
	edgeID, _ := seedReviewEdge(t, database, locID, reviewEdgeSeed{tier: 2, resolver: "stem", confidence: 0.7, state: "REJECTED"})

	rr := serve(srv, userReq(http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/reset-review", edgeID), "bob", "dam-users"))
	if rr.Code != http.StatusForbidden {
		t.Errorf("non-admin: status = %d, want 403", rr.Code)
	}

	machine := auth.WithPrincipal(ctx, auth.Principal{Kind: auth.KindMachine, Name: "agent-1", Authenticated: true})
	_, err := srv.handleResetEdgeReview(machine, &EdgeReviewInput{ID: edgeID})
	var se huma.StatusError
	if !errors.As(err, &se) || se.GetStatus() != http.StatusForbidden {
		t.Errorf("machine principal: err = %v, want 403", err)
	}

	// Admin by group, so only the PAT check can refuse it.
	pat := auth.WithPrincipal(ctx, auth.Principal{
		Kind: auth.KindUser, Name: "carol", Groups: []string{"dam-admins"},
		AuthProvider: auth.AuthProviderPAT, UserID: 1, ExternalUID: "pat:abcd", Authenticated: true,
	})
	if err := srv.requireSettingsAdmin(pat); err != nil {
		t.Fatalf("PAT principal fixture is not an admin: %v", err)
	}
	_, err = srv.handleResetEdgeReview(pat, &EdgeReviewInput{ID: edgeID})
	if !errors.As(err, &se) || se.GetStatus() != http.StatusForbidden {
		t.Errorf("admin PAT principal: err = %v, want 403", err)
	}

	if e, _ := database.Reader.GetMediaEdge(ctx, edgeID); e.ReviewState != "REJECTED" {
		t.Errorf("refused reset still changed the edge to %s", e.ReviewState)
	}
}

type auditQueueBody struct {
	Entries []struct {
		ID               int64   `json:"id"`
		Tier             int64   `json:"tier"`
		Resolver         string  `json:"resolver"`
		RelationshipType string  `json:"relationshipType"`
		Confidence       float64 `json:"confidence"`
	} `json:"entries"`
	Total     int64    `json:"total"`
	Resolvers []string `json:"resolvers"`
}

func getQueue(t *testing.T, srv *Server, v url.Values) (int, auditQueueBody) {
	t.Helper()
	rr := serve(srv, adminReq(http.MethodGet, "/api/v1/edges/audit?"+v.Encode(), nil))
	var body auditQueueBody
	if rr.Code == http.StatusOK {
		if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
	}
	return rr.Code, body
}

// TestAuditQueueServerSideFilters: tier / resolver / relationshipType /
// minConfidence filter the whole queue server-side -- total is the
// filtered count and keyset paging walks only matching rows -- and the
// resolver options come from the whole queue, not the loaded page.
func TestAuditQueueServerSideFilters(t *testing.T) {
	srv, database, _ := newReviewTestServer(t)
	locID := seedAuditLocation(t, database)
	seeds := []reviewEdgeSeed{
		{tier: 2, resolver: "stem", confidence: 0.90, relationship: "DERIVED_FROM"},
		{tier: 2, resolver: "stem", confidence: 0.80, relationship: "FINAL_EXPORT"},
		{tier: 2, resolver: "xmp", confidence: 0.70, relationship: "DERIVED_FROM"},
		{tier: 3, resolver: "phash", confidence: 0.60, relationship: "DUPLICATE_OF"},
		{tier: 3, resolver: "phash", confidence: 0.55, relationship: "DUPLICATE_OF"},
		{tier: 1, resolver: "sidecar", confidence: 0.95, relationship: "PROJECT_SIDECAR", state: "REJECTED"},
	}
	for _, s := range seeds {
		seedReviewEdge(t, database, locID, s)
	}

	cases := []struct {
		name  string
		q     url.Values
		total int64
		check func(tier int64, resolver, rel string, conf float64) bool
	}{
		{"tier", url.Values{"tier": {"3"}}, 2, func(tier int64, _, _ string, _ float64) bool { return tier == 3 }},
		{"resolver", url.Values{"resolver": {"stem"}}, 2, func(_ int64, r, _ string, _ float64) bool { return r == "stem" }},
		{"relationship", url.Values{"relationshipType": {"DERIVED_FROM"}}, 2, func(_ int64, _, rel string, _ float64) bool { return rel == "DERIVED_FROM" }},
		{"minConfidence", url.Values{"minConfidence": {"0.75"}}, 2, func(_ int64, _, _ string, c float64) bool { return c >= 0.75 }},
		{"combined", url.Values{"tier": {"2"}, "relationshipType": {"DERIVED_FROM"}, "minConfidence": {"0.85"}}, 1,
			func(tier int64, _, rel string, c float64) bool {
				return tier == 2 && rel == "DERIVED_FROM" && c >= 0.85
			}},
		{"none", url.Values{}, 5, func(int64, string, string, float64) bool { return true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			code, body := getQueue(t, srv, tc.q)
			if code != http.StatusOK {
				t.Fatalf("status = %d", code)
			}
			if body.Total != tc.total || int64(len(body.Entries)) != tc.total {
				t.Fatalf("total=%d entries=%d, want %d", body.Total, len(body.Entries), tc.total)
			}
			for _, e := range body.Entries {
				if !tc.check(e.Tier, e.Resolver, e.RelationshipType, e.Confidence) {
					t.Errorf("entry %+v does not match the filter", e)
				}
			}
			if got := fmt.Sprint(body.Resolvers); got != "[phash stem xmp]" {
				t.Errorf("resolvers = %s, want the whole queue's [phash stem xmp]", got)
			}
		})
	}

	// Keyset paging within a filter: tier=2 one row at a time.
	_, p1 := getQueue(t, srv, url.Values{"tier": {"2"}, "limit": {"1"}})
	_, p2 := getQueue(t, srv, url.Values{"tier": {"2"}, "limit": {"1"}, "beforeId": {fmt.Sprint(p1.Entries[0].ID)}})
	_, p3 := getQueue(t, srv, url.Values{"tier": {"2"}, "limit": {"1"}, "beforeId": {fmt.Sprint(p2.Entries[0].ID)}})
	_, p4 := getQueue(t, srv, url.Values{"tier": {"2"}, "limit": {"1"}, "beforeId": {fmt.Sprint(p3.Entries[0].ID)}})
	got := []float64{p1.Entries[0].Confidence, p2.Entries[0].Confidence, p3.Entries[0].Confidence}
	if fmt.Sprint(got) != "[0.9 0.8 0.7]" || len(p4.Entries) != 0 || p1.Total != 3 {
		t.Errorf("tier=2 keyset pages = %v then %d rows (total %d), want [0.9 0.8 0.7] then none, total 3", got, len(p4.Entries), p1.Total)
	}

	if code, _ := getQueue(t, srv, url.Values{"relationshipType": {"NOT_A_TYPE"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("unknown relationshipType: status = %d, want 422", code)
	}
	if code, _ := getQueue(t, srv, url.Values{"minConfidence": {"1.5"}}); code != http.StatusUnprocessableEntity {
		t.Errorf("minConfidence 1.5: status = %d, want 422", code)
	}
}
