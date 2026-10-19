package model

import (
	"fmt"
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Adjustments returns the corrections that make acts add up to the reported
// balances (gaps from Reconcile on the same acts).
//
// Where history shows an asset leaving before it ever arrived (a purchase
// the platform no longer reports), the missing quantity is added just
// before the balance would go negative, so it is valued at that day's price
// and replayed in order. Whatever else is missing or extra is corrected at
// now. Ids depend only on the content: the same gap gives the same
// adjustment on every run.
func Adjustments(acts []Activity, gaps []Gap, now time.Time) []Activity {
	sorted := make([]Activity, len(acts))
	copy(sorted, acts)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Time.Before(sorted[j].Time) })
	var out []Activity
	mk := func(g Gap, side Side, q decimal.Decimal, at time.Time, why string) {
		// A correction made "now" keeps the date it was first made on: its
		// id leaves the time out, and the store keeps the first copy.
		when := "now"
		if !at.Equal(now) {
			when = fmt.Sprint(at.UnixMilli())
		}
		a := Activity{
			NativeID: fmt.Sprintf("%s%s:%s:%s:%s", AdjustPrefix, g.Asset.Key(), side, when, q.String()),
			Time:     at, Type: Adjustment, Side: side, Base: Amount{Asset: g.Asset, Qty: q}, Note: why,
		}
		out = append(out, a)
	}
	dust := decimal.New(1, -8)
	for _, g := range gaps {
		missing := g.Diff
		if missing.IsPositive() {
			// Running balance of this asset, alone.
			run, low := decimal.Zero, decimal.Zero
			var first time.Time
			for _, a := range sorted {
				d := delta(a, g.Asset)
				if d.IsZero() {
					continue
				}
				run = run.Add(d)
				if run.LessThan(low.Sub(dust)) {
					if first.IsZero() {
						first = a.Time
					}
					low = run
				}
			}
			if early := decimal.Min(low.Neg(), missing); early.GreaterThan(dust) && !first.IsZero() {
				mk(g, In, early, first.Add(-time.Second), "missing from the platform's history: it shows more leaving than ever arrived")
				missing = missing.Sub(early)
			}
			if missing.GreaterThan(dust) {
				mk(g, In, missing, now, "held on the platform but missing from its history")
			}
		} else if missing.IsNegative() {
			mk(g, Out, missing.Neg(), now, "in the history but no longer held on the platform")
		}
	}
	return out
}

// delta is how much of asset an activity moves (positive in).
func delta(a Activity, asset Asset) decimal.Decimal {
	k := asset.Key()
	d := decimal.Zero
	h := Holdings([]Activity{a})
	if v, ok := h[k]; ok {
		d = v.Qty
	}
	return d
}
