package cardano

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// client talks to a Koios REST API (PostgREST: GET with query parameters
// or POST with a JSON body, rows paged by limit and offset).
type client struct {
	base  string
	key   string // bearer token, optional; only ever sent as a header
	http  *http.Client
	pause time.Duration
	sleep func(context.Context, time.Duration) error

	mu   sync.Mutex
	last time.Time
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

// call sends one request (POST when body is not nil) and decodes the JSON
// answer into out. Timeouts, 429 and 5xx are retried with a backoff.
func (c *client) call(ctx context.Context, path string, q url.Values, body, out any) error {
	u := c.base + "/" + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	backoff := 2 * time.Second
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		wait := c.pause - time.Since(c.last)
		c.mu.Unlock()
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
		method := http.MethodGet
		var rd io.Reader
		if body != nil {
			method, rd = http.MethodPost, bytes.NewReader(payload)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.key != "" {
			req.Header.Set("Authorization", "Bearer "+c.key)
		}
		resp, err := c.http.Do(req)
		c.mu.Lock()
		c.last = time.Now()
		c.mu.Unlock()
		var data []byte
		if err == nil {
			data, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
		}
		retry := err != nil || resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		if retry && ctx.Err() == nil && attempt < 5 {
			if err := c.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff *= 2
			continue
		}
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return fmt.Errorf("%s: %v", path, err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			return fmt.Errorf("%s: rate limited", path)
		}
		if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
			return fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, message(data))
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s: decoding response: %w", path, err)
		}
		return nil
	}
}

// message extracts Koios' error text (PostgREST {"message": …} or plain).
func message(b []byte) string {
	var e struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &e) == nil && e.Message != "" {
		return e.Message
	}
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// pageSize is the rows asked per call (Koios answers at most 1000).
var pageSize = 1000

// rows walks every page of an endpoint, calling each on every row.
func (c *client) rows(ctx context.Context, path string, q url.Values, body any, each func(json.RawMessage) error) error {
	for offset := 0; ; offset += pageSize {
		p := url.Values{}
		for k, v := range q {
			p[k] = v
		}
		p.Set("limit", strconv.Itoa(pageSize))
		p.Set("offset", strconv.Itoa(offset))
		var page []json.RawMessage
		if err := c.call(ctx, path, p, body, &page); err != nil {
			return err
		}
		for _, r := range page {
			if err := each(r); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
	}
}
