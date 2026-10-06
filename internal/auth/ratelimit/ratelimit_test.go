package ratelimit

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func newTestLimiter(t *testing.T) *Limiter {
	t.Helper()
	return NewFromConfig(Config{
		MaxFailuresFast: 3,
		FastWindow:      100 * time.Millisecond,
		CoolOffFast:     50 * time.Millisecond,
		MaxFailuresSlow: 5,
		SlowWindow:      200 * time.Millisecond,
		CoolOffSlow:     100 * time.Millisecond,
		Now:             time.Now,
	})
}

func TestCheck_AllowsUnknownIP(t *testing.T) {
	l := newTestLimiter(t)
	assert.True(t, l.Check("10.0.0.1").Allowed)
}

func TestRecordFailure_FastThresholdTriggersCoolOff(t *testing.T) {
	l := newTestLimiter(t)
	assert.True(t, l.RecordFailure("10.0.0.1").Allowed, "failure 0 below threshold")
	assert.True(t, l.RecordFailure("10.0.0.1").Allowed, "failure 1 below threshold")
	assert.False(t, l.RecordFailure("10.0.0.1").Allowed, "failure 2 hits fast threshold")

	d := l.Check("10.0.0.1")
	assert.False(t, d.Allowed)
	assert.Equal(t, "fast", d.CoolOffKind)
}

func TestRecordFailure_SlowThresholdTriggersLongerCoolOff(t *testing.T) {
	l := newTestLimiter(t)
	for i := 0; i < 5; i++ {
		l.RecordFailure("10.0.0.1")
	}
	d := l.Check("10.0.0.1")
	assert.False(t, d.Allowed)
	assert.Equal(t, "slow", d.CoolOffKind)
	assert.Greater(t, d.RetryAfter, 50*time.Millisecond)
	assert.LessOrEqual(t, d.RetryAfter, 100*time.Millisecond)
}

func TestRecordFailure_WindowSlides(t *testing.T) {
	l := newTestLimiter(t)
	l.RecordFailure("10.0.0.1")
	l.RecordFailure("10.0.0.1")
	assert.True(t, l.Check("10.0.0.1").Allowed)

	time.Sleep(150 * time.Millisecond)
	assert.True(t, l.Check("10.0.0.1").Allowed, "after fast window expires, IP should be allowed")

	d := l.RecordFailure("10.0.0.1")
	assert.True(t, d.Allowed, "1 post-window failure is below fast threshold")
}

func TestRecordFailure_DifferentIPsAreIndependent(t *testing.T) {
	l := newTestLimiter(t)
	for i := 0; i < 3; i++ {
		l.RecordFailure("10.0.0.1")
	}
	assert.True(t, l.Check("10.0.0.2").Allowed)
	assert.False(t, l.Check("10.0.0.1").Allowed)
}

// A success must NOT wipe the IP's failure history: otherwise anyone
// holding one valid account can interleave successful logins to reset
// the counter and brute-force other accounts without limit.
func TestRecordSuccess_DoesNotClearHistory(t *testing.T) {
	l := newTestLimiter(t)
	l.RecordFailure("10.0.0.1")
	l.RecordFailure("10.0.0.1")
	l.RecordSuccess("10.0.0.1")
	assert.False(t, l.RecordFailure("10.0.0.1").Allowed, "3rd failure still hits the fast threshold despite the success")
}

// N concurrent attempts must not all pass the gate before any failure
// is recorded: Begin reserves a slot per in-flight attempt.
func TestBegin_LimitsConcurrentAttempts(t *testing.T) {
	l := newTestLimiter(t) // MaxFailuresFast = 3
	var releases []func()
	allowed := 0
	for i := 0; i < 50; i++ {
		d, release := l.Begin("10.0.0.1")
		if d.Allowed {
			allowed++
			releases = append(releases, release)
		}
	}
	assert.Equal(t, 3, allowed, "only MaxFailuresFast attempts may be in flight at once")
	for _, r := range releases {
		r()
	}
	assert.True(t, l.Check("10.0.0.1").Allowed, "released slots free the IP again")
	d, release := l.Begin("10.0.0.1")
	assert.True(t, d.Allowed)
	release()
	release() // idempotent
}

func TestRecordFailure_CoolOffExpires(t *testing.T) {
	l := newTestLimiter(t)
	for i := 0; i < 3; i++ {
		l.RecordFailure("10.0.0.1")
	}
	assert.False(t, l.Check("10.0.0.1").Allowed)

	time.Sleep(80 * time.Millisecond)
	assert.True(t, l.Check("10.0.0.1").Allowed, "cool-off should expire")
}
