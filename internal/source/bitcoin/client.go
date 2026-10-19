package bitcoin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// client talks to an Esplora REST API (mempool.space, blockstream.info,
// a self-hosted electrs) or a Blockbook.
type client struct {
	base      string
	blockbook bool // base is a Blockbook, not an Esplora
	// fallback: other public servers tried in turn when base stops
	// answering (only with the default, never past a server the user chose).
	fallback []server
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

// down remembers, for every wallet of the process, public servers that
// stopped answering: the next run starts on one that works.
var down = struct {
	sync.Mutex
	until map[string]time.Time
}{until: map[string]time.Time{}}

const downFor = 10 * time.Minute

// reqTimeout bounds one request: a server that does not answer is left
// for the next one instead of holding the run.
const reqTimeout = 20 * time.Second

// failover moves past a server that failed, if another is left. Calls in
// parallel may fail on the same server: only the first one moves on, the
// others just retry on the new one.
func (c *client) failover(failed string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.base != failed {
		return true
	}
	if len(c.fallback) == 0 {
		return false
	}
	down.Lock()
	down.until[c.base] = time.Now().Add(downFor)
	down.Unlock()
	c.base, c.blockbook, c.fallback = c.fallback[0].url, c.fallback[0].blockbook, c.fallback[1:]
	return true
}

// skipDown starts on the first public server not known to be down.
func (c *client) skipDown() {
	down.Lock()
	defer down.Unlock()
	for len(c.fallback) > 0 && time.Now().Before(down.until[c.base]) {
		c.base, c.blockbook, c.fallback = c.fallback[0].url, c.fallback[0].blockbook, c.fallback[1:]
	}
}

// get fetches path and decodes JSON, retrying on rate limits and server
// errors, then on the next public server when this one stays unreachable.
func (c *client) get(ctx context.Context, path string, out any) error {
	backoff := 2 * time.Second
	for attempt := 0; ; attempt++ {
		// A server that cannot be reached is likely down or blocked on this
		// network: with another one to try, do not wait for it.
		limit := 5
		if len(c.fallback) > 0 {
			limit = 0
		}
		// Each request books its slot: calls in parallel still start pause
		// apart.
		c.mu.Lock()
		now := time.Now()
		slot := c.last.Add(c.pause)
		if slot.Before(now) {
			slot = now
		}
		c.last = slot
		base := c.base
		c.mu.Unlock()
		if err := c.sleep(ctx, slot.Sub(now)); err != nil {
			return err
		}
		rctx, cancel := context.WithTimeout(ctx, reqTimeout)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, base+path, nil)
		if err != nil {
			cancel()
			return err
		}
		resp, err := c.http.Do(req)
		var body []byte
		if err == nil {
			body, err = io.ReadAll(io.LimitReader(resp.Body, 32<<20))
			resp.Body.Close()
		}
		cancel()
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
			if ctx.Err() == nil && c.failover(base) {
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
			if c.failover(base) {
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

func (c *client) isBlockbook() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.blockbook
}

func (c *client) address(ctx context.Context, addr string) (addrStats, error) {
	if c.isBlockbook() {
		return c.bbAddress(ctx, addr)
	}
	var s addrStats
	err := c.get(ctx, "/address/"+addr, &s)
	return s, err
}

// scanWorkers: addresses asked at once while scanning.
const scanWorkers = 5

// addresses asks several addresses at once; the first error wins.
func (c *client) addresses(ctx context.Context, addrs []string) ([]addrStats, error) {
	out := make([]addrStats, len(addrs))
	errs := make([]error, len(addrs))
	sem := make(chan struct{}, scanWorkers)
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			out[i], errs[i] = c.address(ctx, a)
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// chainTxs returns every confirmed transaction of an address, 25 per page.
func (c *client) chainTxs(ctx context.Context, addr string) ([]tx, error) {
	if c.isBlockbook() {
		return c.bbTxs(ctx, addr)
	}
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

// ---- Blockbook: the same facts, other words.

type bbAddr struct {
	Balance        string `json:"balance"`
	TotalReceived  string `json:"totalReceived"`
	TotalSent      string `json:"totalSent"`
	Txs            int    `json:"txs"`
	UnconfirmedTxs int    `json:"unconfirmedTxs"`
	TotalPages     int    `json:"totalPages"`
	Transactions   []struct {
		TxID          string `json:"txid"`
		BlockTime     int64  `json:"blockTime"`
		Confirmations int64  `json:"confirmations"`
		Fees          string `json:"fees"`
		Vin           []bbIO `json:"vin"`
		Vout          []bbIO `json:"vout"`
	} `json:"transactions"`
}

type bbIO struct {
	Addresses []string `json:"addresses"`
	Value     string   `json:"value"`
}

func (io bbIO) output() *output {
	o := &output{}
	if len(io.Addresses) > 0 {
		o.Address = io.Addresses[0]
	}
	o.Value, _ = strconv.ParseInt(io.Value, 10, 64)
	return o
}

func (c *client) bbAddress(ctx context.Context, addr string) (addrStats, error) {
	var a bbAddr
	if err := c.get(ctx, "/address/"+addr+"?details=basic", &a); err != nil {
		return addrStats{}, err
	}
	var s addrStats
	s.Chain.TxCount = a.Txs - a.UnconfirmedTxs
	s.Chain.Funded, _ = strconv.ParseInt(a.TotalReceived, 10, 64)
	s.Chain.Spent, _ = strconv.ParseInt(a.TotalSent, 10, 64)
	s.Mempool.TxCount = a.UnconfirmedTxs
	return s, nil
}

func (c *client) bbTxs(ctx context.Context, addr string) ([]tx, error) {
	var all []tx
	for page := 1; ; page++ {
		var a bbAddr
		if err := c.get(ctx, fmt.Sprintf("/address/%s?details=txs&pageSize=1000&page=%d", addr, page), &a); err != nil {
			return nil, err
		}
		for _, bt := range a.Transactions {
			if bt.Confirmations <= 0 || bt.BlockTime == 0 {
				continue
			}
			t := tx{TxID: bt.TxID}
			t.Fee, _ = strconv.ParseInt(bt.Fees, 10, 64)
			t.Status.Confirmed, t.Status.BlockTime = true, bt.BlockTime
			for _, in := range bt.Vin {
				t.Vin = append(t.Vin, struct {
					Prevout *output `json:"prevout"`
				}{in.output()})
			}
			for _, o := range bt.Vout {
				t.Vout = append(t.Vout, *o.output())
			}
			all = append(all, t)
		}
		if page >= a.TotalPages {
			return all, nil
		}
	}
}
