// Package immich is a minimal Immich API client for the external-library scan
// trigger (spec §4 Immich Integration). It is pure HTTP -- no database access.
// branchDAM never uploads bytes: Immich indexes the shared export mount
// natively (zero-copy disk scan), so the only call needed is the library scan
// trigger: POST /api/libraries/{library_id}/scan.
package immich

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Config holds the connection details. Keys come from config.yaml's `immich:`
// section, env-expanded (IMMICH_API_URL / IMMICH_API_KEY).
type Config struct {
	APIURL    string
	APIKey    string
	LibraryID string
}

// Client is the Immich HTTP client.
type Client struct {
	baseURL   string
	apiKey    string
	libraryID string
	http      *http.Client
}

func New(cfg Config) *Client {
	return &Client{
		baseURL:   strings.TrimSuffix(cfg.APIURL, "/"),
		apiKey:    cfg.APIKey,
		libraryID: cfg.LibraryID,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

// ErrImmichHTTP is a typed, non-retryable error for a 4xx response.
type ErrImmichHTTP struct {
	Status int
	Body   string
}

func (e *ErrImmichHTTP) Error() string {
	return fmt.Sprintf("immich: http %d: %s", e.Status, e.Body)
}

const (
	triggerAttempts = 3
	triggerBaseWait = 100 * time.Millisecond
)

// TriggerScan tells Immich to scan its external library. Retries with bounded
// exponential backoff on 5xx and network errors; 4xx fails immediately
// (non-retryable). Respects ctx cancellation between attempts.
func (c *Client) TriggerScan(ctx context.Context) error {
	var lastErr error
	wait := triggerBaseWait
	for attempt := 0; attempt < triggerAttempts; attempt++ {
		err := c.triggerOnce(ctx)
		if err == nil {
			return nil
		}
		var httpErr *ErrImmichHTTP
		if errors.As(err, &httpErr) && httpErr.Status < 500 {
			return err // 4xx: not retryable
		}
		lastErr = err
		if attempt < triggerAttempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			wait *= 2
		}
	}
	return fmt.Errorf("immich: trigger scan after %d attempts: %w", triggerAttempts, lastErr)
}

func (c *Client) triggerOnce(ctx context.Context) error {
	url := fmt.Sprintf("%s/api/libraries/%s/scan", c.baseURL, c.libraryID)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set("X-Api-Key", c.apiKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return &ErrImmichHTTP{Status: resp.StatusCode, Body: string(body)}
}
