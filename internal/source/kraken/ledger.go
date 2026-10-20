package kraken

import (
	"context"
	"encoding/json"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// overlap re-reads a little before the last entry seen: records can appear
// late, and both legs of a trade must be read together.
const overlap = 72 * time.Hour

// pageSize is what Ledgers returns per call.
const pageSize = 50

type entry struct {
	Refid   string      `json:"refid"`
	Time    json.Number `json:"time"`
	Type    string      `json:"type"`
	Subtype string      `json:"subtype"`
	Asset   string      `json:"asset"`
	Amount  num         `json:"amount"`
	Fee     num         `json:"fee"`
}

func (s *Source) Activities(ctx context.Context, f source.Fetch) error {
	var start int64
	if !f.Since.IsZero() {
		from := f.Since
		if t := unixTime(s.st.Last); !t.IsZero() {
			from = t
		}
		start = from.Add(-overlap).Unix()
	}
	names := s.assetNames(ctx)
	if s.namesErr != nil {
		progress(f, "asset list unavailable (%v): using built-in names", s.namesErr)
	}
	progress(f, "ledger")
	entries, err := s.ledger(ctx, f, start)
	if err != nil {
		return err
	}
	emit := func(a model.Activity) error {
		if a.Base.Asset.Symbol == "" || a.Base.Qty.IsZero() {
			return nil
		}
		return f.Emit(a)
	}
	if err := mapLedger(entries, names, emit); err != nil {
		return err
	}
	for _, e := range entries {
		if t := string(e.Time); unixTime(t).After(unixTime(s.st.Last)) {
			s.st.Last = t
		}
	}
	return nil
}

// ledger reads every entry after start (Unix seconds, exclusive; 0 for all),
// newest first, 50 per call, keyed by ledger id.
func (s *Source) ledger(ctx context.Context, f source.Fetch, start int64) (map[string]entry, error) {
	out := map[string]entry{}
	for ofs := 0; ; ofs += pageSize {
		q := url.Values{"ofs": {strconv.Itoa(ofs)}}
		if start > 0 {
			q.Set("start", strconv.FormatInt(start, 10))
		}
		var res struct {
			Ledger map[string]entry `json:"ledger"`
			Count  int              `json:"count"`
		}
		if err := s.c.private(ctx, "/0/private/Ledgers", q, &res); err != nil {
			return nil, err
		}
		for id, e := range res.Ledger { // a page can repeat entries if new ones arrived meanwhile
			out[id] = e
		}
		if ofs > 0 && ofs%1000 == 0 {
			progress(f, "ledger: %d of %d entries", len(out), res.Count)
		}
		if len(res.Ledger) < pageSize || ofs+pageSize >= res.Count {
			return out, nil
		}
	}
}

// line is a ledger entry, normalized.
type line struct {
	id, refid, typ, sub, sym string
	at                       time.Time
	amt, fee                 decimal.Decimal
}

// category says how a ledger entry is read:
//   - "internal": moves between the spot wallet and staking/Earn (transfer
//     spottostaking, stakingfromspot…, earn allocation, deallocation,
//     migration): the coin stays the user's, summed in Balances;
//   - "move": deposit, withdrawal, custody transfer, and transfers to or from
//     Kraken Futures (a separate account);
//   - "airdrop": transfer without subtype (forks, airdrops; negative for a
//     delisted coin removed);
//   - "reward": staking, Earn reward, reward, dividend, credit, bonuses;
//   - "margin": margin and rollover entries (profit, loss and fees);
//   - "exchange": everything else (trade, spend/receive from Buy Crypto,
//     conversion, sale, settled, adjustment…): the legs of one refid
//     become a trade when they swap one coin for another.
func category(typ, sub string) string {
	switch typ {
	case "transfer":
		switch {
		case strings.Contains(sub, "staking"):
			return "internal"
		case sub == "":
			return "airdrop"
		}
		return "move"
	case "earn":
		if sub == "reward" {
			return "reward"
		}
		return "internal"
	case "deposit", "withdrawal", "custodytransfer":
		return "move"
	case "staking", "reward", "dividend", "credit", "invite bonus", "nftrebate":
		return "reward"
	case "margin", "rollover":
		return "margin"
	}
	return "exchange"
}

type group struct {
	key, refid, cat string
	lines           []line
}

func mapLedger(entries map[string]entry, names map[string]string, emit func(model.Activity) error) error {
	groups := map[string]*group{}
	for id, e := range entries {
		sym := symbol(e.Asset, names)
		if sym == "" { // fee credits
			continue
		}
		l := line{id: id, refid: e.Refid, typ: strings.ToLower(e.Type), sub: strings.ToLower(e.Subtype), sym: sym,
			at: unixTime(string(e.Time)), amt: dec(string(e.Amount)), fee: dec(string(e.Fee))}
		cat := category(l.typ, l.sub)
		if cat == "internal" {
			if err := emitFee(emit, l.id, l.at, l.sym, l.fee, "Kraken "+l.typ+" fee"); err != nil {
				return err
			}
			continue
		}
		refid := l.refid
		if refid == "" {
			refid = id
		}
		k := refid + "|" + cat
		g := groups[k]
		if g == nil {
			g = &group{key: k, refid: refid, cat: cat}
			groups[k] = g
		}
		g.lines = append(g.lines, l)
	}
	list := make([]*group, 0, len(groups))
	for _, g := range groups {
		sort.Slice(g.lines, func(i, j int) bool { return g.lines[i].id < g.lines[j].id })
		list = append(list, g)
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i].lines[0].at, list[j].lines[0].at
		if !a.Equal(b) {
			return a.Before(b)
		}
		return list[i].key < list[j].key
	})
	for _, g := range list {
		if err := g.emit(emit); err != nil {
			return err
		}
	}
	return nil
}

