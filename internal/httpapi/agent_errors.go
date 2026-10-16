package httpapi

import (
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"

	"github.com/s3ntin3l8/branchdam/internal/apierr"
	"github.com/s3ntin3l8/branchdam/internal/auth"
)

// agentErrorCodeTransformer gives every error body an agent operation
// returns a stable apierr code. Agent handlers raise *apierr.Problem with an
// explicit code; this catches the rest -- chiefly Huma's own request
// validation and body-parsing errors, which are plain *huma.ErrorModel --
// and assigns apierr.CodeForStatus. Status and the rest of the body are
// untouched. Non-agent operations are passed through unchanged.
//
// It must run before Huma's schema-link transformer (see Handler), which
// would otherwise have rewritten the *huma.ErrorModel into an anonymous
// struct this type switch no longer recognises.
func agentErrorCodeTransformer(ctx huma.Context, _ string, v any) (any, error) {
	em, ok := v.(*huma.ErrorModel)
	if !ok || em == nil {
		return v, nil
	}
	op := ctx.Operation()
	if op == nil || !strings.HasPrefix(op.Path, auth.AgentPathPrefix) {
		return v, nil
	}
	status := em.Status
	if status == 0 {
		status = http.StatusInternalServerError
	}
	return apierr.FromModel(em, apierr.CodeForStatus(status)), nil
}

// legacyUploadProblem is the error body of the raw upload endpoints: the
// apierr problem+json envelope plus the legacy "error" field (same text as
// detail) that upload clients read before the envelope existed.
type legacyUploadProblem struct {
	*apierr.Problem
	Error string `json:"error"`
}

// writeUploadProblem writes p for POST /api/v1/agent/upload (and the
// browser upload sharing writeUploadError), keeping the legacy "error" key.
func writeUploadProblem(w http.ResponseWriter, p *apierr.Problem) {
	apierr.WriteBody(w, p.Status, legacyUploadProblem{Problem: p, Error: p.Detail})
}
