package settings

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/s3ntin3l8/branchdam/internal/config"
	"github.com/s3ntin3l8/branchdam/internal/db"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
	"github.com/s3ntin3l8/branchdam/internal/secrets"
)

// ErrUnknownField is returned by Apply when set/unset names a key with no
// registered Field.
var ErrUnknownField = errors.New("settings: unknown field")

// ErrNotEditable is returned by Apply when set names a Field whose
// Editable is false (authz.groups, listenAddr, database.path, ...).
var ErrNotEditable = errors.New("settings: field is not editable")

// ErrInvalidInput wraps every Apply failure that happens before any
// database write is attempted -- unknown field, non-editable field, a
// Validate failure, a type mismatch, or an encode/seal failure. A caller
// (internal/httpapi) uses this single check to distinguish a client input
// problem (-> 422) from a genuine backend failure during the write itself
// (-> 500), without needing to enumerate every specific error above.
var ErrInvalidInput = errors.New("settings: invalid input")

// Store holds the resolved effective config.Config and mediates every
// override read/write against app_settings. base is the config.yaml/.env
// config as loaded at process start -- Store never mutates it; Resolve
// always builds a fresh copy.
type Store struct {
	log  *slog.Logger
	db   *db.DB
	box  *secrets.Box
	base config.Config

	effective atomic.Pointer[config.Config]

	// overridden tracks which registered keys currently have a live,
	// successfully-decoded-and-validated app_settings row -- this is what
	// the settings API's "source: override" vs "source: config" field
	// (docs/configuration.md) is derived from. A row that reload() dropped
	// (bad decode, failed validation, unavailable secret key) is correctly
	// absent here too, since its value never made it into overrides.
	overridden atomic.Pointer[map[string]bool]

	// bootEffective is the effective config computed once, at NewStore
	// time -- the snapshot PendingRestart diffs the current effective
	// config against, so a value that was already an override before this
	// process started never shows as "pending" (nothing changed since boot).
	bootEffective *config.Config

	// applyMu serializes Apply: reload-validate-write-reload must run as one
	// unit.
	applyMu sync.Mutex

	// subsMu guards subscribers, kept deliberately separate from applyMu:
	// Apply calls each subscriber synchronously (see Subscribe's doc
	// comment), and a subscriber that itself calls Subscribe or Apply must
	// not deadlock against the lock Apply is still holding.
	subsMu      sync.Mutex
	subscribers []func(*config.Config)
}

// NewStore loads any existing app_settings overrides, resolves them against
// base, and returns a Store whose Effective() reflects that resolution
// immediately -- there is no separate "call Reload before first use" step.
func NewStore(ctx context.Context, database *db.DB, base config.Config, box *secrets.Box, log *slog.Logger) (*Store, error) {
	if log == nil {
		log = slog.Default()
	}
	s := &Store{db: database, box: box, base: base, log: log}
	if err := s.reload(ctx); err != nil {
		return nil, err
	}
	s.bootEffective = s.Effective()
	return s, nil
}

// Effective returns the current resolved config.Config. Safe for
// concurrent use; callers must treat the returned pointer as read-only.
func (s *Store) Effective() *config.Config {
	return s.effective.Load()
}

// SecretsAvailable reports whether a secret box is configured -- false
// means BRANCHDAM_SECRET_KEY is unset, so any secret-typed field's Apply
// will fail with secrets.ErrUnavailable and Effective() may be serving a
// config/env fallback for one already stored encrypted. The settings API
// surfaces this so the UI can explain why a secret field looks unset or
// can't be changed, rather than the operator guessing.
func (s *Store) SecretsAvailable() bool {
	return s.box != nil
}

// Subscribe registers fn to be called, synchronously, with the new
// effective config every time Apply changes it. Intended for a domain that
// needs to react without a restart (internal/sync's Immich supervisor);
// fn is called after the write to app_settings has already committed, so a
// failure inside fn never rolls back the stored override -- see Apply's
// doc comment.
func (s *Store) Subscribe(fn func(*config.Config)) {
	s.subsMu.Lock()
	defer s.subsMu.Unlock()
	s.subscribers = append(s.subscribers, fn)
}

