package users

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"time"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/db/sqlcgen"
)

// JITProvisioner returns an auth.JITProvisioner that, on a forward-auth
// request whose asserted groups intersect adminGroups, looks up (or
// creates) a local source='forward-jit' user row keyed by the
// forward-auth asserted email. The returned auth.LocalUserView carries
// the local user_id + is_admin, so the downstream IsAdmin check honors
// the local-`is_admin` override on the very first request after JIT.
//
// Race-safe: concurrent first-sight requests for the same email either
// lose the UNIQUE-constraint race (and fall through to a re-lookup) or
// the lookup returns the row the other request just inserted. The
// UNIQUE index users_email_source_uniq is the source of truth.
//
// requireEmail: when true, refuse JIT if the forward-auth email is
// empty (caller-supplied). When false, JIT is allowed even without an
// email -- useful for homelab deployments where forward-auth doesn't
// assert email. In that case the JIT user is keyed by username, not
// email, and the second request with the same username returns the
// existing row.
//
// Returns (zero, nil) when the request has no intersection with
// adminGroups (regular forward-auth user, not an admin) or when the
// requireEmail gate refuses. The auth/route layer treats that as "no
// local-`is_admin` override, just use the forward principal."
func JITProvisioner(svc *Service, adminGroups []string, requireEmail bool, log *slog.Logger) auth.JITProvisioner {
	return func(ctx context.Context, merged *auth.Principal, allowList []string, requireEmailFlag bool) (auth.LocalUserView, error) {
		if merged == nil || !merged.Authenticated {
			return auth.LocalUserView{}, nil
		}
		if !slicesContainsAny(merged.Groups, allowList) {
			return auth.LocalUserView{}, nil
		}
		if requireEmailFlag && merged.Email == "" {
			log.Debug("auth: forward-JIT refused: requireEmail is true and forward-auth asserted no email",
				"username", merged.Name)
			return auth.LocalUserView{}, nil
		}
		if merged.Email == "" && merged.Name == "" {
			return auth.LocalUserView{}, nil
		}
		email := merged.Email
		var user sqlcgen.User
		var err error
		if email != "" {
			user, err = getOrCreateForwardJIT(ctx, svc, merged.Name, email, true, log)
		} else {
			user, err = getOrCreateForwardJITByUsername(ctx, svc, merged.Name, log)
		}
		if err != nil {
			return auth.LocalUserView{}, err
		}
		return auth.LocalUserView{UserID: user.ID, IsAdmin: user.IsAdmin != 0}, nil
	}
}

// getOrCreateForwardJIT returns the local source='forward-jit' user
// for email, creating one if it doesn't exist.
func getOrCreateForwardJIT(ctx context.Context, svc *Service, name, email string, isAdmin bool, log *slog.Logger) (sqlcgen.User, error) {
	existing, err := svc.GetUserByEmailSource(ctx, email, "forward-jit")
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrUserNotFound) {
		return sqlcgen.User{}, err
	}
	created, err := svc.CreateForwardJITUser(ctx, name, email, isAdmin, time.Now().Unix(), "forward:"+name)
	if err != nil {
		// Race: a concurrent JIT for the same email just won the
		// UNIQUE-constraint race. Re-lookup and return the winner.
		existing2, lookupErr := svc.GetUserByEmailSource(ctx, email, "forward-jit")
		if lookupErr == nil {
			return existing2, nil
		}
		return sqlcgen.User{}, err
	}
	log.Info("auth: forward-JIT provisioned local admin", "email", email, "isAdmin", isAdmin)
	return created, nil
}

// getOrCreateForwardJITByUsername is the email-less fallback path.
// Refuses to create a JIT row when a LOCAL user with the same username
// already exists -- that would silently let the forward-auth user take
// over a manually-created account.
func getOrCreateForwardJITByUsername(ctx context.Context, svc *Service, username string, log *slog.Logger) (sqlcgen.User, error) {
	if username == "" {
		return sqlcgen.User{}, errors.New("auth: forward-JIT username path: empty username")
	}
	existing, err := svc.GetUserByUsername(ctx, username)
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, ErrUserNotFound) {
		return sqlcgen.User{}, err
	}
	created, err := svc.CreateForwardJITUser(ctx, username, "", true, time.Now().Unix(), "forward:"+username)
	if err != nil {
		existing2, lookupErr := svc.GetUserByUsername(ctx, username)
		if lookupErr == nil {
			return existing2, nil
		}
		return sqlcgen.User{}, err
	}
	log.Info("auth: forward-JIT provisioned local admin (username-keyed, no email)", "username", username)
	return created, nil
}

// slicesContainsAny returns true if any element of a is in b. Cheap
// because the adminGroups list is small (a handful of names).
func slicesContainsAny(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}
