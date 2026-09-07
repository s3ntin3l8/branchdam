package users

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db"
)

const jitTestSecretBase64 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

func newJITTestService(t *testing.T) *Service {
	t.Helper()
	root := t.TempDir()
	dbPath := filepath.Join(root, "jit.db")
	database, err := db.Open(context.Background(), dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })
	return NewService(database, jitTestSecretBase64, ServiceOptions{Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
}

func TestJITProvisioner_AdminGroupTriggersProvision(t *testing.T) {
	svc := newJITTestService(t)
	ctx := context.Background()
	jit := JITProvisioner(svc, []string{"dam-admins"}, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	merged := &auth.Principal{
		Kind: auth.KindUser, Name: "alice-fwd", Email: "alice@example.com",
		Groups: []string{"dam-admins", "users"}, Authenticated: true,
	}
	view, err := jit(ctx, merged, []string{"dam-admins"}, true)
	require.NoError(t, err)
	assert.NotZero(t, view.UserID)
	assert.True(t, view.IsAdmin)

	// Second call returns the same row (idempotent).
	view2, err := jit(ctx, merged, []string{"dam-admins"}, true)
	require.NoError(t, err)
	assert.Equal(t, view.UserID, view2.UserID)
	assert.True(t, view2.IsAdmin)

	// DB row has source='forward-jit' and the right email.
	row, err := svc.GetUserByID(ctx, view.UserID)
	require.NoError(t, err)
	assert.Equal(t, "forward-jit", row.Source)
	assert.True(t, row.Email.Valid)
	assert.Equal(t, "alice@example.com", row.Email.String)
}

func TestJITProvisioner_NonAdminGroupDoesNotProvision(t *testing.T) {
	svc := newJITTestService(t)
	ctx := context.Background()
	jit := JITProvisioner(svc, []string{"dam-admins"}, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	merged := &auth.Principal{
		Kind: auth.KindUser, Name: "bob", Email: "bob@example.com",
		Groups: []string{"users"}, Authenticated: true,
	}
	view, err := jit(ctx, merged, []string{"dam-admins"}, true)
	require.NoError(t, err)
	assert.Zero(t, view.UserID, "non-admin should not trigger JIT")
}

func TestJITProvisioner_RequireEmailRefusesEmpty(t *testing.T) {
	svc := newJITTestService(t)
	ctx := context.Background()
	jit := JITProvisioner(svc, []string{"dam-admins"}, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	merged := &auth.Principal{
		Kind: auth.KindUser, Name: "carol", Email: "",
		Groups: []string{"dam-admins"}, Authenticated: true,
	}
	view, err := jit(ctx, merged, []string{"dam-admins"}, true)
	require.NoError(t, err)
	assert.Zero(t, view.UserID, "requireEmail=true with empty email must refuse JIT")
}

func TestJITProvisioner_EmptyEmailAllowedWhenNotRequired(t *testing.T) {
	svc := newJITTestService(t)
	ctx := context.Background()
	jit := JITProvisioner(svc, []string{"dam-admins"}, false, slog.New(slog.NewTextHandler(io.Discard, nil)))

	merged := &auth.Principal{
		Kind: auth.KindUser, Name: "dave-noemail", Email: "",
		Groups: []string{"dam-admins"}, Authenticated: true,
	}
	view, err := jit(ctx, merged, []string{"dam-admins"}, false)
	require.NoError(t, err)
	assert.NotZero(t, view.UserID, "requireEmail=false with empty email keys by username")
	assert.True(t, view.IsAdmin)

	row, err := svc.GetUserByID(ctx, view.UserID)
	require.NoError(t, err)
	assert.Equal(t, "forward-jit", row.Source)
	assert.False(t, row.Email.Valid)
}

func TestJITProvisioner_UsernameClashReturnsExistingLocalUser(t *testing.T) {
	svc := newJITTestService(t)
	ctx := context.Background()
	jit := JITProvisioner(svc, []string{"dam-admins"}, false, slog.New(slog.NewTextHandler(io.Discard, nil)))

	// Pre-create a non-admin LOCAL user with username "eve". When a
	// forward-auth admin with the same username arrives (no email),
	// the JIT path returns the local user's view verbatim -- the local
	// user's existing is_admin (false here) wins, so this forward-auth
	// user is NOT silently promoted to admin via JIT.
	_, err := svc.CreateLocalUser(ctx, "eve", "", "password", false, time.Now().Unix(), "test")
	require.NoError(t, err)

	merged := &auth.Principal{
		Kind: auth.KindUser, Name: "eve", Email: "",
		Groups: []string{"dam-admins"}, Authenticated: true,
	}
	view, err := jit(ctx, merged, []string{"dam-admins"}, false)
	require.NoError(t, err)
	assert.NotZero(t, view.UserID, "JIT should return existing local user's view")
	assert.False(t, view.IsAdmin, "local non-admin's is_admin must NOT be promoted by JIT")
}

func TestJITProvisioner_NilMergedReturnsZero(t *testing.T) {
	svc := newJITTestService(t)
	jit := JITProvisioner(svc, []string{"dam-admins"}, true, slog.New(slog.NewTextHandler(io.Discard, nil)))

	view, err := jit(context.Background(), nil, []string{"dam-admins"}, true)
	require.NoError(t, err)
	assert.Zero(t, view.UserID)
}

// Ensure the unused import stays linked to the test (sql.NullInt64 used
// implicitly by sqlcgen audit-row writes we may add later).
var _ = sql.NullInt64{}
