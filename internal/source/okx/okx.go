// Package okx connects an OKX account through the v5 REST API with a
// read-only key. OKX is treated as one account: funding, trading and Simple
// Earn balances are summed, and moves between them are not activities.
//
// History comes from the account ledgers ("bills"), which list every balance
// change, rather than from order fills, because they reach furthest back:
//   - funding account: GET /api/v5/asset/bills-history, since 2021-02-01;
//   - trading account: GET /api/v5/account/bills-archive (last 3 months),
//     plus the quarterly archive files since 2021 (POST then GET
//     /api/v5/account/bills-history-archive; a requested file is ready about
//     2 hours later, so older quarters arrive on a later run);
//   - deposits and withdrawals: /api/v5/asset/deposit-history and
//     withdrawal-history (they carry the chain, tx hash and withdrawal fee);
//   - Simple Earn interest: /api/v5/finance/savings/lending-history (last
//     month only: older interest stays visible as a reconciliation gap).
package okx

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
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
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

const defaultBase = "https://www.okx.com"

func init() {
	source.Register(source.Definition{
		Kind:        "okx",
		Name:        "OKX",
		Category:    "exchange",
		Description: "Crypto exchange: deposits, withdrawals, spot trades, converts, earn and staking rewards.",
		Fields: []source.Field{
			{Name: "api_key", Label: "API key"},
			{Name: "api_secret", Label: "Secret key", Secret: true},
			{Name: "passphrase", Label: "Passphrase", Secret: true},
			{Name: "base_url", Label: "API domain", Optional: true,
				Help: "leave empty: Fundarr finds the right one (www.okx.com, eea.okx.com for European accounts, us.okx.com)"},
		},
		Setup: "In OKX, open Profile → API keys → Create API key (API v5). Tick only the \"Read\" permission: " +
			"no Trade, no Withdraw. Restrict it to your server's IP address if you can. Copy the API key, " +
			"the secret key and the passphrase you chose.",
		New: New,
	})
}

// New builds the connector from its settings.
func New(cfg source.Config) (source.Source, error) {
	c := &client{
		base:   strings.TrimRight(strings.TrimSpace(cfg.Get("base_url")), "/"),
		key:    strings.TrimSpace(cfg.Get("api_key")),
		secret: strings.TrimSpace(cfg.Get("api_secret")),
		pass:   cfg.Get("passphrase"),
		http:   cfg.HTTP,
		now:    time.Now,
		sleep:  sleepCtx,
	}
	if c.base == "" {
		c.base = defaultBase
		c.auto = true
	} else if !strings.HasPrefix(c.base, "http") {
		c.base = "https://" + c.base
	}
	if c.http == nil {
		c.http = &http.Client{Timeout: 60 * time.Second}
	}
	if c.key == "" || c.secret == "" || c.pass == "" {
		return nil, errors.New("okx: API key, secret key and passphrase are required")
	}
	s := &Source{c: c, st: state{Archive: map[string]string{}}}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &s.st); err != nil {
			return nil, fmt.Errorf("okx: bad saved state: %w", err)
		}
		if s.st.Archive == nil {
			s.st.Archive = map[string]string{}
		}
	}
	return s, nil
}

// Source is a configured OKX connector.
type Source struct {
	c  *client
	st state
}

// state is kept between runs.
type state struct {
	// Archive tracks the trading-account quarterly files: "2023Q4" →
	// "applied" (requested, not ready yet) or "done" (imported).
	Archive map[string]string `json:"archive"`
}

func (s *Source) State() []byte {
	b, _ := json.Marshal(s.st)
	return b
}

// ---- HTTP client

// OKX serves each region from its own domain; a key made on one does not
// exist on the others (code 50119). Without a configured domain, the client
// tries them in turn.
var regionBases = []string{"https://eea.okx.com", "https://us.okx.com"}

