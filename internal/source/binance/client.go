package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

// client signs requests to the Binance REST API (spot "api" and wallet "sapi").
type client struct {
	base   string
	key    string
	secret []byte
	http   *http.Client
	// pause between calls, to stay far below rate limits.
	pause time.Duration
	// slow paths cost so much weight that they need their own spacing
	// (fiat history: 45000 UID weight a call, 180000 a minute).
	slow     map[string]time.Duration
	lastSlow map[string]time.Time
	// onWait reports a rate-limit wait, for the run log.
	onWait func(path string, d time.Duration)
	// sleep is time.Sleep with a context; replaced in tests.
	sleep func(context.Context, time.Duration) error

	mu     sync.Mutex
	offset time.Duration // server time - local time
	synced bool
	last   time.Time
}

// apiError is Binance's error body: {"code": -2015, "msg": "..."}.
type apiError struct {
	Status int
	Code   int    `json:"code"`
	Msg    string `json:"msg"`
}

func (e *apiError) Error() string {
	switch {
	case e.Code != 0:
		return fmt.Sprintf("binance: %s (code %d, HTTP %d)", e.Msg, e.Code, e.Status)
	case e.Msg != "":
		return fmt.Sprintf("binance: %s (HTTP %d)", e.Msg, e.Status)
	}
	return fmt.Sprintf("binance: HTTP %d", e.Status)
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

// now returns the current time corrected to Binance's clock, so signed
// requests are not rejected for being outside recvWindow.
func (c *client) now(ctx context.Context) time.Time {
	c.mu.Lock()
	synced := c.synced
	c.mu.Unlock()
	if !synced {
		var r struct {
			ServerTime int64 `json:"serverTime"`
		}
		before := time.Now()
		if err := c.do(ctx, http.MethodGet, "/api/v3/time", nil, false, &r); err == nil && r.ServerTime > 0 {
			mid := before.Add(time.Since(before) / 2)
			c.mu.Lock()
			c.offset = time.UnixMilli(r.ServerTime).Sub(mid)
			c.mu.Unlock()
		}
		c.mu.Lock()
		c.synced = true
		c.mu.Unlock()
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return time.Now().Add(c.offset)
}

// get calls a signed GET endpoint.
func (c *client) get(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, q, true, out)
}

// post calls a signed POST endpoint (parameters in the query string).
func (c *client) post(ctx context.Context, path string, q url.Values, out any) error {
	return c.do(ctx, http.MethodPost, path, q, true, out)
}

func (c *client) do(ctx context.Context, method, path string, q url.Values, signed bool, out any) error {
	for attempt := 0; ; attempt++ {
		retry, err := c.once(ctx, method, path, q, signed, out)
		if err == nil || retry <= 0 || attempt >= 5 {
			return err
		}
		// Binance often sends no Retry-After for account (UID) limits: back
		// off for real, longer each time.
		if min := time.Duration(attempt+1) * 20 * time.Second; retry < min {
			retry = min
		}
		if retry > 2*time.Minute {
			retry = 2 * time.Minute
		}
		if c.onWait != nil {
			c.onWait(path, retry)
		}
		if err := c.sleep(ctx, retry); err != nil {
			return err
		}
	}
}

// once performs one request; on 429/418 it returns how long to wait.
func (c *client) once(ctx context.Context, method, path string, q url.Values, signed bool, out any) (time.Duration, error) {
	c.mu.Lock()
	wait := c.pause - time.Since(c.last)
	if gap, ok := c.slow[path]; ok {
		if w := gap - time.Since(c.lastSlow[path]); w > wait {
			wait = w
		}
	}
	c.mu.Unlock()
	if err := c.sleep(ctx, wait); err != nil {
		return 0, err
	}
	params := url.Values{}
	for k, v := range q {
		params[k] = v
	}
	if signed {
		params.Set("timestamp", strconv.FormatInt(c.now(ctx).UnixMilli(), 10))
		params.Set("recvWindow", "10000")
	}
	query := params.Encode()
	if signed {
		m := hmac.New(sha256.New, c.secret)
		m.Write([]byte(query))
		query += "&signature=" + hex.EncodeToString(m.Sum(nil))
	}
	u := c.base + path
	if query != "" {
		u += "?" + query
	}
	req, err := http.NewRequestWithContext(ctx, method, u, nil)
	if err != nil {
		return 0, err
	}
	if signed {
		req.Header.Set("X-MBX-APIKEY", c.key)
	}
	resp, err := c.http.Do(req)
	c.mu.Lock()
	c.last = time.Now()
	if _, ok := c.slow[path]; ok {
		if c.lastSlow == nil {
			c.lastSlow = map[string]time.Time{}
		}
		c.lastSlow[path] = c.last
	}
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusTeapot {
		after := 60 * time.Second
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s >= 0 {
			after = time.Duration(s) * time.Second
		}
		return after + time.Second, &apiError{Status: resp.StatusCode, Msg: "rate limited"}
	}
	if resp.StatusCode != http.StatusOK {
		e := &apiError{Status: resp.StatusCode}
		_ = json.Unmarshal(body, e)
		if e.Msg == "" {
			e.Msg = strings.TrimSpace(string(body))
			if len(e.Msg) > 200 {
				e.Msg = e.Msg[:200]
			}
		}
		return 0, e
	}
	if out == nil {
		return 0, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return 0, fmt.Errorf("binance %s: decoding response: %w", path, err)
	}
	return 0, nil
}

// isAuthError tells a credential problem from a temporary one.
func isAuthError(err error) bool {
	var e *apiError
	if !errors.As(err, &e) {
		return false
	}
	switch e.Code {
	case -2014, -2015, -1022, -2008:
		return true
	}
	return e.Status == http.StatusUnauthorized
}
