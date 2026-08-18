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
	"strconv"
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

// ErrImmichHTTP is a typed error for a non-2xx response.
type ErrImmichHTTP struct {
	Status int
	Body   string
	// RetryAfter is set for 429 responses when the Retry-After header is
	// parseable and non-negative; zero otherwise.
	RetryAfter time.Duration
}

func (e *ErrImmichHTTP) Error() string {
	return fmt.Sprintf("immich: http %d: %s", e.Status, e.Body)
}

const (
	triggerAttempts   = 3
	triggerBaseWait   = 100 * time.Millisecond
	maxRetryAfterWait = 30 * time.Second
)

// TriggerScan tells Immich to scan its external library. Retries with bounded
// exponential backoff on 5xx, 429, and network errors; other 4xx fails
// immediately (non-retryable). When a 429 carries a parseable Retry-After
// header, the wait between attempts is the lesser of Retry-After and 30s;
// otherwise the exponential backoff applies. Respects ctx cancellation
// between attempts.
func (c *Client) TriggerScan(ctx context.Context) error {
	var lastErr error
	wait := triggerBaseWait
	for attempt := 0; attempt < triggerAttempts; attempt++ {
		err := c.triggerOnce(ctx)
		if err == nil {
			return nil
		}
		var httpErr *ErrImmichHTTP
		if errors.As(err, &httpErr) && httpErr.Status < 500 && httpErr.Status != http.StatusTooManyRequests {
			return err // 4xx other than 429: not retryable
		}
		lastErr = err
		if attempt < triggerAttempts-1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(retryDelay(wait, httpErr)):
			}
			wait *= 2
		}
	}
	return fmt.Errorf("immich: trigger scan after %d attempts: %w", triggerAttempts, lastErr)
}

// retryDelay is how long to wait before the next attempt: the capped
// Retry-After of a 429 when present and positive, otherwise the exponential
// backoff.
func retryDelay(backoff time.Duration, httpErr *ErrImmichHTTP) time.Duration {
	if httpErr != nil && httpErr.Status == http.StatusTooManyRequests && httpErr.RetryAfter > 0 {
		if httpErr.RetryAfter > maxRetryAfterWait {
			return maxRetryAfterWait
		}
		return httpErr.RetryAfter
	}
	return backoff
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
	httpErr := &ErrImmichHTTP{Status: resp.StatusCode, Body: string(body)}
	if resp.StatusCode == http.StatusTooManyRequests {
		httpErr.RetryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}
	return httpErr
}

// parseRetryAfter parses a Retry-After header value, either a non-negative
// integer number of seconds or an HTTP-date (RFC 7231). It returns zero for
// anything unparseable or negative, and caps any result at maxRetryAfterWait.
func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil {
		if secs < 0 {
			return 0
		}
		if secs > int(maxRetryAfterWait/time.Second) {
			return maxRetryAfterWait
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			if d > maxRetryAfterWait {
				return maxRetryAfterWait
			}
			return d
		}
	}
	return 0
}
