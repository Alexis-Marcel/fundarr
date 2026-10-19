package bitpanda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// client calls the Bitpanda Public API with the key in the x-api-key header.
// Reads are limited to 50 requests a second per key; a 429 or a server error
// is retried with a growing pause.
type client struct {
	base string
	key  string
	http *http.Client
	// onWait reports a pause before a retry, for the run log.
	onWait func(path string, d time.Duration, why string)
	// sleep is time.Sleep with a context; replaced in tests.
	sleep func(context.Context, time.Duration) error
}

// apiError is an error answer. Bitpanda writes either {"error": {"code"}}
// (documented) or {"errors": [{"code", "status", "title"}]} (what a bad key
// gets).
type apiError struct {
	Status int
	Path   string
	Code   string
	Title  string
}

func (e *apiError) Error() string {
	switch e.Status {
	case http.StatusUnauthorized:
		return "bitpanda: API key refused (wrong, expired or revoked)"
	case http.StatusForbidden:
		return fmt.Sprintf("bitpanda %s: access denied: the key needs the Balances and Transaction scopes", e.Path)
	}
	msg := e.Code
	if e.Title != "" {
		msg = strings.TrimSpace(e.Title + " " + msg)
	}
	if msg == "" {
		return fmt.Sprintf("bitpanda %s: HTTP %d", e.Path, e.Status)
	}
	return fmt.Sprintf("bitpanda %s: %s (HTTP %d)", e.Path, msg, e.Status)
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

const maxRetries = 5

// get calls a GET endpoint and decodes the JSON answer into out.
func (c *client) get(ctx context.Context, path string, q url.Values, out any) error {
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		wait, why, err := c.once(ctx, path, q, out)
		if err == nil || wait <= 0 || attempt >= maxRetries || ctx.Err() != nil {
			return err
		}
		if wait < backoff {
			wait = backoff
		}
		if wait > time.Minute {
			wait = time.Minute
		}
		backoff *= 2
		if c.onWait != nil {
			c.onWait(path, wait, why)
		}
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// once performs one request; when it is worth retrying it returns how long
// to wait (at least) and why.
func (c *client) once(ctx context.Context, path string, q url.Values, out any) (time.Duration, string, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, "", err
	}
	req.Header.Set("x-api-key", c.key)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, "", ctx.Err()
		}
		// Timeouts and dropped connections: worth another try. The error
		// names the URL, never the key (it only travels in a header).
		return time.Second, "network error", fmt.Errorf("bitpanda %s: %w", path, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return time.Second, "network error", fmt.Errorf("bitpanda %s: %w", path, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		wait := time.Second
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s >= 0 {
			wait = time.Duration(s) * time.Second
		}
		why := "rate limited"
		if resp.StatusCode >= 500 {
			why = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return wait, why, decodeError(resp.StatusCode, path, body)
	}
	if resp.StatusCode != http.StatusOK {
		return 0, "", decodeError(resp.StatusCode, path, body)
	}
	if out == nil {
		return 0, "", nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return 0, "", fmt.Errorf("bitpanda %s: unexpected answer: %w", path, err)
	}
	return 0, "", nil
}

func decodeError(status int, path string, body []byte) error {
	e := &apiError{Status: status, Path: path}
	var b struct {
		Error *struct {
			Code string `json:"code"`
		} `json:"error"`
		Errors []struct {
			Code  string `json:"code"`
			Title string `json:"title"`
		} `json:"errors"`
	}
	if json.Unmarshal(body, &b) == nil {
		switch {
		case b.Error != nil:
			e.Code = b.Error.Code
		case len(b.Errors) > 0:
			e.Code, e.Title = b.Errors[0].Code, b.Errors[0].Title
		}
	} else if s := strings.TrimSpace(string(body)); s != "" && !strings.HasPrefix(s, "<") {
		if len(s) > 200 {
			s = s[:200] + "…"
		}
		e.Title = s
	}
	return e
}

// isAuthError tells a key problem from a temporary one.
func isAuthError(err error) bool {
	var e *apiError
	return errors.As(err, &e) && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
}
