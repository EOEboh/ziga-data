// Package notion is a minimal client for the Notion REST API, covering only
// what a lead destination needs: discovering the resources a user granted,
// reading a data source's property schema, creating a database, and writing
// pages.
//
// # API version
//
// Notion pins behavior to a dated Notion-Version header that must be sent on
// every request. The version is passed in once when the client is built and
// applied centrally in do(), so it is never spelled at a call site and cannot
// drift between endpoints.
//
// This client targets the data-source model introduced in 2025-09-03: a
// database is a parent of one or more data sources, the property schema lives
// on the data source, and pages are created with a data_source_id parent.
//
// # Rate limits
//
// Notion allows roughly three requests per second per integration install.
// Writing one lead is one or two calls, but a schema fetch plus retries can
// bunch up, so every request passes through a limiter keyed by workspace.
package notion

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// DefaultBaseURL is the Notion REST API root.
const DefaultBaseURL = "https://api.notion.com/v1"

// Notion's documented ceiling is an average of ~3 requests per second per
// integration. The burst matches the rate so a lead write (schema-free, one or
// two calls) is never delayed, while sustained traffic settles at the ceiling.
const (
	requestsPerSecond = 3
	burst             = 3
	maxAttempts       = 3
)

// Typed errors. Callers distinguish these because they mean very different
// things to a user.
var (
	// ErrNoAccess means the integration was not granted this resource. Notion
	// reports this as a 404 with code object_not_found, which reads like
	// "missing" but almost always means "not shared with the integration" —
	// the user picks resources one by one during consent. It is surfaced as a
	// permission/reconnect prompt, never as "your database is gone".
	ErrNoAccess = errors.New("notion: integration was not granted access to this resource")

	// ErrUnauthorized means the token itself is rejected (401): the user
	// uninstalled the integration or revoked access. Also a reconnect.
	ErrUnauthorized = errors.New("notion: access token rejected")

	// ErrRateLimited means retries were exhausted against a 429.
	ErrRateLimited = errors.New("notion: rate limited")
)

// APIError is a structured Notion error response. Code is Notion's machine
// -readable error code (e.g. "validation_error", "object_not_found").
type APIError struct {
	Status  int
	Code    string
	Message string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("notion: %d %s: %s", e.Status, e.Code, e.Message)
}

// IsValidation reports whether err is a Notion validation error, which is what
// a page create returns when a property value does not fit the schema.
func IsValidation(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.Status == http.StatusBadRequest
}

// Client talks to one workspace, authenticated by that workspace's token.
type Client struct {
	http    *http.Client
	baseURL string
	token   string
	version string
	limiter *rate.Limiter
}

// limiters holds one limiter per workspace. Notion's budget is per integration
// install, so two users in different workspaces must not share (or steal) each
// other's allowance, while concurrent requests for the same workspace must.
var limiters struct {
	sync.Mutex
	byWorkspace map[string]*rate.Limiter
}

func limiterFor(workspaceID string) *rate.Limiter {
	limiters.Lock()
	defer limiters.Unlock()
	if limiters.byWorkspace == nil {
		limiters.byWorkspace = map[string]*rate.Limiter{}
	}
	l, ok := limiters.byWorkspace[workspaceID]
	if !ok {
		l = rate.NewLimiter(rate.Limit(requestsPerSecond), burst)
		limiters.byWorkspace[workspaceID] = l
	}
	return l
}

// New builds a client for one workspace. baseURL may be empty for the real
// API; version must not be empty — Notion requires it on every request.
func New(token, workspaceID, version, baseURL string) (*Client, error) {
	if token == "" {
		return nil, errors.New("notion: access token is required")
	}
	if version == "" {
		return nil, errors.New("notion: API version is required on every request")
	}
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		http:    &http.Client{Timeout: 30 * time.Second},
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   token,
		version: version,
		limiter: limiterFor(workspaceID),
	}, nil
}

// do performs one API call, applying the version header, the rate limiter, and
// retries. out may be nil when the response body is not needed.
//
// This is the only place that builds a Notion request, so Notion-Version and
// the Authorization header cannot be omitted by a new call site.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	backoff := time.Second
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := c.limiter.Wait(ctx); err != nil {
			return err
		}

		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, reader)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+c.token)
		req.Header.Set("Notion-Version", c.version)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}

		resp, err := c.http.Do(req)
		if err != nil {
			// Transport-level failure: worth retrying.
			lastErr = err
			if err := sleepBackoff(ctx, &backoff, 0); err != nil {
				return err
			}
			continue
		}

		respBody, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			if err := sleepBackoff(ctx, &backoff, 0); err != nil {
				return err
			}
			continue
		}

		switch {
		case resp.StatusCode == http.StatusTooManyRequests:
			lastErr = ErrRateLimited
			// Notion sends Retry-After in seconds; honor it before falling
			// back to exponential backoff.
			if err := sleepBackoff(ctx, &backoff, retryAfter(resp)); err != nil {
				return err
			}
			continue

		case resp.StatusCode >= 500:
			lastErr = classify(resp.StatusCode, respBody)
			if err := sleepBackoff(ctx, &backoff, 0); err != nil {
				return err
			}
			continue

		case resp.StatusCode >= 400:
			// Client errors are never retried.
			return classify(resp.StatusCode, respBody)
		}

		if out == nil {
			return nil
		}
		return json.Unmarshal(respBody, out)
	}
	return fmt.Errorf("notion: %s %s failed after %d attempts: %w", method, path, maxAttempts, lastErr)
}

// retryAfter reads the Retry-After header as a duration, or 0 when absent or
// unparseable.
func retryAfter(resp *http.Response) time.Duration {
	v := resp.Header.Get("Retry-After")
	if v == "" {
		return 0
	}
	secs, err := strconv.ParseFloat(v, 64)
	if err != nil || secs < 0 {
		return 0
	}
	return time.Duration(secs * float64(time.Second))
}

// sleepBackoff waits for `wait` when the server asked for a specific delay,
// otherwise for the current backoff plus jitter, then doubles the backoff.
func sleepBackoff(ctx context.Context, backoff *time.Duration, wait time.Duration) error {
	delay := wait
	if delay <= 0 {
		delay = *backoff + time.Duration(rand.Int64N(int64(*backoff/2)))
	}
	*backoff *= 2
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(delay):
		return nil
	}
}

// classify turns an error response into a typed error. The 404 case is the
// important one: for Notion it usually means the integration was never granted
// the resource, not that the resource is missing.
func classify(status int, body []byte) error {
	var payload struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	}
	json.Unmarshal(body, &payload)
	apiErr := &APIError{Status: status, Code: payload.Code, Message: payload.Message}

	switch {
	case status == http.StatusUnauthorized:
		return fmt.Errorf("%w: %s", ErrUnauthorized, apiErr.Message)
	case status == http.StatusNotFound && payload.Code == "object_not_found",
		// 403 restricted_resource is the explicit "not shared" signal; other
		// 403s are permission problems too, and lead to the same prompt.
		status == http.StatusForbidden:
		return fmt.Errorf("%w: %s", ErrNoAccess, apiErr.Message)
	}
	return apiErr
}

// NeedsReconnect reports whether an error means the user must reconnect their
// Notion workspace, as opposed to a problem with the request. Both a rejected
// token and an ungranted resource are fixed by walking the consent flow again.
func NeedsReconnect(err error) bool {
	return errors.Is(err, ErrUnauthorized) || errors.Is(err, ErrNoAccess)
}
