// Package ghapi is a small GitHub REST client covering exactly what the
// orchestrator needs: discovering queued jobs, minting just-in-time runner
// configs, and listing or removing org runners.
package ghapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chrisle/action-runner-cluster/internal/config"
)

const (
	apiVersion    = "2022-11-28"
	userAgent     = "action-runner-cluster"
	maxRetries    = 4
	maxRetrySleep = 30 * time.Second
)

// Client talks to the GitHub REST API.
type Client struct {
	apiURL string
	webURL string
	org    string
	auth   Authenticator
	http   *http.Client
	log    *slog.Logger

	etags *etagCache

	// rate holds the most recent rate limit snapshot, for `arc status`.
	rateMu sync.Mutex
	rate   RateLimit

	// groups caches runner group name -> id lookups.
	groupMu sync.Mutex
	groups  map[string]int64
}

// RateLimit is the last-seen state of the core API rate limit.
type RateLimit struct {
	Limit     int       `json:"limit"`
	Remaining int       `json:"remaining"`
	Reset     time.Time `json:"reset"`
	Seen      time.Time `json:"seen"`
}

// New builds a client from config.
func New(cfg *config.Config, log *slog.Logger) (*Client, error) {
	hc := &http.Client{Timeout: 30 * time.Second}

	var auth Authenticator
	switch {
	case cfg.GitHub.App != nil:
		a := cfg.GitHub.App
		aa, err := NewAppAuth(a.AppID, a.InstallationID, a.PrivateKey, a.PrivateKeyPath, cfg.GitHub.APIURL, hc)
		if err != nil {
			return nil, err
		}
		auth = aa
	case cfg.GitHub.Token != "":
		auth = &StaticToken{Value: cfg.GitHub.Token}
	default:
		return nil, errors.New("no GitHub credentials configured")
	}

	return &Client{
		apiURL: strings.TrimSuffix(cfg.GitHub.APIURL, "/"),
		webURL: strings.TrimSuffix(cfg.GitHub.WebURL, "/"),
		org:    cfg.GitHub.Org,
		auth:   auth,
		http:   hc,
		log:    log,
		etags:  newETagCache(),
	}, nil
}

// Org returns the configured organization.
func (c *Client) Org() string { return c.org }

// WebURL returns the base web URL runners register against.
func (c *Client) WebURL() string { return c.webURL }

// AuthDescription names the credential in use.
func (c *Client) AuthDescription() string { return c.auth.Describe() }

// RateLimit returns the most recent rate limit snapshot.
func (c *Client) RateLimit() RateLimit {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	return c.rate
}

// APIError is a non-2xx response.
type APIError struct {
	Status  int
	Message string
	URL     string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("github %s: %d %s", e.URL, e.Status, e.Message)
}

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusNotFound
}

// IsForbidden reports whether err is a 403.
func IsForbidden(err error) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == http.StatusForbidden
}

// request performs one authenticated request with retry on transient failure.
// When etagKey is non-empty the request is conditional: a 304 returns the
// cached body and, importantly, does not count against the REST rate limit.
func (c *Client) request(ctx context.Context, method, path string, body any, etagKey string) ([]byte, string, error) {
	url := path
	if !strings.HasPrefix(path, "http") {
		url = c.apiURL + path
	}

	var payload []byte
	if body != nil {
		var err error
		payload, err = json.Marshal(body)
		if err != nil {
			return nil, "", fmt.Errorf("encode request body: %w", err)
		}
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			sleep := backoff(attempt)
			c.log.Debug("retrying github request", "url", url, "attempt", attempt, "sleep", sleep)
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(sleep):
			}
		}

		token, err := c.auth.Token(ctx)
		if err != nil {
			return nil, "", err
		}

		var rdr io.Reader
		if payload != nil {
			rdr = bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, url, rdr)
		if err != nil {
			return nil, "", err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", apiVersion)
		req.Header.Set("User-Agent", userAgent)
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if etagKey != "" {
			if tag, ok := c.etags.tag(etagKey); ok {
				req.Header.Set("If-None-Match", tag)
			}
		}

		resp, err := c.http.Do(req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, "", ctx.Err()
			}
			lastErr = err
			continue
		}

		c.recordRateLimit(resp)
		next := nextLink(resp.Header.Get("Link"))

		if resp.StatusCode == http.StatusNotModified && etagKey != "" {
			resp.Body.Close()
			cached, ok := c.etags.body(etagKey)
			if ok {
				return cached, next, nil
			}
			// Cache was dropped between the tag and the body; force a refetch.
			c.etags.forget(etagKey)
			lastErr = errors.New("etag cache miss after 304")
			continue
		}

		data, readErr := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if readErr != nil {
			lastErr = readErr
			continue
		}

		if resp.StatusCode >= 200 && resp.StatusCode < 300 {
			if etagKey != "" {
				if tag := resp.Header.Get("ETag"); tag != "" {
					c.etags.store(etagKey, tag, data)
				}
			}
			return data, next, nil
		}

		if wait, ok := retryAfter(resp); ok && attempt < maxRetries {
			c.log.Warn("github rate limited", "url", url, "wait", wait)
			select {
			case <-ctx.Done():
				return nil, "", ctx.Err()
			case <-time.After(wait):
			}
			continue
		}

		apiErr := &APIError{Status: resp.StatusCode, Message: extractMessage(data), URL: url}
		// 5xx is worth another try; 4xx will fail identically every time.
		if resp.StatusCode >= 500 && attempt < maxRetries {
			lastErr = apiErr
			continue
		}
		return nil, "", apiErr
	}

	return nil, "", fmt.Errorf("github %s: %w", url, lastErr)
}

