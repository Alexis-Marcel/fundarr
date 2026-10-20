// Package price values assets on a given day, in a given currency, from
// free public market data: Binance and OKX daily candles for crypto, the
// European Central Bank's reference rates (through Frankfurter) between
// currencies. No account or key is needed. Prices are kept in memory and,
// with a Cache, across restarts: each day is asked once.
//
//	p := price.New(nil, http.DefaultClient) // memory only
//	eur, err := p.Price(ctx, model.CryptoOrFiat("BTC"), day, "EUR")
//
// UnitPrice adapts a Service to what pkg/portfolio expects; any other
// source of prices with that signature works too.
package price

import (
	"context"
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

	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

const day = 24 * time.Hour

// Window is a range of days already asked to a provider for a market.
type Window struct {
	From, To string // YYYY-MM-DD
	Fetched  time.Time
}

// Cache keeps closes between runs. Pairs are a provider and a market
// ("binance:BTCEUR", "fx:USDEUR"); days are YYYY-MM-DD and closes decimal
// strings. Fundarr's store is one.
type Cache interface {
	PriceSeries(ctx context.Context, pair string) (map[string]string, error)
	PriceWindows(ctx context.Context, pair string) ([]Window, error)
	PutPrices(ctx context.Context, pair string, w Window, closes map[string]string) error
}

// Service answers "what was this worth that day". Safe for concurrent use.
type Service struct {
	Cache Cache // nil: memory only
	HTTP  *http.Client
	Now   func() time.Time
	// Provider endpoints, replaceable in tests.
	BinanceURL, OKXURL, FXURL string
	// Market, when set, answers instead of the providers (no network): the
	// closes of pair ("binance:BTCEUR", "fx:USDEUR"…) from from to to. A
	// market it does not know has no closes.
	Market func(pair string, from, to time.Time) map[string]decimal.Decimal

	mu     sync.Mutex
	series map[string]map[string]decimal.Decimal // pair → day → close
	wins   map[string][]Window
}

// New makes a service asking the public providers; cache may be nil.
func New(cache Cache, client *http.Client) *Service {
	if client == nil {
		client = http.DefaultClient
	}
	return &Service{
		Cache:      cache,
		HTTP:       client,
		Now:        func() time.Time { return time.Now().UTC() },
		BinanceURL: "https://data-api.binance.vision",
		OKXURL:     "https://www.okx.com",
		FXURL:      "https://api.frankfurter.dev/v1",
		series:     map[string]map[string]decimal.Decimal{},
		wins:       map[string][]Window{},
	}
}

// Valuer returns a model.Valuer in ccy that never fails: unknown prices
// are reported as not ok.
func (s *Service) Valuer(ctx context.Context, ccy string) model.Valuer {
	return func(a model.Amount, at time.Time) (decimal.Decimal, bool) {
		p, err := s.Price(ctx, a.Asset, at, ccy)
		if err != nil {
			return decimal.Zero, false
		}
		return p.Mul(a.Qty), true
	}
}

// UnitPrice prices one unit of an asset on the day of at, in ccy, never
// failing: ok is false when no price is known. It fits portfolio.Price.
func (s *Service) UnitPrice(ctx context.Context, ccy string) func(a model.Asset, at time.Time) (decimal.Decimal, bool) {
	return func(a model.Asset, at time.Time) (decimal.Decimal, bool) {
		p, err := s.Price(ctx, a, at, ccy)
		return p, err == nil
	}
}

// ErrNoPrice is returned when no provider knows the asset that day.
var ErrNoPrice = errors.New("no price known")

// Price of one unit of asset on the day of at, in ccy (a fiat code).
func (s *Service) Price(ctx context.Context, a model.Asset, at time.Time, ccy string) (decimal.Decimal, error) {
	ccy = strings.ToUpper(ccy)
	sym := a.Symbol
	switch {
	case a.Kind == model.Fiat:
		return s.fx(ctx, sym, ccy, at)
	case model.PeggedTo(a) != "":
		return s.fx(ctx, model.PeggedTo(a), ccy, at)
	case a.Kind != model.Crypto:
		return decimal.Zero, ErrNoPrice
	}
	// Direct market first, then through USDT (≈ USD) and BTC.
	tries := []struct {
		pair string
		then func() (decimal.Decimal, error)
	}{
		{"binance:" + sym + ccy, nil},
		{"binance:" + sym + "USDT", func() (decimal.Decimal, error) { return s.fx(ctx, "USD", ccy, at) }},
		{"binance:" + sym + "USDC", func() (decimal.Decimal, error) { return s.fx(ctx, "USD", ccy, at) }},
		{"okx:" + sym + "-" + ccy, nil},
		{"okx:" + sym + "-USDT", func() (decimal.Decimal, error) { return s.fx(ctx, "USD", ccy, at) }},
		{"binance:" + sym + "BTC", func() (decimal.Decimal, error) { return s.Price(ctx, model.CryptoOrFiat("BTC"), at, ccy) }},
	}
	if sym == "BTC" {
		tries = tries[:5]
	}
	var lastErr error = ErrNoPrice
	for _, t := range tries {
		p, err := s.close(ctx, t.pair, at)
		if err != nil {
			if !errors.Is(err, ErrNoPrice) {
				lastErr = err
			}
			continue
		}
		if t.then == nil {
			return p, nil
		}
		m, err := t.then()
		if err != nil {
			lastErr = err
			continue
		}
		return p.Mul(m), nil
	}
	return decimal.Zero, lastErr
}

// fx converts one unit of from into to, with ECB reference rates (published
// on working days: a weekend takes Friday's).
func (s *Service) fx(ctx context.Context, from, to string, at time.Time) (decimal.Decimal, error) {
	if from == to {
		return decimal.NewFromInt(1), nil
	}
	return s.close(ctx, "fx:"+from+to, at)
}

func dayOf(t time.Time) string { return t.UTC().Format("2006-01-02") }

// close returns a market's close on at's day, or the latest one in the week
// before (weekends, a provider's missing candle).
func (s *Service) close(ctx context.Context, pair string, at time.Time) (decimal.Decimal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.Now()
	if at.After(now) {
		at = now
	}
	if err := s.load(ctx, pair); err != nil {
		return decimal.Zero, err
	}
	look := func() (decimal.Decimal, bool) {
		ser := s.series[pair]
		for i := 0; i <= 7; i++ {
			if p, ok := ser[dayOf(at.Add(-time.Duration(i)*day))]; ok {
				return p, true
			}
		}
		return decimal.Zero, false
	}
	if p, ok := look(); ok && !s.stale(pair, at) {
		return p, nil
	}
	if s.covered(pair, at) {
		if p, ok := look(); ok {
			return p, nil
		}
		return decimal.Zero, ErrNoPrice
	}
	if err := s.fetch(ctx, pair, at); err != nil {
		return decimal.Zero, err
	}
	if p, ok := look(); ok {
		return p, nil
	}
	return decimal.Zero, ErrNoPrice
}

// stale: today's and yesterday's closes are refreshed every few hours.
func (s *Service) stale(pair string, at time.Time) bool {
	if s.Now().Sub(at) > 2*day {
		return false
	}
	for _, w := range s.wins[pair] {
		if w.To >= dayOf(at) && s.Now().Sub(w.Fetched) < 6*time.Hour {
			return false
		}
	}
	return true
}

// covered: the provider was already asked about this day. A recent day is
// asked again after a few hours (its close moves); a market the provider
// did not know, after a month (it may have been listed since).
func (s *Service) covered(pair string, at time.Time) bool {
	d, now := dayOf(at), s.Now()
	for _, w := range s.wins[pair] {
		switch {
		case w.From == unknownFrom:
			if now.Sub(w.Fetched) < 30*day {
				return true
			}
		case w.From <= d && d <= w.To:
			if now.Sub(at) > 2*day || now.Sub(w.Fetched) < 6*time.Hour {
				return true
			}
		}
	}
	return false
}

func (s *Service) load(ctx context.Context, pair string) error {
	if _, ok := s.series[pair]; ok {
		return nil
	}
	ser := map[string]decimal.Decimal{}
	if s.Cache == nil {
		s.series[pair] = ser
		return nil
	}
	raw, err := s.Cache.PriceSeries(ctx, pair)
	if err != nil {
		return err
	}
	for d, p := range raw {
		if v, err := decimal.NewFromString(p); err == nil {
			ser[d] = v
		}
	}
	wins, err := s.Cache.PriceWindows(ctx, pair)
	if err != nil {
		return err
	}
	s.series[pair], s.wins[pair] = ser, wins
	return nil
}

// errUnknownMarket: the provider does not list this market at all.
var errUnknownMarket = errors.New("unknown market")

// unknownFrom marks the window recorded for a market a provider does not list.
const unknownFrom = "0000-00-00"

// fetch asks the provider for a window of days around at and stores it.
func (s *Service) fetch(ctx context.Context, pair string, at time.Time) error {
	provider, market, _ := strings.Cut(pair, ":")
	now := s.Now()
	var from, to time.Time
	var ask func(from, to time.Time) (map[string]decimal.Decimal, error)
	switch provider {
	case "binance":
		// 1000 daily candles per call: most of the window lies before at,
		// where history is.
		from, to = at.Add(-800*day), at.Add(199*day)
		if to.After(now) {
			to, from = now, now.Add(-999*day)
		}
		ask = func(from, to time.Time) (map[string]decimal.Decimal, error) { return s.binance(ctx, market, from, to) }
	case "okx":
		from, to = at.Add(-200*day), at.Add(99*day)
		if to.After(now) {
			to, from = now, now.Add(-299*day)
		}
		ask = func(from, to time.Time) (map[string]decimal.Decimal, error) { return s.okx(ctx, market, from, to) }
	case "fx":
		from, to = at.Add(-300*day), at.Add(64*day)
		if to.After(now) {
			to, from = now, now.Add(-364*day)
		}
		ask = func(from, to time.Time) (map[string]decimal.Decimal, error) {
			return s.frankfurter(ctx, market[:3], market[3:], from, to)
		}
	default:
		return fmt.Errorf("price: unknown provider %q", provider)
	}
	if s.Market != nil {
		ask = func(from, to time.Time) (map[string]decimal.Decimal, error) { return s.Market(pair, from, to), nil }
	}
	closes, err := ask(from, to)
	w := Window{From: dayOf(from), To: dayOf(to), Fetched: now}
	if errors.Is(err, errUnknownMarket) {
		w.From, w.To, closes, err = unknownFrom, "9999-12-31", nil, nil
	}
	if err != nil {
		return err
	}
	raw := map[string]string{}
	for d, p := range closes {
		raw[d] = p.String()
		s.series[pair][d] = p
	}
	if s.Cache != nil {
		if err := s.Cache.PutPrices(ctx, pair, w, raw); err != nil {
			return err
		}
	}
	s.wins[pair] = append(s.wins[pair], w)
	return nil
}

func (s *Service) get(ctx context.Context, u string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("User-Agent", "Fundarr (+https://github.com/Alexis-Marcel/fundarr)")
	var resp *http.Response
	for attempt := 0; ; attempt++ {
		resp, err = s.HTTP.Do(req)
		if err == nil && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
			break
		}
		if err == nil {
			resp.Body.Close()
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if attempt == 2 {
			return 0, fmt.Errorf("price %s: %w", req.URL.Host, err)
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * 3 * time.Second):
		}
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return resp.StatusCode, err
	}
	if err := json.Unmarshal(body, out); err != nil {
		return resp.StatusCode, fmt.Errorf("price %s: HTTP %d, unexpected answer", req.URL.Host, resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// binance: GET /api/v3/klines, public market data (no key, no account).
func (s *Service) binance(ctx context.Context, symbol string, from, to time.Time) (map[string]decimal.Decimal, error) {
	q := url.Values{"symbol": {symbol}, "interval": {"1d"}, "limit": {"1000"},
		"startTime": {strconv.FormatInt(from.Truncate(day).UnixMilli(), 10)}, "endTime": {strconv.FormatInt(to.UnixMilli(), 10)}}
	var raw json.RawMessage
	code, err := s.get(ctx, s.BinanceURL+"/api/v3/klines?"+q.Encode(), &raw)
	if err != nil {
		return nil, err
	}
	if code != 200 {
		var e struct {
			Code int    `json:"code"`
			Msg  string `json:"msg"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Code == -1121 { // Invalid symbol.
			return nil, errUnknownMarket
		}
		return nil, fmt.Errorf("price binance %s: %s (HTTP %d)", symbol, e.Msg, code)
	}
	var rows [][]json.RawMessage
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, err
	}
	out := map[string]decimal.Decimal{}
	for _, r := range rows {
		if len(r) < 5 {
			continue
		}
		var open int64
		var cl string
		if json.Unmarshal(r[0], &open) != nil || json.Unmarshal(r[4], &cl) != nil {
			continue
		}
		if v, err := decimal.NewFromString(cl); err == nil && v.IsPositive() {
			out[dayOf(time.UnixMilli(open))] = v
		}
	}
	return out, nil
}

// okx: GET /api/v5/market/history-candles, 100 days per call, newest first.
func (s *Service) okx(ctx context.Context, inst string, from, to time.Time) (map[string]decimal.Decimal, error) {
	out := map[string]decimal.Decimal{}
	after := to.Add(day).Truncate(day)
	for after.After(from) {
		q := url.Values{"instId": {inst}, "bar": {"1Dutc"}, "limit": {"100"}, "after": {strconv.FormatInt(after.UnixMilli(), 10)}}
		var r struct {
			Code string     `json:"code"`
			Msg  string     `json:"msg"`
			Data [][]string `json:"data"`
		}
		if _, err := s.get(ctx, s.OKXURL+"/api/v5/market/history-candles?"+q.Encode(), &r); err != nil {
			return nil, err
		}
		if r.Code == "51001" { // Instrument ID does not exist.
			return nil, errUnknownMarket
		}
		if r.Code != "0" {
			return nil, fmt.Errorf("price okx %s: %s (code %s)", inst, r.Msg, r.Code)
		}
		if len(r.Data) == 0 {
			break
		}
		oldest := after
		for _, c := range r.Data {
			if len(c) < 5 {
				continue
			}
			ts, err := strconv.ParseInt(c[0], 10, 64)
			if err != nil {
				continue
			}
			t := time.UnixMilli(ts)
			if t.Before(oldest) {
				oldest = t
			}
			if v, err := decimal.NewFromString(c[4]); err == nil && v.IsPositive() {
				out[dayOf(t)] = v
			}
		}
		if !oldest.Before(after) {
			break
		}
		after = oldest
	}
	return out, nil
}

// frankfurter: ECB reference rates, GET /{from}..{to}?from=USD&to=EUR.
func (s *Service) frankfurter(ctx context.Context, base, quote string, from, to time.Time) (map[string]decimal.Decimal, error) {
	var r struct {
		Rates   map[string]map[string]json.Number `json:"rates"`
		Message string                            `json:"message"`
	}
	u := fmt.Sprintf("%s/%s..%s?from=%s&to=%s", s.FXURL, dayOf(from), dayOf(to), url.QueryEscape(base), url.QueryEscape(quote))
	code, err := s.get(ctx, u, &r)
	if err != nil {
		return nil, err
	}
	if code == 404 || code == 422 {
		return nil, errUnknownMarket
	}
	if code != 200 {
		return nil, fmt.Errorf("price frankfurter %s/%s: %s (HTTP %d)", base, quote, r.Message, code)
	}
	out := map[string]decimal.Decimal{}
	days := make([]string, 0, len(r.Rates))
	for d := range r.Rates {
		days = append(days, d)
	}
	sort.Strings(days)
	for _, d := range days {
		if v, err := decimal.NewFromString(r.Rates[d][quote].String()); err == nil && v.IsPositive() {
			out[d] = v
		}
	}
	return out, nil
}
