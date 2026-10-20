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

// Gap is a difference between replayed history and reported balance, in
// one account of the source, or in the source as a whole (Pooled).
type Gap struct {
	Asset Asset `json:"asset"`
	// Account the gap is in, and its correction goes to.
	Account string `json:"account,omitempty"`
	// Pooled: compared summed over the source's accounts, because the
	// platform did not split the balance by account (AllAccounts), or
	// because the accounts' gaps went opposite ways: an asset that would
	// only move from one account to another is not a gap.
	Pooled   bool            `json:"pooled,omitempty"`
	Replayed decimal.Decimal `json:"replayed"`
	Reported decimal.Decimal `json:"reported"`
	Diff     decimal.Decimal `json:"diff"` // Reported - Replayed
}

// AllAccounts, as a balance's account: the platform's figure covers every
// account of the source together. That asset is compared summed over the
// accounts.
const AllAccounts = "*"

// Reconcile lists the assets whose replayed quantity differs from the
// reported balance by more than tolerance (relative to the larger of the
// two, with an absolute floor for dust), account by account. Sorted by
// asset, then account.
//
// An account's Unread balance is not compared (the history stands for it).
// Gaps of one asset that go opposite ways in different accounts are
// compared summed over the accounts (Pooled): no correction then moves an
// asset from one account to another.
func Reconcile(acts []Activity, balances []Balance, tolerance decimal.Decimal) []Gap {
	type ak struct{ acc, key string }
	byAcc := map[string][]Activity{}
	for _, a := range acts {
		byAcc[a.Account] = append(byAcc[a.Account], a)
	}
	replayed := map[ak]decimal.Decimal{}
	assets := map[string]Asset{}
	accsOf := map[string]map[string]bool{}
	seen := func(acc string, asset Asset) {
		k := asset.Key()
		if _, ok := assets[k]; !ok {
			assets[k] = asset
		}
		if accsOf[k] == nil {
			accsOf[k] = map[string]bool{}
		}
		accsOf[k][acc] = true
	}
	for acc, list := range byAcc {
		for k, h := range Holdings(list) {
			replayed[ak{acc, k}] = h.Qty
			seen(acc, h.Asset)
		}
	}
	reported := map[ak]decimal.Decimal{}
	unread := map[ak]bool{}
	pooled := map[string]bool{}
	for _, b := range balances {
		k := b.Asset.Key()
		if b.Unread {
			unread[ak{b.Account, k}] = true
			continue
		}
		if b.Account == AllAccounts {
			pooled[k] = true
			if _, ok := assets[k]; !ok {
				assets[k] = b.Asset
			}
			if accsOf[k] == nil {
				accsOf[k] = map[string]bool{}
			}
		} else {
			seen(b.Account, b.Asset)
		}
		reported[ak{b.Account, k}] = reported[ak{b.Account, k}].Add(b.Qty)
	}
	dust := decimal.New(1, -8)
	differs := func(rp, rr decimal.Decimal) bool {
		diff := rr.Sub(rp).Abs()
		return diff.GreaterThan(dust) && diff.GreaterThan(decimal.Max(rp.Abs(), rr.Abs()).Mul(tolerance))
	}
	var gaps []Gap
	for k, asset := range assets {
		var accs []string
		for acc := range accsOf[k] {
			accs = append(accs, acc)
		}
		sort.Strings(accs)
		// Summed over the accounts; an unread account counts its history.
		rpAll, rrAll := decimal.Zero, reported[ak{AllAccounts, k}]
		var each []Gap
		signs := map[int]bool{}
		for _, acc := range accs {
			rp, rr := replayed[ak{acc, k}], reported[ak{acc, k}]
			if unread[ak{acc, k}] {
				rr = rp
			}
			rpAll, rrAll = rpAll.Add(rp), rrAll.Add(rr)
			if differs(rp, rr) {
				each = append(each, Gap{Asset: asset, Account: acc, Replayed: rp, Reported: rr, Diff: rr.Sub(rp)})
				signs[rr.Sub(rp).Sign()] = true
			}
		}
		if !pooled[k] && len(signs) < 2 {
			gaps = append(gaps, each...)
			continue
		}
		if !differs(rpAll, rrAll) {
			continue
		}
		g := Gap{Asset: asset, Pooled: true, Replayed: rpAll, Reported: rrAll, Diff: rrAll.Sub(rpAll)}
		// The correction goes where the history is most off that way; the
		// main account otherwise.
		best := decimal.Zero
		for _, e := range each {
			if e.Diff.Sign() == g.Diff.Sign() && e.Diff.Abs().GreaterThan(best) {
				g.Account, best = e.Account, e.Diff.Abs()
			}
		}
		gaps = append(gaps, g)
	}
	sort.Slice(gaps, func(i, j int) bool {
		if a, b := gaps[i].Asset.Key(), gaps[j].Asset.Key(); a != b {
			return a < b
		}
		return gaps[i].Account < gaps[j].Account
	})
	return gaps
}
