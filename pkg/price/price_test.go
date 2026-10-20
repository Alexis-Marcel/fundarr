package price

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
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