type client struct {
	base, key, secret, pass string
	auto                    bool // base not configured: may switch region
	tried                   int  // regions tried so far
	http                    *http.Client
	now                     func() time.Time
	sleep                   func(context.Context, time.Duration) error
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

// APIError is an error reported by OKX in the response body.
type APIError struct {
	Code string
	Msg  string
	Path string
}

func (e *APIError) Error() string { return fmt.Sprintf("okx %s: %s (code %s)", e.Path, e.Msg, e.Code) }

// sign returns the OK-ACCESS-SIGN value for one request.
func sign(secret, ts, method, requestPath, body string) string {
	m := hmac.New(sha256.New, []byte(secret))
	m.Write([]byte(ts + method + requestPath + body))
	return base64.StdEncoding.EncodeToString(m.Sum(nil))
}

// retryable codes: rate limit reached, system busy, sub-account rate limit.
var retryable = map[string]bool{"50011": true, "50013": true, "50061": true}

// do sends a signed request and decodes the "data" array into out.
func (c *client) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	requestPath := path
	if len(q) > 0 {
		requestPath += "?" + q.Encode()
	}
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}
	backoff := time.Second
	for attempt := 0; ; attempt++ {
		ts := c.now().UTC().Format("2006-01-02T15:04:05.000Z")
		req, err := http.NewRequestWithContext(ctx, method, c.base+requestPath, bytes.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("OK-ACCESS-KEY", c.key)
		req.Header.Set("OK-ACCESS-SIGN", sign(c.secret, ts, method, requestPath, string(payload)))
		req.Header.Set("OK-ACCESS-TIMESTAMP", ts)
		req.Header.Set("OK-ACCESS-PASSPHRASE", c.pass)
		req.Header.Set("Accept", "application/json")
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return fmt.Errorf("okx %s: %w", path, err)
		}
		raw, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
		resp.Body.Close()
		if err != nil {
			return fmt.Errorf("okx %s: %w", path, err)
		}
		var env struct {
			Code string          `json:"code"`
			Msg  string          `json:"msg"`
			Data json.RawMessage `json:"data"`
		}
		jsonErr := json.Unmarshal(raw, &env)
		limited := resp.StatusCode == http.StatusTooManyRequests || (jsonErr == nil && retryable[env.Code])
		if limited && attempt < 5 {
			if err := c.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff *= 2
			continue
		}
		if jsonErr != nil {
			return fmt.Errorf("okx %s: HTTP %d: %s", path, resp.StatusCode, snippet(raw))
		}
		if env.Code == "50119" && c.auto && c.tried < len(regionBases) {
			c.base = regionBases[c.tried]
			c.tried++
			continue
		}
		if env.Code == "50119" && c.auto {
			return &APIError{Code: env.Code, Path: path, Msg: "this API key exists on none of OKX's domains (www, eea, us): check it was copied whole and is not a demo-trading key"}
		}
		if env.Code != "0" {
			return &APIError{Code: env.Code, Msg: env.Msg, Path: path}
		}
		if resp.StatusCode/100 != 2 {
			return fmt.Errorf("okx %s: HTTP %d", path, resp.StatusCode)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("okx %s: unexpected data: %w", path, err)
		}
		return nil
	}
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// ---- small helpers

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return decimal.Zero
	}
	return d
}

func msTime(s string) time.Time {
	v, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	if err != nil || v <= 0 {
		return time.Time{}
	}
	return time.UnixMilli(v).UTC()
}

