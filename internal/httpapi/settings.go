package httpapi

import (
	"context"
	"errors"
	"fmt"

	"github.com/danielgtaylor/huma/v2"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/secrets"
	"github.com/s3ntin3l8/branchdam/internal/settings"
)

// requireSettingsAdmin gates both GET and PUT on the same policy: a real,
// authenticated user principal who is a member of an allowed admin group
// (or every allowed group is empty, the solo-homelab default). This is
// deliberately stricter than the global auth.RequireAdmin middleware every
// other route runs under, in two ways: RequireAdmin passes ANY browser
// principal (even unauthenticated) on GET, and passes a machine principal
// unconditionally regardless of method. Both are wrong here -- an
// unauthenticated reader must not see the whole resolved config, and an
// agent holding only its API key must never read or write settings, on any
// method. See docs/configuration.md's precedence section.
func (s *Server) requireSettingsAdmin(ctx context.Context) error {
	p, ok := auth.From(ctx)
	if !ok {
		return huma.Error403Forbidden("authentication required")
	}
	if p.Kind == auth.KindMachine {
		return huma.Error403Forbidden("agent principals may not access settings")
	}
	var allowedGroups []string
	if cfg := s.cfg(); cfg != nil {
		allowedGroups = cfg.Authz.Groups
	}
	if !auth.IsAdmin(p, allowedGroups) {
		return huma.Error403Forbidden("admin authorization required")
	}
	return nil
}

// principalName returns the current request's principal name, or "" for
// an unauthenticated/machine principal -- app_settings.updated_by's
// NOT NULL DEFAULT ” column, unlike media_edges.reviewed_by, is never
// nullable, so this returns a plain string rather than reviewerName's
// sql.NullString.
func principalName(ctx context.Context) string {
	p, ok := auth.From(ctx)
	if !ok {
		return ""
	}
	return p.Name
}

type SettingsFieldDTO struct {
	Key   string `json:"key"`
	Type  string `json:"type"`
	Label string `json:"label"`
	Group string `json:"group"`
	// Value is omitted entirely for a secret field -- see HasValue.
	Value any `json:"value,omitempty"`
	// Source is "override" (a live app_settings row) or "config" (the
	// config.yaml/.env base value, whether or not config.yaml set it
	// explicitly -- internal/config.Load tracks no finer provenance than
	// that, see internal/settings' doc comment on the boundary this stops
	// short of).
	Source         string `json:"source"`
	ApplyMode      string `json:"applyMode"`
	Secret         bool   `json:"secret"`
	HasValue       bool   `json:"hasValue,omitempty"`
	Editable       bool   `json:"editable"`
	ReadOnlyReason string `json:"readOnlyReason,omitempty"`
	PendingRestart bool   `json:"pendingRestart,omitempty"`
	Doc            string `json:"doc,omitempty"`
}

type GetSettingsOutput struct {
	Body struct {
		Fields           []SettingsFieldDTO `json:"fields"`
		PendingRestart   []string           `json:"pendingRestart"`
		SecretsAvailable bool               `json:"secretsAvailable"`
	}
}

func (s *Server) buildSettingsOutput(ctx context.Context) (*GetSettingsOutput, error) {
	if err := s.requireSettingsAdmin(ctx); err != nil {
		return nil, err
	}
	if s.settingsStore == nil {
		return nil, huma.Error500InternalServerError("settings store not configured", nil)
	}

	cfg := s.settingsStore.Effective()
	pending := s.settingsStore.PendingRestart()
	pendingSet := make(map[string]bool, len(pending))
	for _, k := range pending {
		pendingSet[k] = true
	}

	fields := settings.Fields()
	out := &GetSettingsOutput{}
	out.Body.Fields = make([]SettingsFieldDTO, 0, len(fields))
	for _, f := range fields {
		dto := SettingsFieldDTO{
			Key:            f.Key,
			Type:           f.Type.String(),
			Label:          f.Label,
			Group:          f.Group,
			ApplyMode:      f.Apply.String(),
			Secret:         f.Secret,
			Editable:       f.Editable,
			ReadOnlyReason: f.ReadOnlyReason,
			PendingRestart: pendingSet[f.Key],
			Doc:            f.Doc,
		}
		if s.settingsStore.IsOverridden(f.Key) {
			dto.Source = "override"
		} else {
			dto.Source = "config"
		}

		val := f.Get(cfg)
		if f.Secret {
			if str, ok := val.(string); ok {
				dto.HasValue = str != ""
			}
		} else {
			dto.Value = val
		}
		out.Body.Fields = append(out.Body.Fields, dto)
	}
	if pending == nil {
		pending = make([]string, 0)
	}
	out.Body.PendingRestart = pending
	out.Body.SecretsAvailable = s.settingsStore.SecretsAvailable()
	return out, nil
}

