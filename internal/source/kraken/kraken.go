// Package kraken connects a Kraken account through the spot REST API with a
// read-only key ("Query Funds" and "Query Ledger Entries").
//
// History comes from the ledger (POST /0/private/Ledgers), which lists every
// balance change: trades, Buy Crypto, deposits, withdrawals, staking and Earn.
// Kraken is treated as one account: the spot wallet and the staking/Earn
// variants of an asset (DOT.S, ETH.F, XBT.M…) are summed under the base coin,
// and moves between them are not activities.
//
// Not read: deposit/withdrawal networks and transaction hashes (another
// permission), Kraken Futures (a separate account), NFT details.
package kraken

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

const defaultBase = "https://api.kraken.com"

func init() {
	source.Register(source.Definition{
		Kind:        "kraken",
		Name:        "Kraken",
		Category:    "exchange",
		Description: "Crypto exchange.",
		Fields: []source.Field{
			{Name: "api_key", Label: "API key"},
			{Name: "api_secret", Label: "Private key", Secret: true},
		},
		Setup: "In Kraken, open Settings → API → Create API key. " +
			"Tick only \"Query Funds\" and \"Query Ledger Entries\".",
		Limits: "Kraken's ledger gives no network or transaction hash for deposits and withdrawals.",
		New:    New,
	})
}

// New builds the connector from its settings.
func New(cfg source.Config) (source.Source, error) {
	key := strings.TrimSpace(cfg.Get("api_key"))
	raw := strings.TrimSpace(cfg.Get("api_secret"))
	if key == "" || raw == "" {
		return nil, errors.New("kraken: API key and private key are required")
	}
	secret, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("kraken: the private key is not valid base64: copy it whole")
	}
	c := &client{base: defaultBase, key: key, secret: secret, http: cfg.HTTP, now: time.Now, sleep: sleepCtx}
	if c.http == nil {
		c.http = &http.Client{Timeout: 60 * time.Second}
	}
	s := &Source{c: c}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &s.st); err != nil {
			return nil, fmt.Errorf("kraken: bad saved state: %w", err)
		}
	}
	return s, nil
}

// Source is a configured Kraken connector.
type Source struct {
	c  *client
	st state

	namesOnce sync.Once
	names     map[string]string // asset code → altname, from /0/public/Assets
	namesErr  error
}

// state is kept between runs.
type state struct {
	// Last is the time of the newest ledger entry processed (Unix seconds,
	// as Kraken writes it).
	Last string `json:"last,omitempty"`
}

func (s *Source) State() []byte {
	b, _ := json.Marshal(s.st)
	return b
}

// ---- HTTP client

