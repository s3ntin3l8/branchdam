package immich

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestTriggerScanSuccess(t *testing.T) {
	var called atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/libraries/lib-1/scan" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Header.Get("X-Api-Key") != "secret" {
			t.Errorf("X-Api-Key = %q", r.Header.Get("X-Api-Key"))
		}
		called.Store(true)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(Config{APIURL: srv.URL, APIKey: "secret", LibraryID: "lib-1"})
	if err := c.TriggerScan(context.Background()); err != nil {
		t.Fatalf("TriggerScan: %v", err)
	}
	if !called.Load() {
		t.Error("scan endpoint was not called")
	}
}

func TestTriggerScanRetries5xxThenSucceeds(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(Config{APIURL: srv.URL, APIKey: "k", LibraryID: "lib"})
	if err := c.TriggerScan(context.Background()); err != nil {
		t.Fatalf("TriggerScan should recover after retry: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (one 5xx + one retry)", calls.Load())
	}
}

func TestTriggerScan4xxDoesNotRetry(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	c := New(Config{APIURL: srv.URL, APIKey: "k", LibraryID: "lib"})
	err := c.TriggerScan(context.Background())
	if err == nil {
		t.Fatal("expected an error for 404")
	}
	var httpErr *ErrImmichHTTP
	if !errors.As(err, &httpErr) || httpErr.Status != http.StatusNotFound {
		t.Errorf("error = %v, want ErrImmichHTTP 404", err)
	}
	if httpErr.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0 for a non-429 response", httpErr.RetryAfter)
	}
	if calls.Load() != 1 {
		t.Errorf("calls = %d, want 1 (4xx is not retried)", calls.Load())
	}
}

func TestTriggerScanTrimsTrailingSlash(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(Config{APIURL: srv.URL + "/", APIKey: "k", LibraryID: "lib"})
	if err := c.TriggerScan(context.Background()); err != nil {
		t.Fatalf("TriggerScan: %v", err)
	}
	if path != "/api/libraries/lib/scan" {
		t.Errorf("path = %q, want /api/libraries/lib/scan (no double slash)", path)
	}
}

func TestTriggerScan429WithRetryAfter(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(Config{APIURL: srv.URL, APIKey: "k", LibraryID: "lib"})
	if err := c.TriggerScan(context.Background()); err != nil {
		t.Fatalf("TriggerScan should recover after 429 with Retry-After: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (one 429 + one retry)", calls.Load())
	}
}

func TestTriggerScan429WithoutHeaderRetries(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()

	c := New(Config{APIURL: srv.URL, APIKey: "k", LibraryID: "lib"})
	if err := c.TriggerScan(context.Background()); err != nil {
		t.Fatalf("TriggerScan should recover after 429 without Retry-After: %v", err)
	}
	if calls.Load() != 2 {
		t.Errorf("calls = %d, want 2 (one 429 + one retry)", calls.Load())
	}
}

func TestTriggerScanPersistent5xxExhausts(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	c := New(Config{APIURL: srv.URL, APIKey: "k", LibraryID: "lib"})
	if err := c.TriggerScan(context.Background()); err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	if calls.Load() != 3 {
		t.Errorf("calls = %d, want 3 (three attempts)", calls.Load())
	}
}

func TestTriggerOnce429ParsesRetryAfterSeconds(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(Config{APIURL: srv.URL, APIKey: "k", LibraryID: "lib"})
	err := c.triggerOnce(context.Background())
	var httpErr *ErrImmichHTTP
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want ErrImmichHTTP", err)
	}
	if httpErr.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d", httpErr.Status)
	}
	if httpErr.RetryAfter != 7*time.Second {
		t.Errorf("RetryAfter = %v, want 7s", httpErr.RetryAfter)
	}
}

func TestTriggerOnce429ParsesRetryAfterDate(t *testing.T) {
	date := time.Now().Add(5 * time.Second).UTC().Format(http.TimeFormat)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", date)
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()

	c := New(Config{APIURL: srv.URL, APIKey: "k", LibraryID: "lib"})
	err := c.triggerOnce(context.Background())
	var httpErr *ErrImmichHTTP
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want ErrImmichHTTP", err)
	}
	if httpErr.Status != http.StatusTooManyRequests {
		t.Errorf("Status = %d", httpErr.Status)
	}
	if httpErr.RetryAfter <= 0 {
		t.Errorf("RetryAfter = %v, want > 0 (date-form Retry-After)", httpErr.RetryAfter)
	}
}

func TestTriggerOnceNon429LeavesRetryAfterZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer srv.Close()

	c := New(Config{APIURL: srv.URL, APIKey: "k", LibraryID: "lib"})
	err := c.triggerOnce(context.Background())
	var httpErr *ErrImmichHTTP
	if !errors.As(err, &httpErr) {
		t.Fatalf("error = %v, want ErrImmichHTTP", err)
	}
	if httpErr.RetryAfter != 0 {
		t.Errorf("RetryAfter = %v, want 0 for a 400 response", httpErr.RetryAfter)
	}
}

func TestParseRetryAfter(t *testing.T) {
	futureDate := time.Now().Add(time.Hour).UTC().Format(http.TimeFormat)
	pastDate := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	tests := []struct {
		name string
		val  string
		want time.Duration
	}{
		{"empty", "", 0},
		{"garbage", "soon", 0},
		{"negative seconds", "-1", 0},
		{"zero seconds", "0", 0},
		{"small seconds", "7", 7 * time.Second},
		{"at cap", "30", maxRetryAfterWait},
		{"above cap", "31", maxRetryAfterWait},
		{"huge seconds", "99999999999", maxRetryAfterWait},
		{"max int64 seconds", "9223372036854775807", maxRetryAfterWait},
		{"overflows int64", "99999999999999999999999", 0},
		{"future date", futureDate, maxRetryAfterWait},
		{"past date", pastDate, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseRetryAfter(tc.val); got != tc.want {
				t.Errorf("parseRetryAfter(%q) = %v, want %v", tc.val, got, tc.want)
			}
		})
	}
}

func TestRetryDelay(t *testing.T) {
	const backoff = 100 * time.Millisecond
	tests := []struct {
		name    string
		httpErr *ErrImmichHTTP
		want    time.Duration
	}{
		{"no http error uses backoff", nil, backoff},
		{"5xx uses backoff", &ErrImmichHTTP{Status: http.StatusServiceUnavailable}, backoff},
		{"429 without retry-after uses backoff", &ErrImmichHTTP{Status: http.StatusTooManyRequests}, backoff},
		{"429 with retry-after uses retry-after", &ErrImmichHTTP{Status: http.StatusTooManyRequests, RetryAfter: 2 * time.Second}, 2 * time.Second},
		{"429 with retry-after above cap is capped", &ErrImmichHTTP{Status: http.StatusTooManyRequests, RetryAfter: maxRetryAfterWait + time.Second}, maxRetryAfterWait},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := retryDelay(backoff, tc.httpErr); got != tc.want {
				t.Errorf("retryDelay(%v, %v) = %v, want %v", backoff, tc.httpErr, got, tc.want)
			}
		})
	}
}
