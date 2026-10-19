package model

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

func TestHoldingsAndReconcile(t *testing.T) {
	usdt, btc, eur := CryptoOrFiat("usdt"), CryptoOrFiat("BTC"), CryptoOrFiat("eur")
	if eur.Kind != Fiat || btc.Kind != Crypto {
		t.Fatalf("kinds: %v %v", eur, btc)
	}
	fee := Amt(btc, "0.0001")
	acts := []Activity{
		{Type: Deposit, Base: Amt(eur, "1000")},
		{Type: Trade, Side: Buy, Base: Amt(usdt, "1050"), Quote: ptr(Amt(eur, "1000"))},
		{Type: Trade, Side: Buy, Base: Amt(btc, "0.02"), Quote: ptr(Amt(usdt, "1000")), Fee: &fee},
		{Type: Reward, Base: Amt(btc, "0.0005")},
		{Type: Withdrawal, Base: Amt(btc, "0.01"), Fee: ptr(Amt(btc, "0.0002"))},
	}
	h := Holdings(acts)
	want := map[string]string{"fiat:EUR": "0", "crypto:USDT": "50", "crypto:BTC": "0.0102"}
	for k, v := range want {
		if !h[k].Qty.Equal(decimal.RequireFromString(v)) {
			t.Errorf("%s = %s, want %s", k, h[k].Qty, v)
		}
	}

	bal := []Balance{{Amount: Amt(btc, "0.0102")}, {Amount: Amt(usdt, "50")}, {Amount: Amt(CryptoOrFiat("ETH"), "1")}}
	gaps := Reconcile(acts, bal, decimal.New(1, -6))
	if len(gaps) != 1 || gaps[0].Asset.Symbol != "ETH" || !gaps[0].Diff.Equal(decimal.NewFromInt(1)) {
		t.Fatalf("gaps = %+v", gaps)
	}

	// Sells move the other way; dust below 1e-8 is ignored.
	acts = append(acts, Activity{Type: Trade, Side: Sell, Base: Amt(btc, "0.0102"), Quote: ptr(Amt(usdt, "600"))})
	bal = []Balance{{Amount: Amt(usdt, "650.000000001")}}
	if g := Reconcile(acts, bal, decimal.Zero); len(g) != 0 {
		t.Fatalf("unexpected gaps %+v", g)
	}
}

func TestActivityIDAndPrice(t *testing.T) {
	if ActivityID("s1", "a") == ActivityID("s1", "b") || ActivityID("s1", "a") != ActivityID("s1", "a") {
		t.Fatal("ids must be stable and distinct")
	}
	a := Activity{Time: time.Now(), Type: Trade, Base: Amt(CryptoOrFiat("BTC"), "0.5"), Quote: ptr(Amt(CryptoOrFiat("EUR"), "30000"))}
	if !a.Price().Equal(decimal.NewFromInt(60000)) {
		t.Fatalf("price %s", a.Price())
	}
	if (Activity{}).Price().Sign() != 0 {
		t.Fatal("no quote, no price")
	}
	if (Asset{Kind: Security, Symbol: "IWDA", ISIN: "IE00B4L5Y983"}).Key() != "security:IE00B4L5Y983" {
		t.Fatal("isin wins")
	}
}

func ptr[T any](v T) *T { return &v }
