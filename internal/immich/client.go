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
	"net/url"
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
	// err is set when the configuration cannot be used: an API URL that is
	// not a usable http(s) URL, or a library id that is not a single path
	// segment. Every call returns it instead of building a request that
	// would go somewhere unintended.
	err error
}

func New(cfg Config) *Client {
	c := &Client{
		baseURL:   strings.TrimSuffix(strings.TrimSpace(cfg.APIURL), "/"),
		apiKey:    cfg.APIKey,
		libraryID: cfg.LibraryID,
		http: &http.Client{
			Timeout:       15 * time.Second,
			CheckRedirect: redirectGuard,
		},
	}
	switch {
	case strings.TrimSpace(cfg.APIURL) == "":
		c.err = errors.New("immich: api url is empty")
	default:
		if err := ValidateAPIURL(cfg.APIURL); err != nil {
			c.err = err
		} else if err := ValidateLibraryID(cfg.LibraryID); err != nil {
			c.err = err
		}
	}
	return c
}

// apiKeyHeader carries the Immich API key.
const apiKeyHeader = "X-Api-Key"

// maxRedirects matches net/http's own default redirect cap.
const maxRedirects = 10

// redirectGuard stops the API key from crossing a host boundary.
//
// net/http forwards a redirect's original headers to the new request,
// filtering only Authorization, Www-Authenticate, Cookie and Cookie2 --
// the headers it knows are credentials. A custom header is copied
// verbatim to ANY host, so a redirect to another host handed the Immich
// API key to whoever controls that host.
//
// A redirect that stays on the same host and port is fine: the key still
// belongs to the same server, and deployments legitimately redirect (a
// trailing-slash fix, an http -> https upgrade). Anything else drops the
// key, so the redirected request fails closed as unauthenticated instead
// of authenticating to a stranger.
//
// Setting CheckRedirect REPLACES net/http's defaultCheckRedirect, so the
// redirect cap has to be carried over by hand.
func redirectGuard(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("immich: stopped after %d redirects", maxRedirects)
	}
	if len(via) == 0 || req.URL == nil || via[0].URL == nil {
		return nil
	}
	// Host carries the port, so a redirect from immich:2283 to immich:2284
	// -- a different service on the same name -- drops the key too.
	if !strings.EqualFold(via[0].URL.Host, req.URL.Host) {
		req.Header.Del(apiKeyHeader)
	}
	return nil
}

// ValidateAPIURL checks that raw is usable as an Immich API base URL.
//
// The scheme must be http or https: anything else is either a typo (a
// bare "immich:2283" parses as the scheme "immich") or a way to point the
// client at something that is not the Immich API -- and this client sends
// the Immich API key to whatever host this names.
//
// The URL must carry a host, and must not carry userinfo: the API key
// belongs in the X-Api-Key header, not in the URL, where it would land in
// proxy logs and server access logs.
//
// Nor may it carry a query string or a fragment. The endpoints are
// concatenated onto the base URL by fmt.Sprintf ("%s/api/libraries/%s/scan"),
// so a base URL of "http://host?token=1" does not produce
// "http://host/api/libraries/1/scan?token=1" -- the query swallows the
// whole path and the request goes out as path "/" with query
// "token=1/api/libraries/1/scan". Every trigger then fails against a URL
// the operator never configured, which is exactly the failure this
// validation exists to catch before the worker starts.
func ValidateAPIURL(raw string) error {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return errors.New("immich: api url is empty")
	}
	u, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("immich: invalid api url: %w", err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("immich: api url scheme %q is not http or https", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("immich: api url has no host")
	}
	if u.User != nil {
		return errors.New("immich: api url must not carry userinfo")
	}
	if u.RawQuery != "" || u.ForceQuery {
		return errors.New("immich: api url must not carry a query string")
	}
	if u.Fragment != "" {
		return errors.New("immich: api url must not carry a fragment")
	}
	return nil
}

// ValidateLibraryID checks that id is safe to interpolate into the scan
// path as a single segment.
//
// The library id is configuration, and configuration is rewritable at
// runtime through the settings API, so it is not necessarily the value the
// operator started with. It must not contain a separator, and it must not
// be a relative-path segment of its own -- either would move the request
// off POST /api/libraries/{id}/scan, onto a path of the caller's choosing,
// with the API key attached.
//
// Exported so a caller can refuse to start on an unusable id instead of
// starting a worker that fails every scan forever -- see
// internal/sync.Supervisor's own call site.
func ValidateLibraryID(id string) error {
	if id == "" {
		return errors.New("immich: library id is empty")
	}
	if strings.ContainsAny(id, `/\`) {
		return fmt.Errorf("immich: library id %q contains a path separator", id)
	}
	// "." and ".." (and "..." and so on) are relative path segments.
	if strings.Trim(id, ".") == "" {
		return fmt.Errorf("immich: library id %q is a relative path segment", id)
	}
	return nil
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
	// A configuration that cannot produce a valid request is not a
	// transient failure -- retrying it three times would only delay the
	// error.
	if c.err != nil {
		return c.err
	}
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
	if c.err != nil {
		return c.err
	}
	endpoint := fmt.Sprintf("%s/api/libraries/%s/scan", c.baseURL, url.PathEscape(c.libraryID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(nil))
	if err != nil {
		return err
	}
	req.Header.Set(apiKeyHeader, c.apiKey)
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
