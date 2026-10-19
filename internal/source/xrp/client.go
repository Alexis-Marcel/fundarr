package xrp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Public JSON-RPC servers, all keeping full history (complete_ledgers from
// 32570) and answering without a key: tried in this order.
var servers = []string{"https://xrplcluster.com/", "https://s1.ripple.com:51234/", "https://s2.ripple.com:51234/"}

// client talks to an XRP Ledger JSON-RPC server (rippled or Clio).
type client struct {
	base string
	// fallback: the other public servers, tried in turn when base stops
	// answering (never past a server given in the settings).
	fallback []string
	http     *http.Client
	pause    time.Duration
	sleep    func(context.Context, time.Duration) error

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

// rpcError is an error the server answered (status "error" in the result).
type rpcError struct {
	Method, Code, Message string
}

func (e *rpcError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("xrpl %s: %s (%s)", e.Method, e.Message, e.Code)
	}
	return fmt.Sprintf("xrpl %s: %s", e.Method, e.Code)
}

// isCode reports whether err is the server's error code.
func isCode(err error, code string) bool {
	var re *rpcError
	return errors.As(err, &re) && re.Code == code
}

// busy: answers meaning "try again later" rather than "your request is wrong".
var busy = map[string]bool{"slowDown": true, "tooBusy": true, "noNetwork": true, "noCurrent": true,
	"noClosed": true, "notReady": true, "notSynced": true, "failedToForward": true, "internal": true}

// failover moves to the next public server, if any is left.
func (c *client) failover() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.fallback) == 0 {
		return false
	}
	c.base, c.fallback = c.fallback[0], c.fallback[1:]
	return true
}

// call runs one JSON-RPC method and decodes its result into out, retrying
// on rate limits, busy servers and network errors, then on the next public
// server when this one keeps failing.
func (c *client) call(ctx context.Context, method string, params map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"method": method, "params": []any{params}})
	if err != nil {
		return err
	}
	backoff := 2 * time.Second
	for attempt := 0; ; attempt++ {
		// A server that cannot be reached twice in a row is likely down or
		// blocked on this network: do not spend minutes on it.
		limit := 5
		if len(c.fallback) > 0 {
			limit = 1
		}
		c.mu.Lock()
		wait := c.pause - time.Since(c.last)
		base := c.base
		c.mu.Unlock()
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := c.http.Do(req)
		c.mu.Lock()
		c.last = time.Now()
		c.mu.Unlock()
		var raw []byte
		if err == nil {
			raw, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
		}
		retry := err != nil
		var env struct {
			Result json.RawMessage `json:"result"`
		}
		var head struct {
			Status  string `json:"status"`
			Error   string `json:"error"`
			Message string `json:"error_message"`
		}
		if err == nil {
			retry = resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
			if json.Unmarshal(raw, &env) == nil && json.Unmarshal(env.Result, &head) == nil && busy[head.Error] {
				retry = true // "slowDown" and the like, sometimes with HTTP 200
			}
		}
		if retry && ctx.Err() == nil {
			if attempt < limit {
				if err := c.sleep(ctx, backoff); err != nil {
					return err
				}
				backoff *= 2
				continue
			}
			if c.failover() {
				attempt, backoff = -1, 2*time.Second
				continue
			}
		}
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return fmt.Errorf("xrpl %s: %w", method, err)
		}
		if head.Error != "" {
			return &rpcError{Method: method, Code: head.Error, Message: head.Message}
		}
		if resp.StatusCode != http.StatusOK {
			msg := strings.TrimSpace(string(raw))
			if len(msg) > 200 {
				msg = msg[:200]
			}
			return fmt.Errorf("xrpl %s: HTTP %d: %s", method, resp.StatusCode, msg)
		}
		if len(env.Result) == 0 {
			return fmt.Errorf("xrpl %s: no result in the response", method)
		}
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("xrpl %s: decoding result: %w", method, err)
		}
		return nil
	}
}
