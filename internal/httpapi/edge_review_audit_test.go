package httpapi

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/s3ntin3l8/branchdam/internal/audit"
)

// assertEdgeReviewAuditRow reads back the single actor_audit row for event
// and checks the resource, actor and details shape shared with
// edge.review_reset.
func assertEdgeReviewAuditRow(t *testing.T, auditSvc *audit.Service, event string, edgeID int64, seed reviewEdgeSeed, previousState, reviewState string) {
	t.Helper()
	entries, _, err := auditSvc.ListActivity(context.Background(), audit.Filter{Event: event}, 10, 0)
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
	var details map[string]any
	if err := json.Unmarshal([]byte(e.DetailsJSON), &details); err != nil {
		t.Fatalf("details: %v", err)
	}
	if details["edgeId"] != float64(edgeID) || details["relationshipType"] != "DERIVED_FROM" ||
		details["resolver"] != seed.resolver || details["previousState"] != previousState || details["reviewState"] != reviewState {
		t.Errorf("details = %v", details)
	}
	if _, ok := details["sourceNodeId"].(float64); !ok {
		t.Errorf("details.sourceNodeId missing: %v", details)
	}
	if _, ok := details["targetNodeId"].(float64); !ok {
		t.Errorf("details.targetNodeId missing: %v", details)
	}
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
	assertEdgeReviewAuditRow(t, auditSvc, audit.EventEdgeReviewConfirmed, edgeID, seed, "NEEDS_REVIEW", "CONFIRMED")
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
	assertEdgeReviewAuditRow(t, auditSvc, audit.EventEdgeReviewRejected, edgeID, seed, "CONFIRMED", "REJECTED")
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
