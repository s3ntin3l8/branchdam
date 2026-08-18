package immich

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
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
