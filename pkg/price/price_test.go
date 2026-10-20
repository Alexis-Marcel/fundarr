package price

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

// memCache is a Cache in memory, standing for a database.
type memCache struct {
	mu     sync.Mutex
	closes map[string]map[string]string
	wins   map[string][]Window
}

func newMemCache() *memCache {
	return &memCache{closes: map[string]map[string]string{}, wins: map[string][]Window{}}
}

func (c *memCache) PriceSeries(_ context.Context, pair string) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := map[string]string{}
	for d, p := range c.closes[pair] {
		out[d] = p
	}
	return out, nil
}

func (c *memCache) PriceWindows(_ context.Context, pair string) ([]Window, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Window(nil), c.wins[pair]...), nil
}

func (c *memCache) PutPrices(_ context.Context, pair string, w Window, closes map[string]string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closes[pair] == nil {
		c.closes[pair] = map[string]string{}
	}
	for d, p := range closes {
		c.closes[pair][d] = p
	}
	c.wins[pair] = append(c.wins[pair], w)
	return nil
}

func TestPrices(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		q := r.URL.Query()
		switch r.URL.Path {
		case "/api/v3/klines":
			start, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
			end, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
			var px float64
			switch q.Get("symbol") {
			case "ETHUSDT":
				px = 2000
			case "BTCEUR":
				px = 50000
			default:
				w.WriteHeader(400)
				w.Write([]byte(`{"code":-1121,"msg":"Invalid symbol."}`))
				return
			}
			var rows [][]any
			for ts := start; ts <= end && len(rows) < 1000; ts += int64(day / time.Millisecond) {
				rows = append(rows, []any{ts, "0", "0", "0", fmt.Sprint(px), "0"})
			}
			json.NewEncoder(w).Encode(rows)
		case "/api/v5/market/history-candles":
			w.Write([]byte(`{"code":"51001","msg":"Instrument ID doesn't exist.","data":[]}`))
		default: // frankfurter: USD→EUR on working days only
			rates := map[string]map[string]float64{}
			for d := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC); d.Before(time.Date(2024, 12, 31, 0, 0, 0, 0, time.UTC)); d = d.Add(day) {
				if d.Weekday() != time.Saturday && d.Weekday() != time.Sunday {
					rates[d.Format("2006-01-02")] = map[string]float64{"EUR": 0.9}
				}
			}
			json.NewEncoder(w).Encode(map[string]any{"rates": rates})
		}
	}))
	defer srv.Close()
	st := newMemCache()
	mk := func() *Service {
		s := New(st, srv.Client())
		s.BinanceURL, s.OKXURL, s.FXURL = srv.URL, srv.URL, srv.URL+"/fx"
		s.Now = func() time.Time { return time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC) }
		return s
	}
	s := mk()
	ctx := context.Background()
	sat := time.Date(2024, 6, 8, 15, 0, 0, 0, time.UTC) // a Saturday: Friday's rate
	eq := func(what string, a model.Asset, ccy, want string) {
		t.Helper()
		p, err := s.Price(ctx, a, sat, ccy)
		if err != nil || p.String() != want {
			t.Fatalf("%s: %s %v, want %s", what, p, err, want)
		}
	}
	eq("direct", model.CryptoOrFiat("BTC"), "EUR", "50000")
	eq("through USDT and the ECB rate", model.CryptoOrFiat("ETH"), "EUR", "1800")
	eq("stablecoin", model.CryptoOrFiat("USDC"), "EUR", "0.9")
	eq("same currency", model.CryptoOrFiat("EUR"), "EUR", "1")
	if _, err := s.Price(ctx, model.CryptoOrFiat("NOPE"), sat, "EUR"); err == nil {
		t.Fatal("an unlisted coin has no price")
	}
	n := calls.Load()
	// Everything is now in the cache: a new service asks nothing.
	s = mk()
	eq("cached", model.CryptoOrFiat("ETH"), "EUR", "1800")
	if _, err := s.Price(ctx, model.CryptoOrFiat("NOPE"), sat, "EUR"); err == nil || calls.Load() != n {
		t.Fatalf("calls %d → %d", n, calls.Load())
	}
	v := s.Valuer(ctx, "EUR")
	if got, ok := v(model.Amt(model.CryptoOrFiat("ETH"), "0.5"), sat); !ok || got.String() != "900" {
		t.Fatalf("valuer %s %v", got, ok)
	}
}

// FUNDARR_LIVE_PRICES=1 asks the real providers.
func TestLivePrices(t *testing.T) {
	if os.Getenv("FUNDARR_LIVE_PRICES") == "" {
		t.Skip("set FUNDARR_LIVE_PRICES=1")
	}
	s := New(nil, &http.Client{Timeout: 30 * time.Second})
	at := time.Date(2022, 3, 10, 12, 0, 0, 0, time.UTC)
	for _, sym := range []string{"BTC", "ETH", "MATIC", "POL", "LUNC", "USDC", "EUR", "ARB"} {
		p, err := s.Price(context.Background(), model.CryptoOrFiat(sym), at, "EUR")
		t.Logf("%s on %s: %s EUR (%v)", sym, at.Format("2006-01-02"), p.StringFixed(4), err)
	}
}

