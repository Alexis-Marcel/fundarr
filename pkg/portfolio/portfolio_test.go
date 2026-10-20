package portfolio

import (
	"fmt"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

func date(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// prices in EUR, each valid from its day on; XYZ has none.
var prices = map[string][]struct {
	from string
	p    string
}{
	"BTC":  {{"2024-01-01", "20000"}, {"2024-03-01", "30000"}, {"2025-01-01", "40000"}, {"2025-06-01", "60000"}},
	"ETH":  {{"2024-01-01", "1500"}, {"2024-02-01", "2000"}, {"2024-03-01", "1500"}, {"2024-06-01", "3000"}, {"2025-01-01", "2000"}, {"2025-06-01", "3000"}},
	"USDT": {{"2020-01-01", "0.9"}},
	"EUR":  {{"2000-01-01", "1"}},
}

var asked int

func fake(a model.Asset, at time.Time) (decimal.Decimal, bool) {
	asked++
	var p string
	for _, e := range prices[a.Symbol] {
		if !at.Before(date(e.from)) {
			p = e.p
		}
	}
	if p == "" {
		return decimal.Zero, false
	}
	return decimal.RequireFromString(p), true
}

func amt(sym, q string) *model.Amount {
	a := model.Amt(model.CryptoOrFiat(sym), q)
	return &a
}

func act(id, src, at string, typ model.Type, side model.Side, base, quote, fee *model.Amount) model.Activity {
	t, err := time.Parse(time.RFC3339, at)
	if err != nil {
		t = date(at).Add(12 * time.Hour)
	}
	return model.Activity{ID: id, SourceID: src, NativeID: id, Time: t, Type: typ, Side: side, Base: *base, Quote: quote, Fee: fee}
}

// history: buys, a deposit from outside, a swap, rewards (one unpriced),
// two sales for euros in 2025 (one of a stablecoin) and a move between two
// of your own sources.
func history() ([]model.Activity, map[string]string) {
	acts := []model.Activity{
		act("b1", "binance", "2024-01-10", model.Trade, model.Buy, amt("BTC", "1"), amt("EUR", "20000"), amt("EUR", "20")),
		act("d1", "binance", "2024-02-01", model.Deposit, "", amt("ETH", "1"), nil, nil),
		act("s1", "binance", "2024-03-01", model.Trade, model.Buy, amt("ETH", "10"), amt("BTC", "0.5"), nil),
		act("r1", "binance", "2024-06-01", model.Reward, "", amt("ETH", "1"), nil, nil),
		act("x1", "binance", "2024-07-01", model.Reward, "", amt("XYZ", "100"), nil, nil),
		act("sell1", "binance", "2025-02-01", model.Trade, model.Sell, amt("BTC", "0.25"), amt("EUR", "10000"), amt("EUR", "10")),
		act("sw", "binance", "2025-03-01", model.Trade, model.Sell, amt("ETH", "5"), amt("USDT", "10000"), nil),
		act("wd", "binance", "2025-04-01T10:00:00Z", model.Withdrawal, "", amt("BTC", "0.25"), nil, nil),
		act("dep", "ledger", "2025-04-01T11:00:00Z", model.Deposit, "", amt("BTC", "0.25"), nil, nil),
		act("sell2", "binance", "2025-07-01", model.Trade, model.Sell, amt("USDT", "5000"), amt("EUR", "4500"), amt("USDT", "5")),
		// Fiat alone is not part of the portfolio.
		act("f1", "binance", "2024-01-05", model.Deposit, "", amt("EUR", "50000"), nil, nil),
	}
	return acts, map[string]string{"dep": "wd"}
}

func eq(t *testing.T, what string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(decimal.RequireFromString(want)) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

func TestNow(t *testing.T) {
	acts, in := history()
	s := Now(acts, in, fake, date("2025-08-01"))
	// Held: 0.25 BTC on the ledger (15000), 7 ETH (21000), 4995 USDT (4495.5).
	eq(t, "value", s.Value, "40495.5")
	// 20000 + 20 fee + 2000 deposited − (10000 − 10 fee) − 4500.
	eq(t, "invested", s.Invested, "7530")
	// BTC 5000 (half of the half left), ETH 8750 (7/12 of 15000), USDT at value.
	eq(t, "cost", s.Cost, "18245.5")
	eq(t, "unrealized", s.Unrealized, "22250")
	// −20 fee + (9990 − 5000) + (9000 − 6250) + (4500 − 4500) − 4.5 fee.
	eq(t, "realized", s.Realized, "7715.5")
	eq(t, "rewards", s.Rewards, "3000")
	// Everything adds up.
	eq(t, "value − invested", s.Value.Sub(s.Invested), s.Unrealized.Add(s.Realized).Add(s.Rewards).String())

	if len(s.Assets) != 3 || s.Assets[0].Asset.Symbol != "ETH" || s.Assets[1].Asset.Symbol != "BTC" || s.Assets[2].Asset.Symbol != "USDT" {
		t.Fatalf("assets %+v", s.Assets)
	}
	btc := s.Assets[1]
	eq(t, "BTC qty", btc.Qty, "0.25")
	eq(t, "BTC price", btc.Price, "60000")
	eq(t, "BTC unrealized", btc.Unrealized, "10000")
	if len(btc.Sources) != 1 || btc.Sources[0].Source != "ledger" {
		t.Errorf("BTC sources %+v", btc.Sources)
	}
	if got := s.Assets[0].Share.StringFixed(3); got != "0.519" {
		t.Errorf("ETH share %s", got)
	}
	if len(s.Sources) != 2 || s.Sources[0].Source != "binance" {
		t.Fatalf("sources %+v", s.Sources)
	}
	eq(t, "binance", s.Sources[0].Amount, "25495.5")
	if len(s.Unpriced) != 1 || s.Unpriced[0].Symbol != "XYZ" {
		t.Errorf("unpriced %+v", s.Unpriced)
	}
	// Earlier, only what had happened by then.
	if s := Now(acts, in, fake, date("2024-01-20")); !s.Value.Equal(decimal.NewFromInt(20000)) || !s.Invested.Equal(decimal.NewFromInt(20020)) {
		t.Errorf("January %s %s", s.Value, s.Invested)
	}
}

func TestDays(t *testing.T) {
	str := func(ds []time.Time) string {
		out := ""
		for _, d := range ds {
			out += d.Format(time.DateOnly) + " "
		}
		return out
	}
	if got := str(Days(date("2024-01-03"), date("2024-01-20"), "week")); got != "2024-01-07 2024-01-14 2024-01-20 " {
		t.Errorf("weeks %s", got)
	}
	if got := str(Days(date("2024-01-15"), date("2024-03-10"), "month")); got != "2024-01-31 2024-02-29 2024-03-10 " {
		t.Errorf("months %s", got)
	}
	if got := len(Days(date("2024-01-01"), date("2024-01-01"), "day")); got != 1 {
		t.Errorf("one day: %d", got)
	}
	if Days(date("2010-01-01"), date("2020-02-01"), "day") == nil || Days(date("2010-01-01"), date("2020-03-15"), "day") != nil {
		t.Error("days are capped just above ten years")
	}
}

func TestHistory(t *testing.T) {
	acts, in := history()
	if f := First(acts); !f.Equal(date("2024-01-10")) {
		t.Fatalf("first %s", f)
	}
	points, unpriced := History(acts, in, fake, Days(date("2024-01-09"), date("2025-08-01"), "month"))
	if len(points) != 20 || len(unpriced) != 1 {
		t.Fatalf("%d points, unpriced %v", len(points), unpriced)
	}
	eq(t, "January value", points[0].Value, "20000")
	eq(t, "January invested", points[0].Invested, "20020")
	// End of February 2024: 1 BTC (20000) and 1 ETH (2000).
	eq(t, "February value", points[1].Value, "22000")
	eq(t, "February invested", points[1].Invested, "22020")
	last := points[len(points)-1]
	if last.Day.Format(time.DateOnly) != "2025-08-01" {
		t.Fatalf("last %s", last.Day)
	}
	eq(t, "last value", last.Value, "40495.5")
	eq(t, "last invested", last.Invested, "7530")
	// A move between your own sources changes nothing.
	days := Days(date("2025-03-31"), date("2025-04-02"), "day")
	points, _ = History(acts, in, fake, days)
	if !points[0].Value.Equal(points[2].Value) || !points[0].Invested.Equal(points[2].Invested) {
		t.Errorf("transfer moved the portfolio %+v", points)
	}
}

// The worked 2086 example, every line by hand.
//
// Acquisition: 20000 € + 20 € fee for 1 BTC = 20020 (the ETH deposited and
// the rewards count for 0; the BTC → ETH swap is not taxable).
//
// 1st disposal, 2025-02-01: 0.25 BTC for 10000 €, 10 € fee.
// Portfolio: 0.5 BTC × 40000 + 12 ETH × 2000 (+ 100 XYZ, no price) = 44000.
// Capital share: 20020 × 10000 / 44000 = 4550. Gain: 10000 − 10 − 4550 = 5440.
//
// ETH → USDT (2025-03-01) and the move to the Ledger: not taxable.
//
// 2nd disposal, 2025-07-01: 5000 USDT for 4500 €, fee 5 USDT (4.50 €).
// Portfolio: 0.25 BTC × 60000 + 7 ETH × 3000 + 10000 USDT × 0.9 = 45000.
// Acquisition: 20020 − 4550 = 15470. Capital share: 15470 × 4500 / 45000 = 1547.
// Gain: 4500 − 4.50 − 1547 = 2948.50.
//
// Year: disposals 14500, gain 8388.50, tax 8388.50 × 31.4 % = 2633.99.
func TestTaxFR(t *testing.T) {
	acts, in := history()
	tx := TaxFR(acts, in, fake, 2025, false)
	if len(tx.Disposals) != 2 {
		t.Fatalf("disposals %+v", tx.Disposals)
	}
	d := tx.Disposals[0]
	if d.Source != "binance" || d.Sold.Asset.Symbol != "BTC" || d.Sold.Qty.String() != "0.25" {
		t.Errorf("first %+v", d)
	}
	eq(t, "1 price", d.Price, "10000")
	eq(t, "1 fees", d.Fees, "10")
	eq(t, "1 portfolio", d.Portfolio, "44000")
	eq(t, "1 acquisition", d.Acquisition, "20020")
	eq(t, "1 capital share", d.CapitalShare, "4550")
	eq(t, "1 gain", d.Gain, "5440")
	d = tx.Disposals[1]
	if d.Sold.Asset.Symbol != "USDT" {
		t.Errorf("second %+v", d)
	}
	eq(t, "2 price", d.Price, "4500")
	eq(t, "2 fees", d.Fees, "4.5")
	eq(t, "2 portfolio", d.Portfolio, "45000")
	eq(t, "2 acquisition", d.Acquisition, "15470")
	eq(t, "2 capital share", d.CapitalShare, "1547")
	eq(t, "2 gain", d.Gain, "2948.5")
	eq(t, "total", tx.Total, "14500")
	eq(t, "gain", tx.Gain, "8388.5")
	eq(t, "rate", tx.Rate, "0.314")
	eq(t, "tax", tx.Tax, "2633.99")
	if tx.Exempt {
		t.Error("not exempt")
	}
	if last := tx.Notes[len(tx.Notes)-1]; last != "No price for XYZ: counted at 0." {
		t.Errorf("notes %q", tx.Notes)
	}

	// Taxed as income, the deposit (2000) and the ETH reward (3000) join the
	// acquisition: 25020 × 10000 / 44000 = 5686.36.
	tx = TaxFR(acts, in, fake, 2025, true)
	eq(t, "income acquisition", tx.Disposals[0].Acquisition, "25020")
	eq(t, "income capital share", tx.Disposals[0].CapitalShare, "5686.36")
	eq(t, "income gain", tx.Disposals[0].Gain, "4303.64")

	// 2024: nothing sold for money.
	tx = TaxFR(acts, in, fake, 2024, false)
	if len(tx.Disposals) != 0 || !tx.Exempt || !tx.Tax.IsZero() || !tx.Rate.Equal(decimal.RequireFromString("0.3")) {
		t.Errorf("2024 %+v", tx)
	}
}

func TestTaxFRThresholdAndLaterYear(t *testing.T) {
	acts := []model.Activity{
		act("b", "s", "2025-01-10", model.Trade, model.Buy, amt("BTC", "1"), amt("EUR", "20000"), nil),
		act("s1", "s", "2025-06-10", model.Trade, model.Sell, amt("BTC", "0.005"), amt("EUR", "300"), nil),
		act("s2", "s", "2026-02-10", model.Trade, model.Sell, amt("BTC", "0.1"), amt("EUR", "4000"), nil),
	}
	tx := TaxFR(acts, nil, fake, 2025, false)
	if !tx.Exempt || !tx.Tax.IsZero() || !tx.Gain.IsPositive() {
		t.Fatalf("300 € in the year: exempt, gain still shown %+v", tx)
	}
	// The exempt sale still used its fraction of capital: 20000 × 300 / 60000
	// (1 BTC at 60000) = 100, so 19900 remain.
	tx = TaxFR(acts, nil, fake, 2026, false)
	eq(t, "acquisition after", tx.Disposals[0].Acquisition, "19900")
	// Portfolio 0.995 × 60000 = 59700: 19900 × 4000 / 59700 = 1333.33.
	eq(t, "capital share", tx.Disposals[0].CapitalShare, "1333.33")
	eq(t, "gain", tx.Disposals[0].Gain, "2666.67")
}

// A few thousand activities over ten years answer quickly, and each asset
// and day is priced once.
func TestLargeHistory(t *testing.T) {
	var acts []model.Activity
	start := date("2016-01-01")
	for i := range 5000 {
		at := start.Add(time.Duration(i) * 17 * time.Hour).Format(time.RFC3339)
		id := fmt.Sprint(i)
		switch i % 4 {
		case 0:
			acts = append(acts, act(id, "a", at, model.Trade, model.Buy, amt("BTC", "0.01"), amt("EUR", "300"), amt("EUR", "1")))
		case 1:
			acts = append(acts, act(id, "a", at, model.Trade, model.Buy, amt("ETH", "0.1"), amt("BTC", "0.005"), nil))
		case 2:
			acts = append(acts, act(id, "b", at, model.Reward, "", amt("ETH", "0.001"), nil, nil))
		default:
			acts = append(acts, act(id, "a", at, model.Trade, model.Sell, amt("ETH", "0.05"), amt("EUR", "100"), nil))
		}
	}
	asked = 0
	end := date("2025-12-31")
	points, _ := History(acts, nil, fake, Days(start, end, "day"))
	if len(points) != 3653 {
		t.Fatalf("%d points", len(points))
	}
	if asked > 3*len(points) {
		t.Errorf("asked %d prices for %d days", asked, len(points))
	}
	Now(acts, nil, fake, end)
	if tx := TaxFR(acts, nil, fake, 2025, false); len(tx.Disposals) == 0 {
		t.Error("no disposals")
	}
}

// Securities count like crypto in the portfolio (not in the 2086); one
// without a market price is valued at its last trade.
func TestSecurities(t *testing.T) {
	etf := model.Asset{Kind: model.Security, Symbol: "IE00B4L5Y983", ISIN: "IE00B4L5Y983"}
	priv := model.Asset{Kind: model.Security, Symbol: "LU3170240538", ISIN: "LU3170240538"}
	sec := func(a model.Asset, q string) *model.Amount { x := model.Amt(a, q); return &x }
	acts := []model.Activity{
		act("b1", "tr", "2024-01-10", model.Trade, model.Buy, sec(etf, "10"), amt("EUR", "800"), amt("EUR", "1")),
		act("s1", "tr", "2024-04-10", model.Trade, model.Sell, sec(etf, "4"), amt("EUR", "400"), nil),
		act("split", "tr", "2024-05-01", model.Trade, model.Buy, sec(etf, "6"), amt("EUR", "0"), nil),
		act("p1", "tr", "2024-05-02", model.Trade, model.Buy, sec(priv, "100"), amt("EUR", "100"), nil),
		act("btc", "x", "2024-01-10", model.Trade, model.Buy, amt("BTC", "0.01"), amt("EUR", "200"), nil),
	}
	price := func(a model.Asset, at time.Time) (decimal.Decimal, bool) {
		if a.Key() == etf.Key() {
			return decimal.NewFromInt(50), true // after the split
		}
		return fake(a, at)
	}
	s := Now(acts, nil, WithTrades(acts, price, "EUR"), date("2025-06-02"))
	// 12 ETF × 50 + 100 private × 1 (its buy) + 0.01 BTC × 60000.
	if !s.Value.Equal(decimal.NewFromInt(1300)) || len(s.Unpriced) != 0 {
		t.Fatalf("value %s, unpriced %v", s.Value, s.Unpriced)
	}
	// Invested: 801 + 100 + 200 − 400.
	if !s.Invested.Equal(decimal.NewFromInt(701)) {
		t.Fatalf("invested %s", s.Invested)
	}
	// Sold 4 of 10 bought for 800: cost 320, the buy's fee apart; the
	// split costs nothing.
	if !s.Realized.Equal(decimal.NewFromInt(79)) {
		t.Fatalf("realized %s", s.Realized)
	}
	for _, h := range s.Assets {
		if h.Asset.Key() == etf.Key() && (!h.Qty.Equal(decimal.NewFromInt(12)) || !h.Cost.Equal(decimal.NewFromInt(480))) {
			t.Fatalf("ETF %+v", h)
		}
	}
	if First(acts) != date("2024-01-10") {
		t.Fatalf("first %s", First(acts))
	}
	// The 2086 leaves securities out.
	if tax := TaxFR(acts, nil, price, 2024, false); len(tax.Disposals) != 0 {
		t.Fatalf("2086 %+v", tax.Disposals)
	}
}
