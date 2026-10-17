package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/db"
)

// assertEdgeReviewAuditRow reads back the single actor_audit row for event
// and checks the resource, actor (name, kind and the admin's users.id) and
// the details shape shared with edge.review_reset. previousState is the
// expected details.previousState, or nil when the key must be absent.
// reviewState is checked against the edge row itself too, so the audit
// value is the one the database holds. It returns the decoded details for
// event-specific checks.
func assertEdgeReviewAuditRow(t *testing.T, database *db.DB, auditSvc *audit.Service, event string, edgeID int64, resolver string, previousState any, reviewState string) map[string]any {
	t.Helper()
	ctx := context.Background()
	entries, _, err := auditSvc.ListActivity(ctx, audit.Filter{Event: event}, 10, 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("%s audit rows = %d, want 1", event, len(entries))
	}
	e := entries[0]
	if e.ResourceType != "edge" || e.ResourceID.String != fmt.Sprint(edgeID) || e.ActorName != "admin" || e.ActorKind != audit.ActorKindUser {
		t.Errorf("audit row = %+v, want resource edge/%d by user admin", e, edgeID)
	}
	// adminReq's principal is provisioned lazily into users on its first
	// request; the audit row must point at that row.
	admin, err := database.Reader.GetUserByUsername(ctx, "admin")
	if err != nil {
		t.Fatalf("GetUserByUsername(admin): %v", err)
	}
	if !e.ActorUserID.Valid || e.ActorUserID.Int64 != admin.ID {
		t.Errorf("audit ActorUserID = %+v, want %d (admin)", e.ActorUserID, admin.ID)
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(e.DetailsJSON), &details); err != nil {
		t.Fatalf("details: %v", err)
	}
	gotPrev, hasPrev := details["previousState"]
	if previousState == nil {
		if hasPrev {
			t.Errorf("details.previousState = %v, want absent", gotPrev)
		}
	} else if gotPrev != previousState {
		t.Errorf("details.previousState = %v, want %v", gotPrev, previousState)
	}
	if details["edgeId"] != float64(edgeID) || details["relationshipType"] != "DERIVED_FROM" ||
		details["resolver"] != resolver || details["reviewState"] != reviewState {
		t.Errorf("details = %v", details)
	}
	if _, ok := details["sourceNodeId"].(float64); !ok {
		t.Errorf("details.sourceNodeId missing: %v", details)
	}
	if _, ok := details["targetNodeId"].(float64); !ok {
		t.Errorf("details.targetNodeId missing: %v", details)
	}
	edge, err := database.Reader.GetMediaEdge(ctx, edgeID)
	if err != nil {
		t.Fatalf("GetMediaEdge(%d): %v", edgeID, err)
	}
	if details["reviewState"] != edge.ReviewState {
		t.Errorf("details.reviewState = %v, edge row review_state = %q", details["reviewState"], edge.ReviewState)
	}
	return details
}

func countAuditRows(t *testing.T, auditSvc *audit.Service, event string) int {
	t.Helper()
	entries, _, err := auditSvc.ListActivity(context.Background(), audit.Filter{Event: event}, 10, 0)
	if err != nil {
		t.Fatalf("ListActivity: %v", err)
	}
	return len(entries)
}

// TestConfirmEdge_WritesAuditRow: a confirm is recorded in actor_audit as
// edge.review_confirmed with the decision it replaced and the one it set.
func TestConfirmEdge_WritesAuditRow(t *testing.T) {
	srv, database, auditSvc := newReviewTestServer(t)
	locID := seedAuditLocation(t, database)
	seed := reviewEdgeSeed{tier: 2, resolver: "stem", confidence: 0.7}
	edgeID, _ := seedReviewEdge(t, database, locID, seed)

	if rr := serve(srv, adminReq(http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/confirm", edgeID), nil)); rr.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", rr.Code, rr.Body.String())
	}
	details := assertEdgeReviewAuditRow(t, database, auditSvc, audit.EventEdgeReviewConfirmed, edgeID, seed.resolver, "NEEDS_REVIEW", "CONFIRMED")
	if _, ok := details["source"]; ok {
		t.Errorf("confirm details carry source = %v, want absent (manual creation only)", details["source"])
	}
	if n := countAuditRows(t, auditSvc, audit.EventEdgeReviewRejected); n != 0 {
		t.Errorf("confirm wrote %d edge.review_rejected rows, want 0", n)
	}
}