type client struct {
	base, key string
	secret    []byte
	http      *http.Client
	now       func() time.Time
	sleep     func(context.Context, time.Duration) error

	mu      sync.Mutex
	nonce   int64
	counter float64   // Kraken's API call counter, as we estimate it
	at      time.Time // when counter was last updated
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// APIError is an error reported by Kraken in the response body.
type APIError struct {
	Path   string
	Errors []string
	Msg    string // what to do about it, when known
}

func (e *APIError) Error() string {
	if e.Msg != "" {
		return fmt.Sprintf("kraken %s: %s (%s)", e.Path, e.Msg, strings.Join(e.Errors, ", "))
	}
	return fmt.Sprintf("kraken %s: %s", e.Path, strings.Join(e.Errors, ", "))
}

// sign returns the API-Sign value: HMAC-SHA512 of the URI path followed by
// SHA256(nonce + POST data), keyed with the decoded private key.
func sign(secret []byte, path, nonce, postdata string) string {
	sum := sha256.Sum256([]byte(nonce + postdata))
	m := hmac.New(sha512.New, secret)
	m.Write([]byte(path))
	m.Write(sum[:])
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// The call counter of a Starter account (the lowest tier): it may reach 15
// and goes down by 0.33 per second. Ledgers costs 2, other calls 1.
const (
	counterMax   = 15
	counterDecay = 0.33
)

// throttle waits until a call of this cost fits under the counter.
func (c *client) throttle(ctx context.Context, cost float64) error {
	c.mu.Lock()
	now := c.now()
	if !c.at.IsZero() {
		c.counter = max(0, c.counter-now.Sub(c.at).Seconds()*counterDecay)
	}
	c.at = now
	var wait time.Duration
	if over := c.counter + cost - counterMax; over > 0 {
		wait = time.Duration(over / counterDecay * float64(time.Second))
		c.counter -= over
	}
	c.counter += cost
	c.mu.Unlock()
	if wait > 0 {
		return c.sleep(ctx, wait)
	}
	return nil
}

func (c *client) nextNonce() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := c.now().UnixMilli()
	if n <= c.nonce {
		n = c.nonce + 1
	}
	c.nonce = n
	return strconv.FormatInt(n, 10)
}

// What each private endpoint needs, for permission errors.
var permissions = map[string]string{
	"/0/private/Ledgers":   "Query Ledger Entries",
	"/0/private/BalanceEx": "Query Funds",
}

// private sends a signed POST and decodes "result" into out.
func (c *client) private(ctx context.Context, path string, form url.Values, out any) error {
	cost := 1.0
	if path == "/0/private/Ledgers" {
		cost = 2
	}
	if form == nil {
		form = url.Values{}
	}
	backoff := 3 * time.Second
	for attempt := 0; ; attempt++ {
		if err := c.throttle(ctx, cost); err != nil {
			return err
		}
		nonce := c.nextNonce()
		form.Set("nonce", nonce)
		body := form.Encode()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+path, strings.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("API-Key", c.key)
		req.Header.Set("API-Sign", sign(c.secret, path, nonce, body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
		req.Header.Set("Accept", "application/json")
		retry, err := c.send(req, path, out)
		if retry && attempt < 5 {
			var ae *APIError
			if errors.As(err, &ae) {
				c.mu.Lock()
				c.counter = counterMax // Kraken says we are at the limit
				c.mu.Unlock()
			}
			if err := c.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff *= 2
			continue
		}
		return err
	}
}

// public sends an unsigned GET (no call counter).
func (c *client) public(ctx context.Context, path string, out any) error {
	backoff := 2 * time.Second
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+path, nil)
		if err != nil {
			return err
		}
		req.Header.Set("Accept", "application/json")
		retry, err := c.send(req, path, out)
		if retry && attempt < 3 {
			if err := c.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff *= 2
			continue
		}
		return err
	}
}

// send runs one request; retry says whether trying again may help (rate
// limit, server busy, network trouble).
func (c *client) send(req *http.Request, path string, out any) (retry bool, err error) {
	resp, err := c.http.Do(req)
	if err != nil {
		if req.Context().Err() != nil {
			return false, req.Context().Err()
		}
		return true, fmt.Errorf("kraken %s: %w", path, err)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	if err != nil {
		return true, fmt.Errorf("kraken %s: %w", path, err)
	}
	var env struct {
		Error  []string        `json:"error"`
		Result json.RawMessage `json:"result"`
	}
	jsonErr := json.Unmarshal(raw, &env)
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5 {
		return true, fmt.Errorf("kraken %s: HTTP %d: %s", path, resp.StatusCode, snippet(raw))
	}
	if jsonErr != nil {
		return false, fmt.Errorf("kraken %s: HTTP %d: %s", path, resp.StatusCode, snippet(raw))
	}
	if len(env.Error) > 0 {
		e := &APIError{Path: path, Errors: env.Error}
		all := strings.Join(env.Error, " ")
		switch {
		case strings.Contains(all, "Rate limit exceeded"), strings.Contains(all, "EService:Unavailable"),
			strings.Contains(all, "EService:Busy"):
			return true, e
		case strings.Contains(all, "Permission denied"):
			e.Msg = "this API key lacks a permission"
			if p := permissions[path]; p != "" {
				e.Msg = fmt.Sprintf("this API key lacks the %q permission", p)
			}
		case strings.Contains(all, "Invalid key"):
			e.Msg = "unknown API key: check it was copied whole"
		case strings.Contains(all, "Invalid signature"):
			e.Msg = "bad signature: check the private key was copied whole"
		case strings.Contains(all, "Invalid nonce"):
			e.Msg = "nonce refused: give Fundarr a key of its own, used nowhere else"
		case strings.Contains(all, "Temporary lockout"):
			e.Msg = "too many failed calls: Kraken locked the key for a few minutes"
		}
		return false, e
	}
	if resp.StatusCode/100 != 2 {
		return false, fmt.Errorf("kraken %s: HTTP %d", path, resp.StatusCode)
	}
	if out == nil {
		return false, nil
	}
	if err := json.Unmarshal(env.Result, out); err != nil {
		return false, fmt.Errorf("kraken %s: unexpected result: %w", path, err)
	}
	return false, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ---- small helpers

// num is a decimal Kraken writes either as a string or as a number.
type num string

func (n *num) UnmarshalJSON(b []byte) error {
	*n = num(strings.Trim(string(b), `"`))
	return nil
}

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return decimal.Zero
	}
	return d
}

// unixTime reads Kraken's "1688464484.1787" (seconds, with a fraction).
func unixTime(s string) time.Time {
	d := dec(s)
	if !d.IsPositive() {
		return time.Time{}
	}
	sec := d.IntPart()
	nano := d.Sub(decimal.NewFromInt(sec)).Shift(9).IntPart()
	return time.Unix(sec, nano).UTC()
}

func progress(f source.Fetch, format string, args ...any) {
	if f.Progress != nil {
		f.Progress(format, args...)
	}
}

// ---- Test

// Test checks both permissions with one ledger page and the balances.
func (s *Source) Test(ctx context.Context) error {
	if err := s.c.private(ctx, "/0/private/Ledgers", url.Values{"without_count": {"true"}}, nil); err != nil {
		return err
	}
	return s.c.private(ctx, "/0/private/BalanceEx", nil, nil)
}

// ---- Balances

func (s *Source) Balances(ctx context.Context) ([]model.Balance, error) {
	var res map[string]struct {
		Balance num `json:"balance"`
	}
	if err := s.c.private(ctx, "/0/private/BalanceEx", nil, &res); err != nil {
		return nil, err
	}
	names := s.assetNames(ctx)
	total := map[string]decimal.Decimal{}
	for code, b := range res {
		sym := symbol(code, names)
		if sym == "" {
			continue
		}
		total[sym] = total[sym].Add(dec(string(b.Balance)))
	}
	out := make([]model.Balance, 0, len(total))
	for sym, v := range total {
		if v.IsPositive() {
			out = append(out, model.Balance{Amount: model.Amount{Asset: model.CryptoOrFiat(sym), Qty: v}})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset.Symbol < out[j].Asset.Symbol })
	return out, nil
}