// PendingRestart returns the keys of every registered ApplyRestart field
// whose current effective value differs from the value effective at
// process boot -- the "applies on restart" banner's data source.
func (s *Store) PendingRestart() []string {
	boot := s.bootEffective
	cur := s.Effective()
	if boot == nil || cur == nil {
		return nil
	}
	var pending []string
	for _, f := range Fields() {
		if f.Apply != ApplyRestart {
			continue
		}
		if fmt.Sprint(f.Get(boot)) != fmt.Sprint(f.Get(cur)) {
			pending = append(pending, f.Key)
		}
	}
	return pending
}

// reload re-reads every app_settings row, decodes/decrypts/validates each
// against the registry, and atomically republishes Effective(). A row that
// fails to decode, fails validation, or is a secret with no key available
// is logged and skipped -- falling back to the config/env base value for
// that one field -- rather than aborting the whole reload. This is
// deliberate: a single bad or since-invalidated override must never brick
// startup or a later Apply (see docs/configuration.md's precedence
// section).
func (s *Store) reload(ctx context.Context) error {
	rows, err := s.db.Reader.ListAppSettings(ctx)
	if err != nil {
		return fmt.Errorf("settings: list app_settings: %w", err)
	}

	overrides := make(map[string]any, len(rows))
	for _, row := range rows {
		if strings.HasPrefix(row.Key, StorageLocationKeyPrefix) {
			// A different, dynamically-keyed namespace this package also
			// owns (see storagelocation.go) -- not a registry field, but
			// not a mistake either, so skip silently rather than warn.
			continue
		}
		field, ok := Lookup(row.Key)
		if !ok {
			s.log.Warn("settings: ignoring app_settings row for unregistered key", "key", row.Key)
			continue
		}

		val, err := s.decodeRow(field, row)
		if err != nil {
			s.log.Error("settings: dropping override, falling back to config/env value", "key", row.Key, "err", err)
			continue
		}

		if field.Validate != nil {
			if err := field.Validate(val); err != nil {
				s.log.Warn("settings: stored override fails validation, falling back to config/env value", "key", row.Key, "err", err)
				continue
			}
		}

		overrides[row.Key] = val
	}

	overridden := make(map[string]bool, len(overrides))
	for key := range overrides {
		overridden[key] = true
	}
	s.overridden.Store(&overridden)
	s.effective.Store(Resolve(&s.base, overrides, s.log))
	return nil
}

// IsOverridden reports whether key currently has a live app_settings
// override in effect (as opposed to its value coming from config.yaml/.env
// or a registered default).
func (s *Store) IsOverridden(key string) bool {
	m := s.overridden.Load()
	if m == nil {
		return false
	}
	return (*m)[key]
}

// decodeRow turns a stored row's JSON-encoded (and, for a secret field,
// encrypted) value column back into the Go value Field.Set expects.
func (s *Store) decodeRow(field Field, row sqlcgen.AppSetting) (any, error) {
	if row.IsSecret != 0 {
		if !field.Secret {
			return nil, fmt.Errorf("row is marked secret but field %q is not", field.Key)
		}
		if s.box == nil {
			return nil, fmt.Errorf("%w (secret rows exist but no key is configured)", secrets.ErrUnavailable)
		}
		var sealed string
		if err := json.Unmarshal([]byte(row.Value), &sealed); err != nil {
			return nil, fmt.Errorf("decode sealed value: %w", err)
		}
		plain, err := s.box.Open(sealed)
		if err != nil {
			return nil, err
		}
		return plain, nil
	}

	switch field.Type {
	case KindString:
		var v string
		err := json.Unmarshal([]byte(row.Value), &v)
		return v, err
	case KindInt:
		var v int
		err := json.Unmarshal([]byte(row.Value), &v)
		return v, err
	case KindBool:
		var v bool
		err := json.Unmarshal([]byte(row.Value), &v)
		return v, err
	case KindStringList:
		var v []string
		err := json.Unmarshal([]byte(row.Value), &v)
		return v, err
	case KindPathRewriteList:
		var v []config.PathRewrite
		err := json.Unmarshal([]byte(row.Value), &v)
		if v == nil {
			v = []config.PathRewrite{}
		}
		return v, err
	default:
		return nil, fmt.Errorf("unknown field kind %v", field.Type)
	}
}