func msStr(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

func progress(f source.Fetch, format string, args ...any) {
	if f.Progress != nil {
		f.Progress(format, args...)
	}
}

// ---- Test: the key must be read-only

func (s *Source) Test(ctx context.Context) error {
	var cfg []struct {
		Perm string `json:"perm"`
	}
	if err := s.c.do(ctx, http.MethodGet, "/api/v5/account/config", nil, nil, &cfg); err != nil {
		return err
	}
	if len(cfg) == 0 {
		return errors.New("okx: empty account configuration")
	}
	perm := strings.ToLower(cfg[0].Perm)
	if strings.Contains(perm, "trade") || strings.Contains(perm, "withdraw") {
		return fmt.Errorf("okx: this API key has %q permissions; create a key with only the Read permission", cfg[0].Perm)
	}
	return nil
}

// ---- Balances

func (s *Source) Balances(ctx context.Context) ([]model.Balance, error) {
	total := map[string]decimal.Decimal{}
	add := func(ccy string, v decimal.Decimal) {
		ccy = strings.ToUpper(strings.TrimSpace(ccy))
		if ccy == "" || v.IsZero() {
			return
		}
		total[ccy] = total[ccy].Add(v)
	}

	var trading []struct {
		Details []struct {
			Ccy     string `json:"ccy"`
			CashBal string `json:"cashBal"`
		} `json:"details"`
	}
	if err := s.c.do(ctx, http.MethodGet, "/api/v5/account/balance", nil, nil, &trading); err != nil {
		return nil, err
	}
	for _, t := range trading {
		for _, d := range t.Details {
			add(d.Ccy, dec(d.CashBal))
		}
	}

	var funding []struct {
		Ccy string `json:"ccy"`
		Bal string `json:"bal"`
	}
	if err := s.c.do(ctx, http.MethodGet, "/api/v5/asset/balances", nil, nil, &funding); err != nil {
		return nil, err
	}
	for _, f := range funding {
		add(f.Ccy, dec(f.Bal))
	}

	var savings []struct {
		Ccy string `json:"ccy"`
		Amt string `json:"amt"`
	}
	if err := s.c.do(ctx, http.MethodGet, "/api/v5/finance/savings/balance", nil, nil, &savings); err != nil {
		return nil, err
	}
	for _, sv := range savings {
		add(sv.Ccy, dec(sv.Amt))
	}

	out := make([]model.Balance, 0, len(total))
	for ccy, v := range total {
		out = append(out, model.Balance{Amount: model.Amount{Asset: model.CryptoOrFiat(ccy), Qty: v}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset.Symbol < out[j].Asset.Symbol })
	return out, nil
}

// ---- Activities

// overlap re-reads a little before the last run: records can appear late.
const overlap = 72 * time.Hour

func (s *Source) Activities(ctx context.Context, f source.Fetch) error {
	var cutoff time.Time
	if !f.Since.IsZero() {
		cutoff = f.Since.Add(-overlap)
	}
	emit := func(a model.Activity) error {
		if a.Base.Asset.Symbol == "" || a.Base.Qty.IsZero() {
			return nil
		}
		return f.Emit(a)
	}

	types, err := s.billTypes(ctx)
	if err != nil {
		progress(f, "bill types unavailable (%v): using built-in rules", err)
	}

	progress(f, "deposits")
	if err := s.deposits(ctx, cutoff, emit); err != nil {
		return err
	}
	progress(f, "withdrawals")
	if err := s.withdrawals(ctx, cutoff, emit); err != nil {
		return err
	}

	progress(f, "funding account ledger")
	fb, err := s.fundingBills(ctx, cutoff)
	if err != nil {
		return err
	}
	if err := mapFundingBills(fb, emit); err != nil {
		return err
	}

	progress(f, "trading account ledger (last 3 months)")
	tb, err := s.tradingBills(ctx, cutoff)
	if err != nil {
		return err
	}
	if err := mapTradingBills(tb, types, emit); err != nil {
		return err
	}

	if err := s.archives(ctx, f, types, emit); err != nil {
		return err
	}

	progress(f, "Simple Earn interest (last month)")
	return s.lending(ctx, cutoff, emit)
}

// ---- deposits and withdrawals

type deposit struct {
	Ccy   string `json:"ccy"`
	Chain string `json:"chain"`
	Amt   string `json:"amt"`
	To    string `json:"to"`
	TxID  string `json:"txId"`
	Ts    string `json:"ts"`
	State string `json:"state"`
	DepID string `json:"depId"`
	From  string `json:"from"`
}

// pageByTs walks an endpoint sorted newest first and paginated with "after"
// (records strictly older than a timestamp), stopping at cutoff.
func pageByTs[T any](ctx context.Context, c *client, path string, cutoff time.Time, ts func(T) string, id func(T) string, visit func(T) error) error {
	after := ""
	seen := map[string]bool{}
	for {
		q := url.Values{"limit": {"100"}}
		if after != "" {
			q.Set("after", after)
		}
		var page []T
		if err := c.do(ctx, http.MethodGet, path, q, nil, &page); err != nil {
			return err
		}
		if len(page) == 0 {
			return nil
		}
		oldest := int64(0)
		fresh := 0
		for _, r := range page {
			t := msTime(ts(r))
			if oldest == 0 || t.UnixMilli() < oldest {
				oldest = t.UnixMilli()
			}
			if seen[id(r)] {
				continue
			}
			seen[id(r)] = true
			fresh++
			if !cutoff.IsZero() && t.Before(cutoff) {
				continue
			}
			if err := visit(r); err != nil {
				return err
			}
		}
		if len(page) < 100 || (!cutoff.IsZero() && time.UnixMilli(oldest).Before(cutoff)) {
			return nil
		}
		// Ask again from the oldest timestamp included, so records sharing it
		// are not lost; if a whole page shares it, move past it.
		next := strconv.FormatInt(oldest+1, 10)
		if fresh == 0 || next == after {
			next = strconv.FormatInt(oldest, 10)
		}
		if next == after { // cannot move further back
			return nil
		}
		after = next
	}
}

func (s *Source) deposits(ctx context.Context, cutoff time.Time, emit func(model.Activity) error) error {
	return pageByTs(ctx, s.c, "/api/v5/asset/deposit-history", cutoff,
		func(d deposit) string { return d.Ts }, func(d deposit) string { return d.DepID },
		func(d deposit) error {
			// 1: credited, 2: successful. Others are pending or blocked.
			if d.State != "1" && d.State != "2" {
				return nil
			}
			note := "OKX deposit"
			if d.From != "" {
				note = "OKX internal transfer from " + d.From
			}
			return emit(model.Activity{
				NativeID: "dep:" + d.DepID, Time: msTime(d.Ts), Type: model.Deposit,
				Base:    model.Amount{Asset: model.CryptoOrFiat(d.Ccy), Qty: dec(d.Amt)},
				Network: d.Chain, TxID: d.TxID, Address: d.To, Note: note,
			})
		})
}

type withdrawal struct {
	Ccy    string `json:"ccy"`
	Chain  string `json:"chain"`
	Amt    string `json:"amt"`
	Fee    string `json:"fee"`
	FeeCcy string `json:"feeCcy"`
	To     string `json:"to"`
	TxID   string `json:"txId"`
	Ts     string `json:"ts"`
	State  string `json:"state"`
	WdID   string `json:"wdId"`
}

func (s *Source) withdrawals(ctx context.Context, cutoff time.Time, emit func(model.Activity) error) error {
	return pageByTs(ctx, s.c, "/api/v5/asset/withdrawal-history", cutoff,
		func(w withdrawal) string { return w.Ts }, func(w withdrawal) string { return w.WdID },
		func(w withdrawal) error {
			if w.State != "2" { // only successful withdrawals
				return nil
			}
			a := model.Activity{
				NativeID: "wd:" + w.WdID, Time: msTime(w.Ts), Type: model.Withdrawal,
				Base:    model.Amount{Asset: model.CryptoOrFiat(w.Ccy), Qty: dec(w.Amt)},
				Network: w.Chain, TxID: w.TxID, Address: w.To, Note: "OKX withdrawal",
			}
			if fee := dec(w.Fee); fee.IsPositive() {
				ccy := w.FeeCcy
				if ccy == "" {
					ccy = w.Ccy
				}
				a.Fee = &model.Amount{Asset: model.CryptoOrFiat(ccy), Qty: fee}
			}
			return emit(a)
		})
}

// ---- Simple Earn interest

func (s *Source) lending(ctx context.Context, cutoff time.Time, emit func(model.Activity) error) error {
	type rec struct {
		Ccy      string `json:"ccy"`
		Earnings string `json:"earnings"`
		Ts       string `json:"ts"`
	}
	return pageByTs(ctx, s.c, "/api/v5/finance/savings/lending-history", cutoff,
		func(r rec) string { return r.Ts }, func(r rec) string { return r.Ccy + ":" + r.Ts },
		func(r rec) error {
			return emit(model.Activity{
				NativeID: "earn:" + strings.ToUpper(r.Ccy) + ":" + r.Ts, Time: msTime(r.Ts), Type: model.Reward,
				Base: model.Amount{Asset: model.CryptoOrFiat(r.Ccy), Qty: dec(r.Earnings)}, Note: "OKX Simple Earn interest",
			})
		})
}
