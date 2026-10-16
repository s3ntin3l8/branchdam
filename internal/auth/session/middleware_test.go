package session

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/s3ntin3l8/branchdam/internal/auth"
	"github.com/s3ntin3l8/branchdam/internal/auth/users"
	"github.com/s3ntin3l8/branchdam/internal/db"
)

// testSecretBase64 is 32 zero bytes encoded as base64.
const testSecretBase64 = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="

// fakeClock is a settable clock shared by the users service and the
// middleware so the test controls both the session's created/last-seen
// stamps and the "now" each request is evaluated at.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

type sessionFixture struct {
	svc         *users.Service
	mw          *Middleware
	clock       *fakeClock
	cookieID    string
	cookieValue string
}

func newSessionFixture(t *testing.T, idle time.Duration) sessionFixture {
	t.Helper()
	ctx := context.Background()
	database, err := db.Open(ctx, filepath.Join(t.TempDir(), "session.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = database.Close() })

	clock := &fakeClock{now: time.Unix(1_800_000_000, 0)}
	svc := users.NewService(database, testSecretBase64, users.ServiceOptions{Now: clock.Now})
	user, err := svc.CreateLocalUser(ctx, "erin", "", "password", true, clock.Now().Unix(), "test")
	require.NoError(t, err)
	cookieID, cookieValue, err := svc.MintCookieValue()
	require.NoError(t, err)
	now := clock.Now()
	_, err = svc.CreateSession(ctx, user.ID, cookieID, "127.0.0.1", "test", now.Add(30*24*time.Hour), now.Add(idle))
	require.NoError(t, err)

	mw := New(svc, Config{IdleTimeout: idle, Now: clock.Now})
	return sessionFixture{svc: svc, mw: mw, clock: clock, cookieID: cookieID, cookieValue: cookieValue}
}

// serve runs one request through the middleware and reports whether the
// handler saw an authenticated local principal.
func (f sessionFixture) serve(t *testing.T) bool {
	t.Helper()
	var authed bool
	h := f.mw.Middleware(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		p, ok := auth.From(r.Context())
		authed = ok && p.Authenticated && p.AuthProvider == auth.AuthProviderLocal
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/v1/assets/1/thumbnail", nil)
	req.AddCookie(&http.Cookie{Name: "branchdam_session", Value: f.cookieValue})
	h.ServeHTTP(httptest.NewRecorder(), req)
	return authed
}

func (f sessionFixture) lastSeen(t *testing.T) (lastSeen, idleExpires int64) {
	t.Helper()
	s, err := f.svc.GetSessionByCookieID(context.Background(), f.cookieID)
	require.NoError(t, err)
	return s.LastSeenAt, s.IdleExpiresAt
}

// TestMiddleware_SessionTouchThrottled pins the fix for the audit finding
// that TouchSession took the single writer on every authenticated request
// (every thumbnail): requests inside the touch window authenticate without
// writing, and the first request after the window slides the idle expiry.
func TestMiddleware_SessionTouchThrottled(t *testing.T) {
	const idle = 24 * time.Hour
	f := newSessionFixture(t, idle)
	created := f.clock.Now().Unix()

	for range 20 {
		f.clock.Advance(2 * time.Second)
		require.True(t, f.serve(t), "request inside the touch window must still authenticate")
	}
	seen, idleExp := f.lastSeen(t)
	assert.Equal(t, created, seen, "no touch inside the window: last_seen_at must stay at session creation")
	assert.Equal(t, created+int64(idle/time.Second), idleExp)

	f.clock.Advance(sessionTouchInterval) // now 100s after creation
	require.True(t, f.serve(t))
	touchedAt := f.clock.Now().Unix()
	seen, idleExp = f.lastSeen(t)
	assert.Equal(t, touchedAt, seen, "first request after the window must touch")
	assert.Equal(t, touchedAt+int64(idle/time.Second), idleExp, "touch must slide the idle window")

	f.clock.Advance(10 * time.Second)
	require.True(t, f.serve(t))
	seen, _ = f.lastSeen(t)
	assert.Equal(t, touchedAt, seen, "a request right after a touch must not touch again")
}

// TestMiddleware_ShortIdleStillSlides guards the clamp in touchInterval: a
// deliberately short idle timeout must not be outlived by the throttle,
// or an active session would expire mid-use.
func TestMiddleware_ShortIdleStillSlides(t *testing.T) {
	const idle = 40 * time.Second
	f := newSessionFixture(t, idle)
	require.Equal(t, 10*time.Second, f.mw.touchEvery)

	for range 10 {
		f.clock.Advance(11 * time.Second)
		require.True(t, f.serve(t), "an active session must not idle out under the throttle")
	}
	seen, _ := f.lastSeen(t)
	assert.Equal(t, f.clock.Now().Unix(), seen)
}

// TestShouldTouch_CollapsesConcurrentBurst covers the in-process half of
// the throttle: a burst of requests that all read the same stale
// last_seen_at claims exactly one touch.
func TestShouldTouch_CollapsesConcurrentBurst(t *testing.T) {
	m := New(nil, Config{})
	now := time.Unix(1_800_000_000, 0)
	stale := now.Add(-2 * sessionTouchInterval).Unix()

	var claimed atomic.Int32
	var wg sync.WaitGroup
	for range 64 {
		wg.Go(func() {
			if m.shouldTouch(7, stale, now) {
				claimed.Add(1)
			}
		})
	}
	wg.Wait()
	assert.Equal(t, int32(1), claimed.Load())

	// A different session is independent.
	assert.True(t, m.shouldTouch(8, stale, now))
	// The same session becomes due again once the window has passed.
	assert.True(t, m.shouldTouch(7, stale, now.Add(sessionTouchInterval)))
	// A recent persisted last_seen_at suppresses the touch outright.
	assert.False(t, m.shouldTouch(9, now.Add(-time.Second).Unix(), now))
}