// Stocks and ETFs by ISIN: justETF, then a euro listing on Yahoo.
func TestSecurities(t *testing.T) {
	var mu sync.Mutex
	asked := map[string]int{}
	yahooDown := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		asked[r.URL.Path]++
		if !strings.Contains(r.Header.Get("User-Agent"), "Chrome/") {
			w.WriteHeader(403)
			return
		}
		q := r.URL.Query()
		switch {
		case strings.HasPrefix(r.URL.Path, "/api/etfs/"):
			isin := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/api/etfs/"), "/performance-chart")
			if isin != "IE00B4L5Y983" {
				w.WriteHeader(404)
				return
			}
			px := map[string]float64{"EUR": 100, "USD": 110, "GBP": 85}[q.Get("currency")]
			type val struct {
				Raw json.Number `json:"raw"`
			}
			type pt struct {
				Date  string `json:"date"`
				Value val    `json:"value"`
			}
			var series []pt
			from, _ := time.Parse(time.DateOnly, q.Get("dateFrom"))
			to, _ := time.Parse(time.DateOnly, q.Get("dateTo"))
			for d := from; !d.After(to); d = d.Add(day) {
				series = append(series, pt{d.Format(time.DateOnly), val{json.Number(fmt.Sprint(px))}})
			}
			json.NewEncoder(w).Encode(map[string]any{"series": series})
		case strings.HasPrefix(r.URL.Path, "/y1/"):
			w.WriteHeader(429) // the first Yahoo host refuses
		case r.URL.Path == "/y2/v1/finance/search":
			if yahooDown {
				w.WriteHeader(500)
				return
			}
			if q.Get("q") != "FR0000120271" {
				json.NewEncoder(w).Encode(map[string]any{"quotes": []any{}})
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"quotes": []map[string]string{{"symbol": "TTE", "quoteType": "EQUITY"}, {"symbol": "TTE.PA", "quoteType": "EQUITY"}}})
		case r.URL.Path == "/y2/v8/finance/chart/TTE.PA":
			p1, _ := strconv.ParseInt(q.Get("period1"), 10, 64)
			p2, _ := strconv.ParseInt(q.Get("period2"), 10, 64)
			var ts []int64
			var cl []float64
			for x := p1 - p1%86400 + 7*3600; x < p2; x += 86400 {
				ts, cl = append(ts, x), append(cl, 60.5)
			}
			json.NewEncoder(w).Encode(map[string]any{"chart": map[string]any{"result": []any{map[string]any{
				"meta": map[string]any{"currency": "EUR", "gmtoffset": 7200}, "timestamp": ts,
				"indicators": map[string]any{"quote": []any{map[string]any{"close": cl}}}}}}})
		case strings.HasPrefix(r.URL.Path, "/fx"):
			json.NewEncoder(w).Encode(map[string]any{"rates": map[string]map[string]float64{"2024-06-07": {"SEK": 11.5}}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	s := New(newMemCache(), srv.Client())
	s.JustETFURL, s.YahooURLs, s.FXURL = srv.URL, []string{srv.URL + "/y1", srv.URL + "/y2"}, srv.URL+"/fx"
	s.Now = func() time.Time { return time.Date(2024, 12, 1, 0, 0, 0, 0, time.UTC) }
	ctx := context.Background()
	at := time.Date(2024, 6, 7, 15, 0, 0, 0, time.UTC)
	etf := model.Asset{Kind: model.Security, Symbol: "IE00B4L5Y983", ISIN: "IE00B4L5Y983"}
	for ccy, want := range map[string]string{"EUR": "100", "USD": "110", "GBP": "85", "SEK": "1150"} {
		if p, err := s.Price(ctx, etf, at, ccy); err != nil || p.String() != want {
			t.Errorf("ETF in %s: %s %v", ccy, p, err)
		}
	}
	// Not on justETF, Yahoo down: no price (and asked again later).
	tte := model.Asset{Kind: model.Security, Symbol: "FR0000120271", ISIN: "FR0000120271"}
	if _, err := s.Price(ctx, tte, at, "EUR"); err == nil {
		t.Fatal("a price with Yahoo down")
	}
	mu.Lock()
	yahooDown = false
	mu.Unlock()
	s.Now = func() time.Time { return time.Date(2024, 12, 1, 7, 0, 0, 0, time.UTC) }
	if p, err := s.Price(ctx, tte, at, "EUR"); err != nil || p.String() != "60.5" {
		t.Fatalf("from Yahoo's euro listing: %s %v", p, err)
	}
	// Unknown everywhere, and not an ISIN at all.
	if _, err := s.Price(ctx, model.Asset{Kind: model.Security, ISIN: "XS0000000009"}, at, "EUR"); !errors.Is(err, ErrNoPrice) {
		t.Fatalf("unknown ISIN: %v", err)
	}
	if _, err := s.Price(ctx, model.Asset{Kind: model.Security, Symbol: "AAPL"}, at, "EUR"); !errors.Is(err, ErrNoPrice) {
		t.Fatalf("no ISIN: %v", err)
	}
	// Each market asked once; the second day comes from what was fetched.
	n := asked["/api/etfs/IE00B4L5Y983/performance-chart"]
	if _, err := s.Price(ctx, etf, at.Add(-30*day), "EUR"); err != nil || asked["/api/etfs/IE00B4L5Y983/performance-chart"] != n {
		t.Fatalf("asked again: %v", err)
	}
}
