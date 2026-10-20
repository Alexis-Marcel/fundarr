package model

import (
	"sort"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// Pair links a withdrawal on one source to the deposit it became on another
// (Binance → Ledger, OKX → Binance): the coins stayed yours, so destinations
// move them between accounts and carry their cost over.
type Pair struct {
	OutID string `json:"outId"`
	InID  string `json:"inId"`
	// By says how the two were matched: "txid" (same transaction hash),
	// "amount" (same asset and amount, a few days apart) or "user".
	By string `json:"by"`
}

// Window in which a deposit can follow the withdrawal it comes from.
const (
	pairBefore = time.Hour           // clocks of two platforms disagree a little
	pairAfter  = 72 * time.Hour      // slow chains, exchange reviews
	userWindow = 45 * 24 * time.Hour // what the user may pair by hand
)

func txKey(tx string) string {
	t := strings.ToLower(strings.TrimSpace(tx))
	t = strings.TrimPrefix(t, "0x")
	if len(t) < 16 || strings.ContainsAny(t, " :") { // "Internal transfer 123", bank refs
		return ""
	}
	return t
}

// Transferable reports whether an activity can be one side of a transfer
// between your own accounts. Fiat moves through banks Fundarr does not see.
func Transferable(a Activity) bool {
	return (a.Type == Withdrawal || a.Type == Deposit) && a.Base.Asset.Kind == Crypto && a.Base.Qty.IsPositive()
}

// amountFits says whether in could be what arrived from out: at most what
// left (plus a fee the platform counted apart), at least 99% of it.
func amountFits(out, in Activity) bool {
	hi := out.Base.Qty
	if out.Fee != nil && out.Fee.Asset.Key() == out.Base.Asset.Key() {
		hi = hi.Add(out.Fee.Qty)
	}
	lo := out.Base.Qty.Mul(decimal.RequireFromString("0.99"))
	if out.Fee != nil && out.Fee.Asset.Key() == out.Base.Asset.Key() {
		lo = decimal.Min(lo, out.Base.Qty.Sub(out.Fee.Qty))
	}
	return in.Base.Qty.LessThanOrEqual(hi.Mul(decimal.RequireFromString("1.000001"))) && in.Base.Qty.GreaterThanOrEqual(lo)
}

// CanPair reports whether the user may link out and in by hand.
func CanPair(out, in Activity) bool {
	return out.Type == Withdrawal && in.Type == Deposit && out.SourceID != in.SourceID &&
		out.Base.Asset.Key() == in.Base.Asset.Key() &&
		in.Time.After(out.Time.Add(-userWindow)) && in.Time.Before(out.Time.Add(userWindow))
}

// PairTransfers matches withdrawals with deposits across sources. forced
// pairs (chosen by the user) come first; refused ones ("out\x00in") are never
// made. Each activity is in at most one pair. Hashes win over amounts; among
// amount matches the closest in time wins.
func PairTransfers(acts []Activity, forced []Pair, refused map[string]bool) []Pair {
	byID := map[string]Activity{}
	var outs, ins []Activity
	for _, a := range acts {
		byID[a.ID] = a
		if !Transferable(a) {
			continue
		}
		if a.Type == Withdrawal {
			outs = append(outs, a)
		} else {
			ins = append(ins, a)
		}
	}
	used := map[string]bool{}
	var pairs []Pair
	for _, p := range forced {
		o, okO := byID[p.OutID]
		i, okI := byID[p.InID]
		if !okO || !okI || used[o.ID] || used[i.ID] || !CanPair(o, i) {
			continue
		}
		used[o.ID], used[i.ID] = true, true
		pairs = append(pairs, Pair{OutID: o.ID, InID: i.ID, By: "user"})
	}
	ok := func(o, i Activity) bool {
		return !used[o.ID] && !used[i.ID] && o.SourceID != i.SourceID &&
			o.Base.Asset.Key() == i.Base.Asset.Key() && !refused[o.ID+"\x00"+i.ID]
	}
	// Same transaction hash: the same coins, whatever the timing.
	byTx := map[string][]Activity{}
	for _, i := range ins {
		if k := txKey(i.TxID); k != "" {
			byTx[k] = append(byTx[k], i)
		}
	}
	for _, o := range outs {
		k := txKey(o.TxID)
		if k == "" {
			continue
		}
		for _, i := range byTx[k] {
			if ok(o, i) && amountFits(o, i) {
				used[o.ID], used[i.ID] = true, true
				pairs = append(pairs, Pair{OutID: o.ID, InID: i.ID, By: "txid"})
				break
			}
		}
	}
	// Same asset and amount within the window, closest first.
	type cand struct {
		o, i Activity
		gap  time.Duration
	}
	var cands []cand
	for _, o := range outs {
		if used[o.ID] {
			continue
		}
		for _, i := range ins {
			if !ok(o, i) || i.Time.Before(o.Time.Add(-pairBefore)) || i.Time.After(o.Time.Add(pairAfter)) || !amountFits(o, i) {
				continue
			}
			// Two hashes that are both known and differ are two transactions.
			if txKey(o.TxID) != "" && txKey(i.TxID) != "" {
				continue
			}
			g := i.Time.Sub(o.Time)
			if g < 0 {
				g = -g
			}
			cands = append(cands, cand{o, i, g})
		}
	}
	sort.SliceStable(cands, func(a, b int) bool {
		if cands[a].gap != cands[b].gap {
			return cands[a].gap < cands[b].gap
		}
		return cands[a].o.ID+cands[a].i.ID < cands[b].o.ID+cands[b].i.ID
	})
	for _, c := range cands {
		if used[c.o.ID] || used[c.i.ID] {
			continue
		}
		used[c.o.ID], used[c.i.ID] = true, true
		pairs = append(pairs, Pair{OutID: c.o.ID, InID: c.i.ID, By: "amount"})
	}
	sort.Slice(pairs, func(a, b int) bool {
		ta, tb := byID[pairs[a].OutID].Time, byID[pairs[b].OutID].Time
		if !ta.Equal(tb) {
			return ta.Before(tb)
		}
		return pairs[a].OutID < pairs[b].OutID
	})
	return pairs
}
