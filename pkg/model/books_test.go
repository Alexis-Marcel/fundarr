package model

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

var t0 = time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC)

func mk(id, src string, typ Type, sym, q string, at time.Time) Activity {
	return Activity{ID: id, SourceID: src, NativeID: id, Time: at, Type: typ, Base: Amt(CryptoOrFiat(sym), q)}
}

func TestPairTransfers(t *testing.T) {
	out1 := mk("o1", "binance", Withdrawal, "BTC", "0.5", t0)
	out1.TxID, out1.Fee = "ABCDEF0123456789ABCDEF", &Amount{Asset: CryptoOrFiat("BTC"), Qty: decimal.RequireFromString("0.0001")}
	in1 := mk("i1", "ledger", Deposit, "BTC", "0.5", t0.Add(40*time.Minute))
	in1.TxID = "abcdef0123456789abcdef"
	// Same asset and amount, no hash on one side, a day later.
	out2 := mk("o2", "okx", Withdrawal, "ETH", "1", t0)
	in2 := mk("i2", "binance", Deposit, "ETH", "0.999", t0.Add(24*time.Hour))
	decoy := mk("i3", "binance", Deposit, "ETH", "0.999", t0.Add(60*time.Hour)) // further away
	// Too late, wrong amount, same source, different hashes.
	out3 := mk("o3", "okx", Withdrawal, "SOL", "10", t0)
	late := mk("i4", "ledger", Deposit, "SOL", "10", t0.Add(100*time.Hour))
	out4 := mk("o4", "okx", Withdrawal, "ADA", "10", t0)
	small := mk("i5", "ledger", Deposit, "ADA", "5", t0.Add(time.Hour))
	same := mk("i6", "okx", Deposit, "ADA", "10", t0.Add(time.Hour))
	out5 := mk("o5", "okx", Withdrawal, "XRP", "10", t0)
	out5.TxID = "1111111111111111111111"
	in5 := mk("i7", "ledger", Deposit, "XRP", "10", t0.Add(time.Hour))
	in5.TxID = "2222222222222222222222"
	acts := []Activity{out1, in1, out2, in2, decoy, out3, late, out4, small, same, out5, in5}

	pairs := PairTransfers(acts, nil, nil)
	got := map[string]string{}
	for _, p := range pairs {
		got[p.OutID] = p.InID + "/" + p.By
	}
	if got["o1"] != "i1/txid" || got["o2"] != "i2/amount" || len(got) != 2 {
		t.Fatalf("pairs %v", got)
	}
	// The user refuses one and pairs the late one by hand.
	pairs = PairTransfers(acts, []Pair{{OutID: "o3", InID: "i4"}}, map[string]bool{"o2\x00i2": true})
	got = map[string]string{}
	for _, p := range pairs {
		got[p.OutID] = p.InID + "/" + p.By
	}
	if got["o3"] != "i4/user" || got["o2"] != "i3/amount" {
		t.Fatalf("with choices %v", got)
	}
}