// emit turns one group (the entries of one refid in one category) into
// activities. Amounts are netted per coin, so legs that cancel out (a failed
// withdrawal given back, ETH2 rewards moved to ETH) leave nothing.
func (g *group) emit(emit func(model.Activity) error) error {
	net := map[string]decimal.Decimal{}
	fees := map[string]decimal.Decimal{}
	var syms []string
	var at time.Time
	for _, l := range g.lines {
		if _, ok := net[l.sym]; !ok {
			syms = append(syms, l.sym)
		}
		net[l.sym] = net[l.sym].Add(l.amt)
		fees[l.sym] = fees[l.sym].Add(l.fee)
		if l.at.After(at) {
			at = l.at
		}
	}
	sort.Strings(syms)
	var moved []string // coins whose amount changed
	for _, s := range syms {
		if !net[s].IsZero() {
			moved = append(moved, s)
		}
	}
	id := g.refid
	if len(g.lines) == 1 {
		id = g.lines[0].id
	}
	typ := g.lines[0].typ
	note := "Kraken " + typ
	if sub := g.lines[0].sub; sub != "" {
		note += " (" + sub + ")"
	}
	feeTaken := map[string]bool{}
	feeOf := func(sym string) *model.Amount {
		if f := fees[sym]; f.IsPositive() && !feeTaken[sym] {
			feeTaken[sym] = true
			return &model.Amount{Asset: model.CryptoOrFiat(sym), Qty: f}
		}
		return nil
	}

	if g.cat == "exchange" && len(moved) == 2 && net[moved[0]].Sign() != net[moved[1]].Sign() {
		base, quote := moved[0], moved[1]
		if quoteRank(base) < quoteRank(quote) || (quoteRank(base) == quoteRank(quote) && net[base].IsNegative()) {
			base, quote = quote, base
		}
		a := model.Activity{
			NativeID: g.refid, Time: at, Type: model.Trade, Side: model.Buy,
			Base:  model.Amount{Asset: model.CryptoOrFiat(base), Qty: net[base].Abs()},
			Quote: &model.Amount{Asset: model.CryptoOrFiat(quote), Qty: net[quote].Abs()},
			Note:  note,
		}
		if net[base].IsNegative() {
			a.Side = model.Sell
		}
		if a.Fee = feeOf(quote); a.Fee == nil {
			a.Fee = feeOf(base)
		}
		if err := emit(a); err != nil {
			return err
		}
	} else {
		for _, s := range moved {
			v := net[s]
			a := model.Activity{NativeID: id, Time: at, Note: note, Base: model.Amount{Asset: model.CryptoOrFiat(s), Qty: v.Abs()}}
			if len(moved) > 1 {
				a.NativeID = id + ":" + s
			}
			switch {
			case g.cat == "reward" && v.IsNegative() && ethStaking(g.lines):
				continue // ETH2 rewards moved out of staking: not a loss
			case (g.cat == "reward" || g.cat == "airdrop") && v.IsPositive():
				a.Type = model.Reward
			case g.cat == "margin" && v.IsPositive():
				a.Type, a.Note = model.Reward, note+": profit"
			case g.cat == "reward" || g.cat == "margin":
				a.Type = model.Fee
				if g.cat == "margin" {
					a.Note = note + ": loss"
				}
			case v.IsPositive():
				a.Type = model.Deposit
			default:
				a.Type = model.Withdrawal
			}
			a.Fee = feeOf(s)
			if err := emit(a); err != nil {
				return err
			}
		}
	}
	for _, s := range syms {
		if feeTaken[s] {
			continue
		}
		fid := id + ":fee"
		if len(syms) > 1 {
			fid += ":" + s
		}
		if err := emitFee(emit, fid, at, s, fees[s], note+" fee"); err != nil {
			return err
		}
	}
	return nil
}

// emitFee records a fee not attached to another activity; a negative fee
// (given back) is a reward.
func emitFee(emit func(model.Activity) error, id string, at time.Time, sym string, fee decimal.Decimal, note string) error {
	if fee.IsZero() {
		return nil
	}
	a := model.Activity{NativeID: id, Time: at, Type: model.Fee, Note: note,
		Base: model.Amount{Asset: model.CryptoOrFiat(sym), Qty: fee.Abs()}}
	if fee.IsNegative() {
		a.Type, a.Note = model.Reward, note+" refund"
	}
	return emit(a)
}

// ethStaking: Kraken's virtual entry moving staked ETH rewards to ETH.
func ethStaking(lines []line) bool {
	for _, l := range lines {
		if l.typ == "staking" && l.sym == "ETH" && l.amt.IsNegative() {
			return true
		}
	}
	return false
}

// quoteRank orders what a trade is priced in: fiat, then stablecoins, then
// BTC, then ETH; anything else is the coin traded.
func quoteRank(sym string) int {
	a := model.CryptoOrFiat(sym)
	switch {
	case a.Kind == model.Fiat:
		return 0
	case model.PeggedTo(a) != "":
		return 1
	case sym == "BTC":
		return 2
	case sym == "ETH":
		return 3
	}
	return 9
}
