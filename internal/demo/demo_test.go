package demo

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/Alexis-Marcel/fundarr/pkg/portfolio"
	"github.com/Alexis-Marcel/fundarr/pkg/price"
	"github.com/shopspring/decimal"
)

var now = time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

func TestPricesAreFixed(t *testing.T) {
	at := time.Date(2024, 3, 12, 0, 0, 0, 0, time.UTC)
	if p, ok := Price("BTC", at); !ok || !p.Equal(decimal.NewFromInt(64000)) {
		t.Fatalf("BTC on a waypoint %s %v", p, ok)
	}
	if _, ok := Price("BTC", first.Add(-day)); ok {
		t.Fatal("a price before the first day")
	}
	a := Market("binance:ETHUSDT", at.Add(-10*day), at)
	if len(a) != 11 || !reflect.DeepEqual(a, Market("binance:ETHUSDT", at.Add(-10*day), at)) {
		t.Fatalf("market %v", a)
	}
	if Market("binance:DOGEEUR", at, at) != nil || Market("fx:GBPEUR", at, at) != nil {
		t.Fatal("unknown markets have no closes")
	}
	if !reflect.DeepEqual(Activities(now), Activities(now)) {
		t.Fatal("the history changes between runs")
	}
}

// TestSeed: the engine sees what a real Fundarr would after syncing three
// clean sources, and computes the figures from it.
func TestSeed(t *testing.T) {
	ctx := context.Background()
	st, err := Open(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eng := engine.New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	eng.Prices = price.New(st, nil) // no client: any request would panic
	eng.Prices.Market = Market
	eng.Prices.Now = func() time.Time { return now }

	srcs, _ := st.Sources(ctx)
	if len(srcs) != 4 {
		t.Fatalf("%d sources", len(srcs))
	}
	for _, s := range srcs {
		if s.LastOK.IsZero() || s.LastErr != "" {
			t.Fatalf("%s not synced", s.Name)
		}
		if gaps, err := eng.Reconcile(ctx, s.ID); err != nil || len(gaps) > 0 {
			t.Fatalf("%s: history and balances differ %v %v", s.Name, gaps, err)
		}
	}
	l, err := eng.Ledger(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Every move to the Ledger is paired with its deposit.
	moves := 0
	for _, a := range l.Acts {
		if a.Type == model.Withdrawal && a.Base.Asset.Kind == model.Crypto {
			moves++
			if l.Out[a.ID] == "" {
				t.Fatalf("unpaired move %+v", a)
			}
		}
	}
	if moves < 6 || len(l.Pairs) != moves {
		t.Fatalf("%d moves, %d pairs", moves, len(l.Pairs))
	}

	unit := eng.UnitPrice(ctx, "EUR")
	p := portfolio.Now(l.Acts, l.In, unit, now)
	held := map[string]bool{}
	for _, h := range p.Assets {
		held[h.Asset.Symbol] = true
	}
	for _, sym := range []string{"BTC", "ETH", "SOL", "USDC", "IE00B4L5Y983", "LU1681043599", "FR0000120271"} {
		if !held[sym] {
			t.Fatalf("%s not held: %+v", sym, p.Assets)
		}
	}
	if len(p.Unpriced) > 0 || !p.Rewards.IsPositive() || !p.Realized.IsPositive() || !p.Value.GreaterThan(p.Invested) {
		t.Fatalf("summary %+v", p)
	}
	// value − invested = unrealized + realized + rewards, but for USDC's
	// moves against the euro.
	diff := p.Value.Sub(p.Invested).Sub(p.Unrealized.Add(p.Realized).Add(p.Rewards)).Abs()
	if diff.GreaterThan(decimal.NewFromInt(100)) {
		t.Fatalf("figures do not add up by %s: %+v", diff, p)
	}

	// 2023 has one small sale (exempt); later years are taxed.
	if tx := portfolio.TaxFR(l.Acts, l.In, unit, 2023, false); len(tx.Disposals) != 1 || !tx.Exempt {
		t.Fatalf("2023 %+v", tx)
	}
	for _, y := range []int{2024, 2025} {
		if tx := portfolio.TaxFR(l.Acts, l.In, unit, y, false); len(tx.Disposals) < 2 || tx.Exempt || !tx.Gain.IsPositive() {
			t.Fatalf("%d %+v", y, tx)
		}
	}
	days := portfolio.Days(portfolio.First(l.Acts), now.Truncate(day), "month")
	points, unpriced := portfolio.History(l.Acts, l.In, unit, days)
	if len(points) < 40 || len(unpriced) > 0 || !points[len(points)-1].Value.Equal(p.Value) {
		t.Fatalf("history %d points, last %+v, now %s", len(points), points[len(points)-1], p.Value)
	}
}