func TestCosts(t *testing.T) {
	eur := func(s string) Amount { return Amt(CryptoOrFiat("EUR"), s) }
	prices := map[string]string{"BTC": "40000", "ETH": "2000", "BNB": "300", "USDT": "0.9"}
	value := func(a Amount, _ time.Time) (decimal.Decimal, bool) {
		if a.Asset.Symbol == "EUR" {
			return a.Qty, true
		}
		p, ok := prices[a.Asset.Symbol]
		if !ok {
			return decimal.Zero, false
		}
		return a.Qty.Mul(decimal.RequireFromString(p)), true
	}
	buy := mk("b1", "binance", Trade, "BTC", "1", t0)
	buy.Side, buy.Quote = Buy, &[]Amount{eur("30000")}[0]
	buy2 := mk("b2", "binance", Trade, "BTC", "1", t0.Add(time.Hour))
	buy2.Side, buy2.Quote = Buy, &[]Amount{eur("50000")}[0]
	// Half of it swapped for ETH: ETH carries BTC's average cost (40000/BTC).
	swap := mk("s1", "binance", Trade, "ETH", "10", t0.Add(2*time.Hour))
	swap.Side, swap.Quote = Buy, &[]Amount{Amt(CryptoOrFiat("BTC"), "0.5")}[0]
	// 1 BTC moved to the Ledger: it keeps its cost; the network fee is lost.
	out := mk("o1", "binance", Withdrawal, "BTC", "1", t0.Add(3*time.Hour))
	out.Fee = &Amount{Asset: CryptoOrFiat("BTC"), Qty: decimal.RequireFromString("0.1")}
	in := mk("i1", "ledger", Deposit, "BTC", "1", t0.Add(2*time.Hour)) // the ledger's clock is early
	reward := mk("r1", "binance", Reward, "BNB", "2", t0.Add(4*time.Hour))
	adj := mk("a1", "binance", Adjustment, "ETH", "1", t0.Add(5*time.Hour))
	adj.Side = In
	unknown := mk("r2", "binance", Reward, "NOPE", "5", t0)
	usdt := mk("u1", "binance", Deposit, "USDT", "100", t0)

	c := Costs([]Activity{buy, buy2, swap, out, in, reward, adj, unknown, usdt}, map[string]string{"i1": "o1"}, value)
	want := map[string]string{"b1": "30000", "b2": "50000", "s1": "20000", "i1": "40000", "r1": "600", "a1": "2000"}
	for id, w := range want {
		if c[id].String() != w {
			t.Errorf("%s cost %s, want %s", id, c[id], w)
		}
	}
	if _, ok := c["r2"]; ok {
		t.Error("no price, no cost")
	}
	if _, ok := c["u1"]; ok {
		t.Error("cash has no cost to carry")
	}
}

func TestAdjustments(t *testing.T) {
	now := t0.Add(1000 * time.Hour)
	// BTC sold that history never saw bought, and more held than explained.
	sell := mk("s", "b", Trade, "BTC", "0.3", t0.Add(time.Hour))
	sell.Side, sell.Quote = Sell, &[]Amount{Amt(CryptoOrFiat("EUR"), "9000")}[0]
	dep := mk("d", "b", Deposit, "BTC", "0.1", t0)
	eth := mk("e", "b", Deposit, "ETH", "2", t0)
	acts := []Activity{dep, sell, eth}
	bal := []Balance{{Amount: Amt(CryptoOrFiat("BTC"), "0.5")}, {Amount: Amt(CryptoOrFiat("ETH"), "1.5")}, {Amount: Amt(CryptoOrFiat("EUR"), "9000")}}
	gaps := Reconcile(acts, bal, decimal.New(1, -6))
	adj := Adjustments(acts, gaps, now)
	if len(adj) != 3 {
		t.Fatalf("adjustments %+v", adj)
	}
	byNote := map[string]Activity{}
	for _, a := range adj {
		byNote[a.Base.Asset.Symbol+string(a.Side)+a.Base.Qty.String()] = a
	}
	early, ok := byNote["BTCin0.2"]
	if !ok || !early.Time.Equal(t0.Add(time.Hour-time.Second)) {
		t.Fatalf("early BTC %+v", adj)
	}
	if late, ok := byNote["BTCin0.5"]; !ok || !late.Time.Equal(now) {
		t.Fatalf("late BTC %+v", adj)
	}
	if out, ok := byNote["ETHout0.5"]; !ok || !out.Time.Equal(now) {
		t.Fatalf("ETH %+v", adj)
	}
	// With them, everything adds up; the same gaps give the same ids.
	if g := Reconcile(append(acts, adj...), bal, decimal.New(1, -6)); len(g) != 0 {
		t.Fatalf("still %+v", g)
	}
	again := Adjustments(acts, gaps, now.Add(time.Hour))
	for i := range adj {
		if adj[i].NativeID != again[i].NativeID {
			t.Fatalf("unstable id %s / %s", adj[i].NativeID, again[i].NativeID)
		}
	}
}
