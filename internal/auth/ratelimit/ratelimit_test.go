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

func TestRecordSuccess_ClearsHistory(t *testing.T) {
	l := newTestLimiter(t)
	l.RecordFailure("10.0.0.1")
	l.RecordFailure("10.0.0.1")
	l.RecordSuccess("10.0.0.1")
	assert.True(t, l.RecordFailure("10.0.0.1").Allowed, "1st post-success failure is below fast threshold")
	assert.True(t, l.RecordFailure("10.0.0.1").Allowed, "2nd post-success failure is below fast threshold")
	assert.False(t, l.RecordFailure("10.0.0.1").Allowed, "3rd post-success failure hits fast threshold")
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