// Apply validates every entry in set and unset against the registry, then
// writes all of it in one database transaction, then reloads Effective()
// and finally notifies subscribers with the new config. Nothing is written
// if any single entry fails validation -- Apply either changes every
// requested key or none of them.
//
// Subscribers run after the transaction has already committed: if a
// subscriber (e.g. restarting the Immich worker) fails, the stored
// override is not rolled back -- there is no two-phase apply here by
// design (see docs/configuration.md). A subscriber is responsible for its
// own error handling/logging; Apply does not surface subscriber errors to
// the caller, since by the time they run the write has already succeeded.
func (s *Store) Apply(ctx context.Context, set map[string]any, unset []string, actor string) error {
	s.applyMu.Lock()
	defer s.applyMu.Unlock()

	type write struct {
		key      string
		value    string
		isSecret bool
	}
	writes := make([]write, 0, len(set))

	for key, value := range set {
		field, ok := Lookup(key)
		if !ok {
			return fmt.Errorf("%w: %w: %q", ErrInvalidInput, ErrUnknownField, key)
		}
		if !field.Editable {
			return fmt.Errorf("%w: %w: %q (%s)", ErrInvalidInput, ErrNotEditable, key, field.ReadOnlyReason)
		}
		if field.Validate != nil {
			if err := field.Validate(value); err != nil {
				return fmt.Errorf("%w: invalid value for %q: %w", ErrInvalidInput, key, err)
			}
		}

		var stored string
		if field.Secret {
			plain, ok := value.(string)
			if !ok {
				return fmt.Errorf("%w: secret field %q must be a string", ErrInvalidInput, key)
			}
			sealed, err := s.box.Seal(plain)
			if err != nil {
				return fmt.Errorf("%w: %q: %w", ErrInvalidInput, key, err)
			}
			b, err := json.Marshal(sealed)
			if err != nil {
				return fmt.Errorf("%w: encode %q: %w", ErrInvalidInput, key, err)
			}
			stored = string(b)
		} else {
			b, err := json.Marshal(value)
			if err != nil {
				return fmt.Errorf("%w: encode %q: %w", ErrInvalidInput, key, err)
			}
			stored = string(b)
		}

		writes = append(writes, write{key: key, value: stored, isSecret: field.Secret})
	}

	for _, key := range unset {
		if _, ok := Lookup(key); !ok {
			return fmt.Errorf("%w: %w: %q", ErrInvalidInput, ErrUnknownField, key)
		}
	}

	err := s.db.InTx(ctx, func(q *sqlcgen.Queries) error {
		for _, w := range writes {
			isSecret := int64(0)
			if w.isSecret {
				isSecret = 1
			}
			if _, err := q.UpsertAppSetting(ctx, sqlcgen.UpsertAppSettingParams{
				Key:       w.key,
				Value:     w.value,
				IsSecret:  isSecret,
				UpdatedBy: actor,
			}); err != nil {
				return fmt.Errorf("upsert %q: %w", w.key, err)
			}
		}
		for _, key := range unset {
			if err := q.DeleteAppSetting(ctx, key); err != nil {
				return fmt.Errorf("delete %q: %w", key, err)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}

	if err := s.reload(ctx); err != nil {
		return err
	}

	s.subsMu.Lock()
	subs := append([]func(*config.Config){}, s.subscribers...)
	s.subsMu.Unlock()

	effective := s.Effective()
	for _, fn := range subs {
		fn(effective)
	}
	return nil
}