func (s *Server) handleGetSettings(ctx context.Context, _ *struct{}) (*GetSettingsOutput, error) {
	return s.buildSettingsOutput(ctx)
}

type PutSettingsInput struct {
	Body struct {
		Set   map[string]any `json:"set,omitempty"`
		Unset []string       `json:"unset,omitempty"`
	}
}

// normalizeSettingValue converts a JSON-decoded value (numbers always
// arrive as float64, lists as []any, regardless of the field's declared
// Kind) into the Go type internal/settings' Field.Get/Set/Validate expect.
// This keeps internal/settings itself free of any wire-format concern --
// see its Field.Set doc comment.
func normalizeSettingValue(f settings.Field, v any) (any, error) {
	switch f.Type {
	case settings.KindInt:
		switch n := v.(type) {
		case float64:
			return int(n), nil
		case int:
			return n, nil
		default:
			return nil, fmt.Errorf("must be a number")
		}
	case settings.KindBool:
		b, ok := v.(bool)
		if !ok {
			return nil, fmt.Errorf("must be a boolean")
		}
		return b, nil
	case settings.KindString:
		str, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("must be a string")
		}
		return str, nil
	case settings.KindStringList:
		arr, ok := v.([]any)
		if !ok {
			return nil, fmt.Errorf("must be a list of strings")
		}
		out := make([]string, len(arr))
		for i, item := range arr {
			str, ok := item.(string)
			if !ok {
				return nil, fmt.Errorf("item %d must be a string", i)
			}
			out[i] = str
		}
		return out, nil
	default:
		return v, nil
	}
}

func (s *Server) handlePutSettings(ctx context.Context, in *PutSettingsInput) (*GetSettingsOutput, error) {
	if err := s.requireSettingsAdmin(ctx); err != nil {
		return nil, err
	}
	if s.settingsStore == nil {
		return nil, huma.Error500InternalServerError("settings store not configured", nil)
	}

	set := make(map[string]any, len(in.Body.Set))
	for key, raw := range in.Body.Set {
		field, ok := settings.Lookup(key)
		if !ok {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("unknown field %q", key))
		}
		norm, err := normalizeSettingValue(field, raw)
		if err != nil {
			return nil, huma.Error422UnprocessableEntity(fmt.Sprintf("field %q: %v", key, err))
		}
		set[key] = norm
	}

	if err := s.settingsStore.Apply(ctx, set, in.Body.Unset, principalName(ctx)); err != nil {
		// secrets.ErrUnavailable is itself wrapped inside ErrInvalidInput
		// (Store.Apply's seal-failure branch), so this check must come
		// first -- otherwise it's unreachable and the operator only ever
		// sees ErrInvalidInput's generic message instead of the actionable
		// "set BRANCHDAM_SECRET_KEY" one.
		if errors.Is(err, secrets.ErrUnavailable) {
			return nil, huma.Error422UnprocessableEntity("secret storage unavailable: set BRANCHDAM_SECRET_KEY")
		}
		if errors.Is(err, settings.ErrInvalidInput) {
			return nil, huma.Error422UnprocessableEntity(err.Error())
		}
		return nil, huma.Error500InternalServerError("apply settings", err)
	}

	if s.hub != nil {
		s.hub.Broadcast()
	}

	return s.buildSettingsOutput(ctx)
}
