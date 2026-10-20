package portfolio_test

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/Alexis-Marcel/fundarr/pkg/api"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/Alexis-Marcel/fundarr/pkg/portfolio"
	"github.com/shopspring/decimal"
)

// Two pages of GET /api/v1/changes: BTC bought on an exchange and moved to
// a wallet, then part of it sent back and sold, and a balance correction
// Fundarr dropped since.
const page1 = `{"changes": [
 {"op": "upsert", "id": "buy", "activity": {"id": "buy", "sourceId": "binance", "time": "2024-01-10T09:00:00Z",
  "type": "trade", "side": "buy", "base": {"asset": {"kind": "crypto", "symbol": "BTC"}, "qty": "0.1"},
  "quote": {"asset": {"kind": "fiat", "symbol": "EUR"}, "qty": "4000"}, "source": {"id": "binance", "name": "Binance", "kind": "binance"}}},
 {"op": "upsert", "id": "out", "activity": {"id": "out", "sourceId": "binance", "time": "2024-02-01T09:00:00Z",
  "type": "withdrawal", "base": {"asset": {"kind": "crypto", "symbol": "BTC"}, "qty": "0.1"}, "transferWith": "in"}},
 {"op": "upsert", "id": "in", "activity": {"id": "in", "sourceId": "ledger", "time": "2024-02-01T09:20:00Z",
  "type": "deposit", "base": {"asset": {"kind": "crypto", "symbol": "BTC"}, "qty": "0.1"}, "transferWith": "out"}},
 {"op": "upsert", "id": "fix", "activity": {"id": "fix", "sourceId": "ledger", "time": "2024-06-01T00:00:00Z",
  "type": "adjustment", "side": "in", "base": {"asset": {"kind": "crypto", "symbol": "BTC"}, "qty": "0.01"}, "correction": true}}
], "cursor": "4e1a9c02.4", "more": false}`

const page2 = `{"changes": [
 {"op": "remove", "id": "fix"},
 {"op": "upsert", "id": "back", "activity": {"id": "back", "sourceId": "ledger", "time": "2025-03-01T09:00:00Z",
  "type": "withdrawal", "base": {"asset": {"kind": "crypto", "symbol": "BTC"}, "qty": "0.05"}, "transferWith": "back-in"}},
 {"op": "upsert", "id": "back-in", "activity": {"id": "back-in", "sourceId": "binance", "time": "2025-03-01T09:30:00Z",
  "type": "deposit", "base": {"asset": {"kind": "crypto", "symbol": "BTC"}, "qty": "0.05"}, "transferWith": "back"}},
 {"op": "upsert", "id": "sale", "activity": {"id": "sale", "sourceId": "binance", "time": "2025-03-02T10:00:00Z",
  "type": "trade", "side": "sell", "base": {"asset": {"kind": "crypto", "symbol": "BTC"}, "qty": "0.05"},
  "quote": {"asset": {"kind": "fiat", "symbol": "EUR"}, "qty": "3000"}}}
], "cursor": "4e1a9c02.8", "more": false}`

// prices: BTC at 40000 € in 2024, 60000 € after; euros are euros. Use
// price.New(…).UnitPrice(ctx, "EUR") for real daily closes.
func prices(a model.Asset, at time.Time) (decimal.Decimal, bool) {
	switch {
	case a.Kind == model.Fiat && a.Symbol == "EUR":
		return decimal.NewFromInt(1), true
	case a.Symbol == "BTC" && at.Year() <= 2024:
		return decimal.NewFromInt(40000), true
	case a.Symbol == "BTC":
		return decimal.NewFromInt(60000), true
	}
	return decimal.Zero, false
}

// An app keeps its own copy from the changes feed, then computes the
// portfolio and the French 2086 form with Fundarr's code.
func Example() {
	copy := api.Copy{}
	for _, raw := range []string{page1, page2} {
		var page api.Changes
		if err := json.Unmarshal([]byte(raw), &page); err != nil {
			panic(err)
		}
		copy.Apply(page) // and store page.Cursor for next time
	}
	acts, transfers := copy.Ledger()

	now := portfolio.Now(acts, transfers, prices, time.Date(2025, 12, 31, 0, 0, 0, 0, time.UTC))
	fmt.Println("value", now.Value.StringFixed(2), "cost", now.Cost.StringFixed(2), "realized", now.Realized.StringFixed(2))
	for _, h := range now.Assets {
		fmt.Println(h.Asset, h.Qty, "on", len(h.Sources), "source(s)")
	}

	tax := portfolio.TaxFR(acts, transfers, prices, 2025, false)
	for _, d := range tax.Disposals {
		fmt.Println("2086:", d.Sold.Qty, d.Sold.Asset, "sold", d.Price.StringFixed(2), "gain", d.Gain.StringFixed(2))
	}
	fmt.Println("tax", tax.Tax.StringFixed(2))
	// Output:
	// value 3000.00 cost 2000.00 realized 1000.00
	// BTC 0.05 on 1 source(s)
	// 2086: 0.05 BTC sold 3000.00 gain 1000.00
	// tax 314.00
}
