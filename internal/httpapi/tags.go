package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/danielgtaylor/huma/v2"
	"github.com/mattn/go-sqlite3"

	"github.com/s3ntin3l8/branchdam/internal/audit"
	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

const (
	tagMaxRunes         = 64
	descriptionMaxRunes = 4000
	modelMaxRunes       = 128
)

// enrichmentPATScope reports the PAT route group for tag and annotation
// routes. GET is tags-read (mcp:read or mcp:enrich). Other methods are
// tags-write (mcp:enrich). Admin and "*" still pass those groups.
func enrichmentPATScope(method, path string) (string, bool) {
	if !isEnrichmentAPI(path) {
		return "", false
	}
	if method == http.MethodGet || method == http.MethodHead {
		return "tags-read", true
	}
	return "tags-write", true
}

func isEnrichmentAPI(path string) bool {
	if path == "/api/v1/tags" || path == "/api/v1/tags/revert" {
		return true
	}
	rest, ok := strings.CutPrefix(path, "/api/v1/assets/")
	if !ok {
		return false
	}
	parts := strings.Split(rest, "/")
	if len(parts) < 2 || parts[0] == "" {
		return false
	}
	switch parts[1] {
	case "tags":
		return len(parts) == 2 || len(parts) == 3
	case "annotations":
		return len(parts) == 2 || (len(parts) == 3 && parts[2] == "description")
	default:
		return false
	}
}