// TestRejectEdge_WritesAuditRow: a reject is recorded as
// edge.review_rejected; overturning a prior CONFIRMED decision records
// that as the previous state.
func TestRejectEdge_WritesAuditRow(t *testing.T) {
	srv, database, auditSvc := newReviewTestServer(t)
	locID := seedAuditLocation(t, database)
	seed := reviewEdgeSeed{tier: 3, resolver: "phash", confidence: 0.6, state: "CONFIRMED"}
	edgeID, _ := seedReviewEdge(t, database, locID, seed)

	if rr := serve(srv, adminReq(http.MethodPost, fmt.Sprintf("/api/v1/edges/%d/reject", edgeID), nil)); rr.Code != http.StatusOK {
		t.Fatalf("reject: %d %s", rr.Code, rr.Body.String())
	}
	assertEdgeReviewAuditRow(t, database, auditSvc, audit.EventEdgeReviewRejected, edgeID, seed.resolver, "CONFIRMED", "REJECTED")
	if n := countAuditRows(t, auditSvc, audit.EventEdgeReviewConfirmed); n != 0 {
		t.Errorf("reject wrote %d edge.review_confirmed rows, want 0", n)
	}
}

// TestEdgeReview_UnknownEdgeWritesNoAuditRow: a 404 confirm/reject
// changes nothing and so records nothing, like a refused reset.
func TestEdgeReview_UnknownEdgeWritesNoAuditRow(t *testing.T) {
	srv, _, auditSvc := newReviewTestServer(t)
	for _, action := range []string{"confirm", "reject"} {
		if rr := serve(srv, adminReq(http.MethodPost, "/api/v1/edges/999999/"+action, nil)); rr.Code != http.StatusNotFound {
			t.Errorf("%s unknown edge: status = %d, want 404", action, rr.Code)
		}
	}
	for _, ev := range []string{audit.EventEdgeReviewConfirmed, audit.EventEdgeReviewRejected} {
		if n := countAuditRows(t, auditSvc, ev); n != 0 {
			t.Errorf("%s rows after 404s = %d, want 0", ev, n)
		}
	}
}

// createEdgeReq is an admin POST /api/v1/edges for a manual link.
func createEdgeReq(t *testing.T, sourceID, targetID int64, relationship string) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{"sourceNodeId": sourceID, "targetNodeId": targetID, "relationshipType": relationship})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return adminReq(http.MethodPost, "/api/v1/edges", body)
}

// seededEdgeEndpoints returns the (source, target) node ids of a seeded edge.
func seededEdgeEndpoints(t *testing.T, database *db.DB, edgeID int64) (int64, int64) {
	t.Helper()
	edge, err := database.Reader.GetMediaEdge(context.Background(), edgeID)
	if err != nil {
		t.Fatalf("GetMediaEdge(%d): %v", edgeID, err)
	}
	return edge.SourceNodeID, edge.TargetNodeID
}

