package model

import (
	"sort"

	"github.com/shopspring/decimal"
)

// Holdings replays activities and returns the quantity of each asset they
// add up to. Comparing it with what the platform reports (Reconcile) is how
// we know a source understood the whole history.
func Holdings(acts []Activity) map[string]Amount {
	out := map[string]Amount{}
	add := func(a Amount, sign int64) {
		k := a.Asset.Key()
		cur, ok := out[k]
		if !ok {
			cur = Amount{Asset: a.Asset, Qty: decimal.Zero}
		}
		cur.Qty = cur.Qty.Add(a.Qty.Mul(decimal.NewFromInt(sign)))
		out[k] = cur
	}
	for _, a := range acts {
		switch a.Type {
		case Trade:
			if a.Side == Buy {
				add(a.Base, 1)
				if a.Quote != nil {
					add(*a.Quote, -1)
				}
			} else {
				add(a.Base, -1)
				if a.Quote != nil {
					add(*a.Quote, 1)
				}
			}
		default:
			if a.Inflow() {
				add(a.Base, 1)
			} else if a.Outflow() {
				add(a.Base, -1)
			}
		}
		if a.Fee != nil {
			add(*a.Fee, -1)
		}
	}
	return out
}

// Gap is a difference between replayed history and reported balance.
type Gap struct {
	Asset    Asset           `json:"asset"`
	Replayed decimal.Decimal `json:"replayed"`
	Reported decimal.Decimal `json:"reported"`
	Diff     decimal.Decimal `json:"diff"` // Reported - Replayed
}

// Reconcile lists assets whose replayed quantity differs from the reported
// balance by more than tolerance (relative to the larger of the two, with an
// absolute floor for dust). Sorted by asset.
func Reconcile(acts []Activity, balances []Balance, tolerance decimal.Decimal) []Gap {
	replayed := Holdings(acts)
	reported := map[string]Amount{}
	for _, b := range balances {
		k := b.Asset.Key()
		cur, ok := reported[k]
		if !ok {
			cur = Amount{Asset: b.Asset, Qty: decimal.Zero}
		}
		cur.Qty = cur.Qty.Add(b.Qty)
		reported[k] = cur
	}
	keys := map[string]Asset{}
	for k, a := range replayed {
		keys[k] = a.Asset
	}
	for k, a := range reported {
		keys[k] = a.Asset
	}
	dust := decimal.New(1, -8)
	var gaps []Gap
	for k, asset := range keys {
		rp := replayed[k].Qty
		rr := reported[k].Qty
		diff := rr.Sub(rp)
		scale := decimal.Max(rp.Abs(), rr.Abs())
		if diff.Abs().LessThanOrEqual(dust) || diff.Abs().LessThanOrEqual(scale.Mul(tolerance)) {
			continue
		}
		gaps = append(gaps, Gap{Asset: asset, Replayed: rp, Reported: rr, Diff: diff})
	}
	sort.Slice(gaps, func(i, j int) bool { return gaps[i].Asset.Key() < gaps[j].Asset.Key() })
	return gaps
}
