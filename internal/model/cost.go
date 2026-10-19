package model

import (
	"sort"
	"time"

	"github.com/shopspring/decimal"
)

// Valuer gives the market value of an amount at a time, in one currency.
// ok is false when no price is known.
type Valuer func(a Amount, at time.Time) (value decimal.Decimal, ok bool)

// Costs replays every source's history together and returns, for each
// activity that brings a non-cash asset in, what that asset cost, in the
// Valuer's currency:
//
//   - bought with cash: the cash paid;
//   - swapped for another asset, or moved from another of your accounts (a
//     paired transfer): the cost carried by what left, at its average cost;
//   - received for nothing (rewards, airdrops, deposits from outside,
//     adjustments): its market value that day.
//
// Activities whose cost is unknown (no price) are left out. pairs maps a
// deposit's id to the withdrawal it came from.
func Costs(acts []Activity, pairs map[string]string, value Valuer) map[string]decimal.Decimal {
	type pool struct{ qty, cost decimal.Decimal }
	pools := map[string]*pool{}
	poolOf := func(src string, a Asset) *pool {
		k := src + "\x00" + a.Key()
		p := pools[k]
		if p == nil {
			p = &pool{}
			pools[k] = p
		}
		return p
	}
	market := func(a Amount, at time.Time) (decimal.Decimal, bool) {
		if a.Qty.IsZero() {
			return decimal.Zero, true
		}
		return value(a, at)
	}
	// take removes an amount from its pool and returns the cost it carried.
	// What the history does not explain is taken at market value.
	take := func(src string, a Amount, at time.Time) (decimal.Decimal, bool) {
		if CashLike(a.Asset) {
			return market(a, at)
		}
		p := poolOf(src, a.Asset)
		have := decimal.Max(p.qty, decimal.Zero)
		from := decimal.Min(have, a.Qty)
		cost := decimal.Zero
		if from.IsPositive() {
			cost = p.cost.Mul(from).Div(p.qty)
			p.cost = p.cost.Sub(cost)
		}
		p.qty = p.qty.Sub(a.Qty)
		if p.qty.LessThanOrEqual(decimal.Zero) {
			p.cost = decimal.Zero
		}
		ok := true
		if rest := a.Qty.Sub(from); rest.IsPositive() {
			v, known := market(Amount{Asset: a.Asset, Qty: rest}, at)
			cost, ok = cost.Add(v), known
		}
		return cost, ok
	}
	put := func(src string, a Amount, cost decimal.Decimal) {
		if CashLike(a.Asset) {
			return
		}
		p := poolOf(src, a.Asset)
		if p.qty.IsNegative() {
			// Coins that left before history shows them arriving: start over.
			p.qty, p.cost = decimal.Zero, decimal.Zero
		}
		p.qty = p.qty.Add(a.Qty)
		p.cost = p.cost.Add(cost)
	}

	// A deposit is replayed after the withdrawal it came from, even when the
	// receiving platform's clock says otherwise.
	byID := map[string]Activity{}
	for _, a := range acts {
		byID[a.ID] = a
	}
	at := func(a Activity) time.Time {
		if o, ok := byID[pairs[a.ID]]; ok && !a.Time.After(o.Time) {
			return o.Time.Add(time.Nanosecond)
		}
		return a.Time
	}
	order := make([]Activity, len(acts))
	copy(order, acts)
	sort.SliceStable(order, func(i, j int) bool { return at(order[i]).Before(at(order[j])) })

	out := map[string]decimal.Decimal{}
	carried := map[string]decimal.Decimal{} // withdrawal id → cost that left with it
	known := map[string]bool{}
	for _, a := range order {
		var fee *Amount
		if a.Fee != nil && a.Fee.Qty.IsPositive() {
			fee = a.Fee
		}
		switch {
		case a.Type == Trade && a.Quote != nil:
			in, given := a.Base, *a.Quote
			if a.Side == Sell {
				in, given = *a.Quote, a.Base
			}
			cost, ok := take(a.SourceID, given, a.Time)
			if !ok {
				cost, ok = market(in, a.Time)
			}
			net := in
			if fee != nil && fee.Asset.Key() == in.Asset.Key() {
				// The fee came out of what was received: fewer units, same cost.
				net.Qty = in.Qty.Sub(fee.Qty)
				fee = nil
			}
			put(a.SourceID, net, cost)
			if ok && !CashLike(in.Asset) {
				out[a.ID] = cost
			}
		case a.Inflow():
			var cost decimal.Decimal
			var ok bool
			if o, paired := pairs[a.ID]; paired && known[o] {
				cost, ok = carried[o], true
				// A little less arrives than left (network fee): same cost.
			} else {
				cost, ok = market(a.Base, a.Time)
			}
			put(a.SourceID, a.Base, cost)
			if ok && !CashLike(a.Base.Asset) {
				out[a.ID] = cost
			}
		case a.Outflow():
			cost, ok := take(a.SourceID, a.Base, a.Time)
			carried[a.ID], known[a.ID] = cost, ok
		}
		if fee != nil {
			take(a.SourceID, *fee, a.Time)
		}
	}
	return out
}