// TestCreateEdge_WritesAuditRow (#585): a manual link is born CONFIRMED,
// so it is recorded as edge.review_confirmed with source=manual. A fresh
// insert has no prior row and so no previousState key.
func TestCreateEdge_WritesAuditRow(t *testing.T) {
	srv, database, auditSvc := newReviewTestServer(t)
	locID := seedAuditLocation(t, database)
	// Seed a PROXY_OF edge only to get a node pair; the manual link below
	// is a different (DERIVED_FROM) triple, so it is a fresh insert.
	seedID, _ := seedReviewEdge(t, database, locID, reviewEdgeSeed{relationship: "PROXY_OF", tier: 2, resolver: "stem", confidence: 0.7})
	sourceID, targetID := seededEdgeEndpoints(t, database, seedID)

	rr := serve(srv, createEdgeReq(t, sourceID, targetID, "DERIVED_FROM"))
	if rr.Code != http.StatusOK {
		t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
	}
	var created edgeDTO
	if err := json.Unmarshal(rr.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	details := assertEdgeReviewAuditRow(t, database, auditSvc, audit.EventEdgeReviewConfirmed, created.ID, "manual", nil, "CONFIRMED")
	if details["source"] != "manual" {
		t.Errorf("details.source = %v, want manual", details["source"])
	}
	for _, k := range []string{"previousReviewedBy", "previousReviewedAt"} {
		if v, ok := details[k]; ok {
			t.Errorf("details.%s = %v, want absent on a fresh insert", k, v)
		}
	}
	if details["sourceNodeId"] != float64(sourceID) || details["targetNodeId"] != float64(targetID) {
		t.Errorf("details endpoints = %v -> %v, want %d -> %d", details["sourceNodeId"], details["targetNodeId"], sourceID, targetID)
	}
	if n := countAuditRows(t, auditSvc, audit.EventEdgeReviewRejected); n != 0 {
		t.Errorf("create wrote %d edge.review_rejected rows, want 0", n)
	}
}

// TestCreateEdge_OverExistingEdgeRecordsPreviousState: linking by hand a
// triple that already exists upserts that row; the audit row records the
// state the upsert replaced and, like a reset, who last reviewed it and
// when (#587), so re-linking over a human decision keeps its attribution.
// An unreviewed resolver guess has no reviewer, so carries neither key.
func TestCreateEdge_OverExistingEdgeRecordsPreviousState(t *testing.T) {
	cases := []struct {
		name  string
		state string // seeded prior state; CONFIRMED is reviewed by carol
	}{
		{"resolver guess", ""},
		{"human decision", "CONFIRMED"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv, database, auditSvc := newReviewTestServer(t)
			locID := seedAuditLocation(t, database)
			edgeID, _ := seedReviewEdge(t, database, locID, reviewEdgeSeed{tier: 2, resolver: "stem", confidence: 0.7, state: tc.state})
			sourceID, targetID := seededEdgeEndpoints(t, database, edgeID)
			prior, err := database.Reader.GetMediaEdge(context.Background(), edgeID)
			if err != nil {
				t.Fatalf("GetMediaEdge(%d): %v", edgeID, err)
			}

			if rr := serve(srv, createEdgeReq(t, sourceID, targetID, "DERIVED_FROM")); rr.Code != http.StatusOK {
				t.Fatalf("create: %d %s", rr.Code, rr.Body.String())
			}
			details := assertEdgeReviewAuditRow(t, database, auditSvc, audit.EventEdgeReviewConfirmed, edgeID, "manual", prior.ReviewState, "CONFIRMED")
			if details["source"] != "manual" {
				t.Errorf("details.source = %v, want manual", details["source"])
			}

			if tc.state == "" {
				if prior.ReviewState != "NEEDS_REVIEW" {
					t.Fatalf("seeded prior state = %q, want NEEDS_REVIEW", prior.ReviewState)
				}
				for _, k := range []string{"previousReviewedBy", "previousReviewedAt"} {
					if v, ok := details[k]; ok {
						t.Errorf("details.%s = %v, want absent (prior row never reviewed)", k, v)
					}
				}
				return
			}
			if !prior.ReviewedBy.Valid || !prior.ReviewedAt.Valid {
				t.Fatalf("seeded prior row not reviewed: %+v", prior)
			}
			if details["previousReviewedBy"] != "carol" {
				t.Errorf("details.previousReviewedBy = %v, want carol", details["previousReviewedBy"])
			}
			if details["previousReviewedAt"] != float64(prior.ReviewedAt.Int64) {
				t.Errorf("details.previousReviewedAt = %v, want %d", details["previousReviewedAt"], prior.ReviewedAt.Int64)
			}
		})
	}
}

// TestCreateEdge_RefusedCreateWritesNoAuditRow: validation, missing-node
// and cycle refusals create nothing and so record nothing.
func TestCreateEdge_RefusedCreateWritesNoAuditRow(t *testing.T) {
	srv, database, auditSvc := newReviewTestServer(t)
	locID := seedAuditLocation(t, database)
	edgeID, _ := seedReviewEdge(t, database, locID, reviewEdgeSeed{tier: 2, resolver: "stem", confidence: 0.7, state: "CONFIRMED"})
	sourceID, targetID := seededEdgeEndpoints(t, database, edgeID)

	cases := []struct {
		name           string
		source, target int64
		relationship   string
		want           int
	}{
		{"self-loop", sourceID, sourceID, "DERIVED_FROM", http.StatusUnprocessableEntity},
		{"invalid relationship", sourceID, targetID, "NOT_A_TYPE", http.StatusUnprocessableEntity},
		{"unknown target", sourceID, 999999, "DERIVED_FROM", http.StatusNotFound},
		{"unknown source", 999999, targetID, "DERIVED_FROM", http.StatusNotFound},
		// The seeded CONFIRMED edge is source -> target; the reverse closes a cycle.
		{"cycle", targetID, sourceID, "DERIVED_FROM", http.StatusConflict},
	}
	for _, tc := range cases {
		if rr := serve(srv, createEdgeReq(t, tc.source, tc.target, tc.relationship)); rr.Code != tc.want {
			t.Errorf("%s: status = %d, want %d (%s)", tc.name, rr.Code, tc.want, rr.Body.String())
		}
	}
	if n := countAuditRows(t, auditSvc, audit.EventEdgeReviewConfirmed); n != 0 {
		t.Errorf("edge.review_confirmed rows after refused creates = %d, want 0", n)
	}
}