type tagDTO struct {
	ID              int64  `json:"id"`
	NodeID          int64  `json:"nodeId"`
	Tag             string `json:"tag"`
	Source          string `json:"source"`
	CreatedByUserID int64  `json:"createdByUserId"`
	PatID           *int64 `json:"patId,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
}

type annotationDTO struct {
	ID              int64  `json:"id"`
	NodeID          int64  `json:"nodeId"`
	Kind            string `json:"kind"`
	Text            string `json:"text"`
	Source          string `json:"source"`
	Model           string `json:"model,omitempty"`
	CreatedByUserID int64  `json:"createdByUserId"`
	PatID           *int64 `json:"patId,omitempty"`
	CreatedAt       int64  `json:"createdAt"`
	UpdatedAt       int64  `json:"updatedAt"`
}

func toTagDTO(row sqlcgen.AssetTag) tagDTO {
	dto := tagDTO{
		ID: row.ID, NodeID: row.NodeID, Tag: row.Tag, Source: row.Source,
		CreatedByUserID: row.CreatedByUserID, CreatedAt: row.CreatedAt,
	}
	if row.PatID.Valid {
		v := row.PatID.Int64
		dto.PatID = &v
	}
	return dto
}

func toAnnotationDTO(row sqlcgen.AssetAnnotation) annotationDTO {
	dto := annotationDTO{
		ID: row.ID, NodeID: row.NodeID, Kind: row.Kind, Text: row.Text, Source: row.Source,
		CreatedByUserID: row.CreatedByUserID, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
	}
	if row.Model.Valid {
		dto.Model = row.Model.String
	}
	if row.PatID.Valid {
		v := row.PatID.Int64
		dto.PatID = &v
	}
	return dto
}

type enrichmentActor struct {
	Source string
	UserID int64
	PatID  sql.NullInt64
}

func (s *Server) enrichmentActor(ctx context.Context) (enrichmentActor, error) {
	p, ok := auth.From(ctx)
	if !ok || !p.Authenticated || p.Kind != auth.KindUser {
		return enrichmentActor{}, huma.Error401Unauthorized("authentication required")
	}
	uid := p.UserID
	if uid == 0 {
		if s.attribution == nil {
			return enrichmentActor{}, huma.Error500InternalServerError("attribution is not configured", nil)
		}
		row, err := s.attribution.ResolveOrCreate(ctx, p)
		if err != nil || row.ID == 0 {
			return enrichmentActor{}, huma.Error401Unauthorized("authentication required")
		}
		uid = row.ID
	}
	var patID sql.NullInt64
	if p.PATID != 0 {
		patID = sql.NullInt64{Int64: p.PATID, Valid: true}
	}
	source := "user"
	if p.AuthProvider == auth.AuthProviderOAuth || mcpOnlyToken(ctx) {
		source = "assistant"
	}
	return enrichmentActor{Source: source, UserID: uid, PatID: patID}, nil
}

func mcpOnlyToken(ctx context.Context) bool {
	scopes, ok := auth.PATScopesFrom(ctx)
	if !ok || len(scopes) == 0 {
		return false
	}
	for _, scope := range scopes {
		if !strings.HasPrefix(scope, "mcp:") {
			return false
		}
	}
	return true
}

func (s *Server) enrichmentNode(ctx context.Context, id int64) error {
	node, err := s.db.Reader.GetMediaNodeByID(ctx, id)
	if errors.Is(err, sql.ErrNoRows) {
		return huma.Error404NotFound("asset not found")
	}
	if err != nil {
		return huma.Error500InternalServerError("get asset", err)
	}
	if node.LifecycleState == "ARCHIVED" {
		return huma.Error404NotFound("asset not found")
	}
	return nil
}

func normalizeTag(raw string) (string, error) {
	tag := strings.ToLower(strings.TrimSpace(raw))
	if tag == "" || utf8.RuneCountInString(tag) > tagMaxRunes {
		return "", huma.Error400BadRequest("tag must be 1 to 64 characters")
	}
	if strings.Contains(tag, "/") {
		return "", huma.Error400BadRequest("tag must not contain /")
	}
	for _, r := range tag {
		if unicode.IsControl(r) {
			return "", huma.Error400BadRequest("tag must not contain control characters")
		}
	}
	return tag, nil
}

func tagQueryParam(raw string) (sql.NullString, error) {
	if strings.TrimSpace(raw) == "" {
		return sql.NullString{}, nil
	}
	tag, err := normalizeTag(raw)
	if err != nil {
		return sql.NullString{}, err
	}
	return sql.NullString{String: tag, Valid: true}, nil
}

func normalizeDescription(raw string) (string, error) {
	text := strings.TrimSpace(raw)
	if text == "" || utf8.RuneCountInString(text) > descriptionMaxRunes {
		return "", huma.Error400BadRequest("description must be 1 to 4000 characters")
	}
	return text, nil
}

type ListAssetTagsOutput struct {
	Body struct {
		Tags []tagDTO `json:"tags"`
	}
}

func (s *Server) handleListAssetTags(ctx context.Context, in *AssetPathInput) (*ListAssetTagsOutput, error) {
	if err := s.enrichmentNode(ctx, in.ID); err != nil {
		return nil, err
	}
	rows, err := s.db.Reader.ListAssetTags(ctx, in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("list tags", err)
	}
	out := &ListAssetTagsOutput{}
	out.Body.Tags = make([]tagDTO, len(rows))
	for i, row := range rows {
		out.Body.Tags[i] = toTagDTO(row)
	}
	return out, nil
}

type AddAssetTagInput struct {
	ID   int64 `path:"id"`
	Body struct {
		Tag string `json:"tag"`
		// Source is accepted and ignored. The server derives user vs assistant.
		Source string `json:"source,omitempty"`
	}
}

type AddAssetTagOutput struct {
	Body tagDTO
}

func (s *Server) handleAddAssetTag(ctx context.Context, in *AddAssetTagInput) (*AddAssetTagOutput, error) {
	tag, err := normalizeTag(in.Body.Tag)
	if err != nil {
		return nil, err
	}
	if err := s.enrichmentNode(ctx, in.ID); err != nil {
		return nil, err
	}
	actor, err := s.enrichmentActor(ctx)
	if err != nil {
		return nil, err
	}
	var row sqlcgen.AssetTag
	inserted := false
	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		existing, err := q.GetAssetTag(ctx, sqlcgen.GetAssetTagParams{NodeID: in.ID, Tag: tag})
		if err == nil {
			row = existing
			return nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		row, err = q.InsertAssetTag(ctx, sqlcgen.InsertAssetTagParams{
			NodeID: in.ID, Tag: tag, Source: actor.Source,
			CreatedByUserID: actor.UserID, PatID: actor.PatID,
		})
		if err != nil {
			if isUniqueViolation(err) {
				row, err = q.GetAssetTag(ctx, sqlcgen.GetAssetTagParams{NodeID: in.ID, Tag: tag})
				return err
			}
			return err
		}
		inserted = true
		return nil
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("add tag", err)
	}
	if inserted {
		if err := s.writeEnrichmentAudit(ctx, audit.EventTagAdded, in.ID, map[string]any{
			"tag": tag, "source": row.Source,
		}); err != nil {
			return nil, err
		}
	}
	return &AddAssetTagOutput{Body: toTagDTO(row)}, nil
}

type DeleteAssetTagInput struct {
	ID  int64  `path:"id"`
	Tag string `path:"tag"`
}

type DeleteAssetTagOutput struct {
	Body struct {
		OK bool `json:"ok"`
	}
}

func (s *Server) handleDeleteAssetTag(ctx context.Context, in *DeleteAssetTagInput) (*DeleteAssetTagOutput, error) {
	tag, err := normalizeTag(in.Tag)
	if err != nil {
		return nil, err
	}
	if err := s.enrichmentNode(ctx, in.ID); err != nil {
		return nil, err
	}
	if _, err := s.enrichmentActor(ctx); err != nil {
		return nil, err
	}
	var n int64
	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		n, err = q.DeleteAssetTag(ctx, sqlcgen.DeleteAssetTagParams{NodeID: in.ID, Tag: tag})
		return err
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("delete tag", err)
	}
	if n == 0 {
		return nil, huma.Error404NotFound("tag not found")
	}
	if err := s.writeEnrichmentAudit(ctx, audit.EventTagRemoved, in.ID, map[string]any{"tag": tag}); err != nil {
		return nil, err
	}
	return &DeleteAssetTagOutput{Body: struct {
		OK bool `json:"ok"`
	}{OK: true}}, nil
}

type ListAnnotationsOutput struct {
	Body struct {
		Annotations []annotationDTO `json:"annotations"`
	}
}

func (s *Server) handleListAssetAnnotations(ctx context.Context, in *AssetPathInput) (*ListAnnotationsOutput, error) {
	if err := s.enrichmentNode(ctx, in.ID); err != nil {
		return nil, err
	}
	rows, err := s.db.Reader.ListAssetAnnotations(ctx, in.ID)
	if err != nil {
		return nil, huma.Error500InternalServerError("list annotations", err)
	}
	out := &ListAnnotationsOutput{}
	out.Body.Annotations = make([]annotationDTO, len(rows))
	for i, row := range rows {
		out.Body.Annotations[i] = toAnnotationDTO(row)
	}
	return out, nil
}

type PutDescriptionInput struct {
	ID   int64 `path:"id"`
	Body struct {
		Text  string `json:"text"`
		Model string `json:"model,omitempty"`
		// Source is accepted and ignored. The server derives user vs assistant.
		Source string `json:"source,omitempty"`
	}
}

type PutDescriptionOutput struct {
	Body annotationDTO
}

func (s *Server) handlePutAssetDescription(ctx context.Context, in *PutDescriptionInput) (*PutDescriptionOutput, error) {
	text, err := normalizeDescription(in.Body.Text)
	if err != nil {
		return nil, err
	}
	model := strings.TrimSpace(in.Body.Model)
	if utf8.RuneCountInString(model) > modelMaxRunes {
		return nil, huma.Error400BadRequest("model must be at most 128 characters")
	}
	if err := s.enrichmentNode(ctx, in.ID); err != nil {
		return nil, err
	}
	actor, err := s.enrichmentActor(ctx)
	if err != nil {
		return nil, err
	}
	var modelArg sql.NullString
	if model != "" {
		modelArg = sql.NullString{String: model, Valid: true}
	}
	var row sqlcgen.AssetAnnotation
	err = s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		row, err = q.UpsertAssetDescription(ctx, sqlcgen.UpsertAssetDescriptionParams{
			NodeID: in.ID, Text: text, Source: actor.Source, Model: modelArg,
			CreatedByUserID: actor.UserID, PatID: actor.PatID,
		})
		return err
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("set description", err)
	}
	if err := s.writeEnrichmentAudit(ctx, audit.EventDescriptionSet, in.ID, map[string]any{
		"source": row.Source,
	}); err != nil {
		return nil, err
	}
	return &PutDescriptionOutput{Body: toAnnotationDTO(row)}, nil
}

type ListTagsOutput struct {
	Body struct {
		Tags []struct {
			Tag   string `json:"tag"`
			Count int64  `json:"count"`
		} `json:"tags"`
	}
}

func (s *Server) handleListTags(ctx context.Context, _ *struct{}) (*ListTagsOutput, error) {
	rows, err := s.db.Reader.ListTagCounts(ctx)
	if err != nil {
		return nil, huma.Error500InternalServerError("list tags", err)
	}
	out := &ListTagsOutput{}
	out.Body.Tags = make([]struct {
		Tag   string `json:"tag"`
		Count int64  `json:"count"`
	}, len(rows))
	for i, row := range rows {
		out.Body.Tags[i].Tag = row.Tag
		out.Body.Tags[i].Count = row.Count
	}
	return out, nil
}

type RevertAssistantTagsInput struct {
	Body struct {
		Source string `json:"source"`
		PatID  int64  `json:"patId,omitempty"`
		Since  int64  `json:"since,omitempty"`
	}
}

type RevertAssistantTagsOutput struct {
	Body struct {
		TagsRemoved        int64 `json:"tagsRemoved"`
		AnnotationsRemoved int64 `json:"annotationsRemoved"`
	}
}

func (s *Server) handleRevertAssistantTags(ctx context.Context, in *RevertAssistantTagsInput) (*RevertAssistantTagsOutput, error) {
	if err := s.requireSettingsAdmin(ctx); err != nil {
		return nil, err
	}
	if in.Body.Source != "assistant" {
		return nil, huma.Error400BadRequest("revert source must be assistant")
	}
	var patID sql.NullInt64
	if in.Body.PatID > 0 {
		patID = sql.NullInt64{Int64: in.Body.PatID, Valid: true}
	}
	var since sql.NullInt64
	if in.Body.Since > 0 {
		since = sql.NullInt64{Int64: in.Body.Since, Valid: true}
	}
	params := sqlcgen.DeleteAssistantTagsParams{PatID: patID, SinceUnix: since}
	var tagsRemoved, notesRemoved int64
	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		var err error
		tagsRemoved, err = q.DeleteAssistantTags(ctx, params)
		if err != nil {
			return err
		}
		notesRemoved, err = q.DeleteAssistantAnnotations(ctx, sqlcgen.DeleteAssistantAnnotationsParams{
			PatID: patID, SinceUnix: since,
		})
		return err
	})
	if err != nil {
		return nil, huma.Error500InternalServerError("revert assistant tags", err)
	}
	if err := s.writeEnrichmentAudit(ctx, audit.EventTagsReverted, 0, map[string]any{
		"patId": in.Body.PatID, "since": in.Body.Since,
		"tagsRemoved": tagsRemoved, "annotationsRemoved": notesRemoved,
	}); err != nil {
		return nil, err
	}
	out := &RevertAssistantTagsOutput{}
	out.Body.TagsRemoved = tagsRemoved
	out.Body.AnnotationsRemoved = notesRemoved
	return out, nil
}

func (s *Server) writeEnrichmentAudit(ctx context.Context, event string, nodeID int64, details map[string]any) error {
	if s.audit == nil {
		return nil
	}
	resourceID := ""
	if nodeID != 0 {
		resourceID = strconv.FormatInt(nodeID, 10)
	}
	if err := s.audit.WriteActorAudit(ctx, principalFromCtx(ctx), event, "asset", resourceID, details); err != nil {
		return huma.Error500InternalServerError("write audit", err)
	}
	return nil
}

func isUniqueViolation(err error) bool {
	var se sqlite3.Error
	return errors.As(err, &se) && (se.ExtendedCode == sqlite3.ErrConstraintUnique || se.Code == sqlite3.ErrConstraint)
}