// getJSON does a GET and unmarshals into out.
func (c *Client) getJSON(ctx context.Context, path, etagKey string, out any) (string, error) {
	data, next, err := c.request(ctx, http.MethodGet, path, nil, etagKey)
	if err != nil {
		return "", err
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return "", fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return next, nil
}

// postJSON does a POST and unmarshals into out.
func (c *Client) postJSON(ctx context.Context, path string, body, out any) error {
	data, _, err := c.request(ctx, http.MethodPost, path, body, "")
	if err != nil {
		return err
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
	}
	return nil
}

// paginate walks Link-header pagination, calling fn with each page's body.
// etagPrefix, if non-empty, makes every page conditional.
func (c *Client) paginate(ctx context.Context, path, etagPrefix string, fn func([]byte) error) error {
	url := path
	page := 0
	for url != "" {
		key := ""
		if etagPrefix != "" {
			key = fmt.Sprintf("%s#%d", etagPrefix, page)
		}
		data, next, err := c.request(ctx, http.MethodGet, url, nil, key)
		if err != nil {
			return err
		}
		if err := fn(data); err != nil {
			return err
		}
		url, page = next, page+1
		if page > 200 { // hard stop against a pathological Link loop
			return fmt.Errorf("pagination exceeded 200 pages at %s", path)
		}
	}
	return nil
}

func (c *Client) recordRateLimit(resp *http.Response) {
	rem := resp.Header.Get("X-RateLimit-Remaining")
	if rem == "" {
		return
	}
	snap := RateLimit{Seen: time.Now()}
	snap.Remaining, _ = strconv.Atoi(rem)
	snap.Limit, _ = strconv.Atoi(resp.Header.Get("X-RateLimit-Limit"))
	if r, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		snap.Reset = time.Unix(r, 0)
	}
	c.rateMu.Lock()
	c.rate = snap
	c.rateMu.Unlock()
}

// retryAfter reports how long to wait when GitHub asks us to back off, either
// via Retry-After (secondary limits) or an exhausted primary limit.
func retryAfter(resp *http.Response) (time.Duration, bool) {
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil {
			return capSleep(time.Duration(secs) * time.Second), true
		}
	}
	if resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if resp.Header.Get("X-RateLimit-Remaining") != "0" {
		return 0, false
	}
	reset, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64)
	if err != nil {
		return 0, false
	}
	wait := time.Until(time.Unix(reset, 0))
	if wait <= 0 {
		return time.Second, true
	}
	return capSleep(wait), true
}

func capSleep(d time.Duration) time.Duration {
	if d > maxRetrySleep {
		return maxRetrySleep
	}
	if d < 0 {
		return 0
	}
	return d
}

func backoff(attempt int) time.Duration {
	d := time.Duration(1<<uint(attempt)) * 250 * time.Millisecond
	return capSleep(d)
}

var linkRE = regexp.MustCompile(`<([^>]+)>;\s*rel="next"`)

func nextLink(header string) string {
	m := linkRE.FindStringSubmatch(header)
	if len(m) == 2 {
		return m[1]
	}
	return ""
}

func extractMessage(data []byte) string {
	var e struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
			Code    string `json:"code"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(data, &e); err != nil || e.Message == "" {
		s := strings.TrimSpace(string(data))
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		if s == "" {
			s = "(empty response)"
		}
		return s
	}
	if len(e.Errors) > 0 && e.Errors[0].Message != "" {
		return e.Message + ": " + e.Errors[0].Message
	}
	return e.Message
}

func describeError(resp *http.Response) string {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return fmt.Sprintf("%d %s", resp.StatusCode, extractMessage(data))
}

// etagCache stores ETags and their response bodies so repeated polls of quiet
// repos return 304 and cost no rate limit.
type etagCache struct {
	mu      sync.Mutex
	entries map[string]etagEntry
}

type etagEntry struct {
	tag  string
	body []byte
	seen time.Time
}

func newETagCache() *etagCache {
	return &etagCache{entries: make(map[string]etagEntry)}
}

func (e *etagCache) tag(key string) (string, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ent, ok := e.entries[key]
	return ent.tag, ok
}

func (e *etagCache) body(key string) ([]byte, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ent, ok := e.entries[key]
	if !ok {
		return nil, false
	}
	ent.seen = time.Now()
	e.entries[key] = ent
	return ent.body, true
}

func (e *etagCache) store(key, tag string, body []byte) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.entries[key] = etagEntry{tag: tag, body: body, seen: time.Now()}
}

func (e *etagCache) forget(key string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	delete(e.entries, key)
}

// Sweep drops cache entries untouched for the given age, so repos that go quiet
// or get deleted don't pin memory forever.
func (e *etagCache) Sweep(maxAge time.Duration) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cutoff := time.Now().Add(-maxAge)
	for k, v := range e.entries {
		if v.seen.Before(cutoff) {
			delete(e.entries, k)
		}
	}
}

// SweepCache prunes stale conditional-request cache entries.
func (c *Client) SweepCache(maxAge time.Duration) { c.etags.Sweep(maxAge) }
