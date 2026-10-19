package bitcoin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// client talks to an Esplora REST API (mempool.space, blockstream.info,
// a self-hosted electrs).
type client struct {
	base string
	// fallback: other public servers tried in turn when base stops
	// answering (only with the default, never past a server the user chose).
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

type stats struct {
	TxCount int   `json:"tx_count"`
	Funded  int64 `json:"funded_txo_sum"`
	Spent   int64 `json:"spent_txo_sum"`
}

type addrStats struct {
	Chain   stats `json:"chain_stats"`
	Mempool stats `json:"mempool_stats"`
}

type output struct {
	Address string `json:"scriptpubkey_address"`
	Value   int64  `json:"value"`
}

type tx struct {
	TxID string `json:"txid"`
	Fee  int64  `json:"fee"`
	Vin  []struct {
		Prevout *output `json:"prevout"`
	} `json:"vin"`
	Vout   []output `json:"vout"`
	Status struct {
		Confirmed bool  `json:"confirmed"`
		BlockTime int64 `json:"block_time"`
	} `json:"status"`
}

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

// get fetches path and decodes JSON, retrying on rate limits and server
// errors, then on the next public server when this one stays unreachable.
func (c *client) get(ctx context.Context, path string, out any) error {
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
		c.mu.Unlock()
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return err
		}
		resp, err := c.http.Do(req)
		c.mu.Lock()
		c.last = time.Now()
		c.mu.Unlock()
		var body []byte
		if err == nil {
			body, err = io.ReadAll(io.LimitReader(resp.Body, 32<<20))
			resp.Body.Close()
		}
		if err != nil {
			// Timeouts and dropped connections are common on public servers:
			// retry them like a rate limit, unless the run was cancelled.
			if ctx.Err() == nil && attempt < limit {
				if err := c.sleep(ctx, backoff); err != nil {
					return err
				}
				backoff *= 2
				continue
			}
			if ctx.Err() == nil && c.failover() {
				attempt, backoff = -1, 2*time.Second
				continue
			}
			return fmt.Errorf("esplora: %w", err)
		}
		if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			if attempt < 5 {
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
		if resp.StatusCode != http.StatusOK {
			msg := strings.TrimSpace(string(body))
			if len(msg) > 200 {
				msg = msg[:200]
			}
			return fmt.Errorf("esplora %s: HTTP %d: %s", path, resp.StatusCode, msg)
		}
		if err := json.Unmarshal(body, out); err != nil {
			return fmt.Errorf("esplora %s: decoding response: %w", path, err)
		}
		return nil
	}
}

func (c *client) address(ctx context.Context, addr string) (addrStats, error) {
	var s addrStats
	err := c.get(ctx, "/address/"+addr, &s)
	return s, err
}

// chainTxs returns every confirmed transaction of an address, 25 per page.
func (c *client) chainTxs(ctx context.Context, addr string) ([]tx, error) {
	var all []tx
	path := "/address/" + addr + "/txs/chain"
	for {
		var page []tx
		if err := c.get(ctx, path, &page); err != nil {
			return nil, err
		}
		all = append(all, page...)
		if len(page) < pageSize {
			return all, nil
		}
		path = "/address/" + addr + "/txs/chain/" + page[len(page)-1].TxID
	}
}
