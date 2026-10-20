// Package portfolio values the crypto held across every source: what it is
// worth, what went into it and what it gained, now and day by day, and the
// French tax on its disposals (formulaire 2086). Fiat and securities are
// left out; stablecoins count as crypto. See docs/api.md.
package portfolio

import (
	"sort"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/shopspring/decimal"
)

const day = 24 * time.Hour

// Price is the unit price of an asset on the day of at, in one currency;
// ok is false when no price is known.
type Price func(a model.Asset, at time.Time) (p decimal.Decimal, ok bool)

// Cached remembers p's answers by asset and day: the same days come back
// for every point and every disposal.
func Cached(p Price) Price {
	type key struct{ asset, day string }
	type val struct {
		p  decimal.Decimal
		ok bool
	}
	memo := map[key]val{}
	return func(a model.Asset, at time.Time) (decimal.Decimal, bool) {
		k := key{a.Key(), at.UTC().Format(time.DateOnly)}
		if v, ok := memo[k]; ok {
			return v.p, v.ok
		}
		v, ok := p(a, at)
		memo[k] = val{v, ok}
		return v, ok
	}
}

// value of an amount; nothing is worth nothing, priced or not.
func (p Price) value(a model.Amount, at time.Time) (decimal.Decimal, bool) {
	if a.Qty.IsZero() {
		return decimal.Zero, true
	}
	u, ok := p(a.Asset, at)
	return u.Mul(a.Qty), ok
}

func (p Price) valuer() model.Valuer { return p.value }

func crypto(a model.Asset) bool { return a.Kind == model.Crypto }

// legs returns what a trade brought in and what it gave.
func legs(a model.Activity) (in, given model.Amount) {
	if a.Side == model.Sell {
		return *a.Quote, a.Base
	}
	return a.Base, *a.Quote
}

func isTrade(a model.Activity) bool { return a.Type == model.Trade && a.Quote != nil }

// walker replays the history in order and keeps the crypto each source
// holds and the money invested so far.
type walker struct {
	price    Price
	in, out  map[string]string // deposit → withdrawal, withdrawal → deposit
	held     map[string]map[string]decimal.Decimal
	assets   map[string]model.Asset
	invested decimal.Decimal
	unpriced map[string]model.Asset
}

func newWalker(price Price, in map[string]string) *walker {
	w := &walker{price: price, in: in, out: map[string]string{}, held: map[string]map[string]decimal.Decimal{},
		assets: map[string]model.Asset{}, unpriced: map[string]model.Asset{}}
	for i, o := range in {
		w.out[o] = i
	}
	return w
}

func (w *walker) move(src string, a model.Amount, sign int64) {
	if !crypto(a.Asset) || a.Qty.IsZero() {
		return
	}
	k := a.Asset.Key()
	if w.held[k] == nil {
		w.held[k], w.assets[k] = map[string]decimal.Decimal{}, a.Asset
	}
	w.held[k][src] = w.held[k][src].Add(a.Qty.Mul(decimal.NewFromInt(sign)))
}

// worth values what is known, recording what is not.
func (w *walker) worth(a model.Amount, at time.Time) decimal.Decimal {
	v, ok := w.price.value(a, at)
	if !ok {
		w.unpriced[a.Asset.Key()] = a.Asset
	}
	return v
}

// external: a move to or from outside your sources (or a correction).
func (w *walker) external(a model.Activity) bool {
	switch a.Type {
	case model.Deposit:
		return w.in[a.ID] == ""
	case model.Withdrawal:
		return w.out[a.ID] == ""
	}
	return a.Type == model.Adjustment
}

// apply replays one activity.
func (w *walker) apply(a model.Activity) {
	switch {
	case isTrade(a):
		in, given := legs(a)
		if !crypto(in.Asset) && !crypto(given.Asset) {
			break
		}
		w.move(a.SourceID, in, 1)
		w.move(a.SourceID, given, -1)
		switch {
		case given.Asset.Kind == model.Fiat && crypto(in.Asset):
			w.invested = w.invested.Add(w.worth(given, a.Time))
		case crypto(given.Asset) && in.Asset.Kind == model.Fiat:
			w.invested = w.invested.Sub(w.worth(in, a.Time))
		}
		if a.Fee != nil && a.Fee.Asset.Kind == model.Fiat {
			w.invested = w.invested.Add(w.worth(*a.Fee, a.Time))
		}
	case a.Inflow():
		w.move(a.SourceID, a.Base, 1)
		if crypto(a.Base.Asset) && w.external(a) {
			w.invested = w.invested.Add(w.worth(a.Base, a.Time))
		}
	case a.Outflow():
		w.move(a.SourceID, a.Base, -1)
		if crypto(a.Base.Asset) && w.external(a) {
			w.invested = w.invested.Sub(w.worth(a.Base, a.Time))
		}
	}
	if a.Fee != nil {
		w.move(a.SourceID, *a.Fee, -1)
	}
}

// qty held of an asset key, across sources (a source showing more leaving
// than arriving holds nothing).
func (w *walker) qty(k string) decimal.Decimal {
	q := decimal.Zero
	for _, v := range w.held[k] {
		if v.IsPositive() {
			q = q.Add(v)
		}
	}
	return q
}

// value of everything held, at the day's prices.
func (w *walker) value(at time.Time) decimal.Decimal {
	total := decimal.Zero
	for k, a := range w.assets {
		if q := w.qty(k); q.IsPositive() {
			total = total.Add(w.worth(model.Amount{Asset: a, Qty: q}, at))
		}
	}
	return total
}

func (w *walker) unpricedList() []model.Asset {
	out := []model.Asset{}
	for _, a := range w.unpriced {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// ---- now

// Summary is the portfolio at one time.
type Summary struct {
	Value, Invested, Cost, Unrealized, Realized, Rewards decimal.Decimal
	Assets                                               []Holding // largest value first
	Sources                                              []Worth   // largest value first
	Unpriced                                             []model.Asset
}

type Holding struct {
	Asset                               model.Asset
	Qty, Price, Value, Cost, Unrealized decimal.Decimal
	Share                               decimal.Decimal // of Summary.Value
	Sources                             []Worth         // Qty by source
}

// Worth is a source's part: a quantity or a value.
type Worth struct {
	Source string
	Amount decimal.Decimal
}

// Now replays the whole history (in maps a deposit's id to the withdrawal
// it came from) and values what is held at at.
func Now(acts []model.Activity, in map[string]string, price Price, at time.Time) Summary {
	price = Cached(price)
	acts = past(acts, at)
	w := newWalker(price, in)
	for _, a := range model.ReplayOrder(acts, in) {
		w.apply(a)
	}
	// Costs' pools hold what is not cash; cash-like coins cost what they are worth.
	book := model.Books(acts, in, price.valuer())
	var s Summary
	s.Invested = w.invested
	s.Realized, s.Rewards = gains(acts, book, w)
	bySource := map[string]decimal.Decimal{}
	for k, asset := range w.assets {
		q := w.qty(k)
		if !q.IsPositive() {
			continue
		}
		u, ok := price(asset, at)
		if !ok {
			w.unpriced[k] = asset
			continue
		}
		h := Holding{Asset: asset, Qty: q, Price: u, Value: u.Mul(q), Cost: decimal.Zero}
		for src, v := range w.held[k] {
			if !v.IsPositive() {
				continue
			}
			h.Sources = append(h.Sources, Worth{src, v})
			bySource[src] = bySource[src].Add(u.Mul(v))
			if !model.CashLike(asset) {
				_, c := book.Held(src, asset)
				h.Cost = h.Cost.Add(c)
			}
		}
		if model.CashLike(asset) {
			h.Cost = h.Value
		}
		sort.Slice(h.Sources, func(i, j int) bool { return h.Sources[i].Source < h.Sources[j].Source })
		h.Unrealized = h.Value.Sub(h.Cost)
		s.Value, s.Cost = s.Value.Add(h.Value), s.Cost.Add(h.Cost)
		s.Assets = append(s.Assets, h)
	}
	s.Unrealized = s.Value.Sub(s.Cost)
	for i := range s.Assets {
		if s.Value.IsPositive() {
			s.Assets[i].Share = s.Assets[i].Value.Div(s.Value)
		}
	}
	sort.SliceStable(s.Assets, func(i, j int) bool {
		if c := s.Assets[i].Value.Cmp(s.Assets[j].Value); c != 0 {
			return c > 0
		}
		return s.Assets[i].Asset.Key() < s.Assets[j].Asset.Key()
	})
	for src, v := range bySource {
		s.Sources = append(s.Sources, Worth{src, v})
	}
	sort.Slice(s.Sources, func(i, j int) bool {
		if c := s.Sources[i].Amount.Cmp(s.Sources[j].Amount); c != 0 {
			return c > 0
		}
		return s.Sources[i].Source < s.Sources[j].Source
	})
	s.Unpriced = w.unpricedList()
	return s
}

func past(acts []model.Activity, at time.Time) []model.Activity {
	out := make([]model.Activity, 0, len(acts))
	for _, a := range acts {
		if !a.Time.After(at) {
			out = append(out, a)
		}
	}
	return out
}

// gains: realized with the average cost of model.Costs (a disposal for cash
// or a stablecoin realizes its value minus its cost; a swap for another
// coin carries the cost over), and rewards at their day's value.
func gains(acts []model.Activity, book *model.Book, w *walker) (realized, rewards decimal.Decimal) {
	for _, a := range acts {
		feeCounts := a.Fee != nil && crypto(a.Fee.Asset)
		switch {
		case isTrade(a):
			got, given := legs(a)
			if !crypto(got.Asset) && !crypto(given.Asset) {
				continue
			}
			feeCounts = a.Fee != nil
			if crypto(given.Asset) && model.CashLike(got.Asset) {
				net := got
				if a.Fee != nil && a.Fee.Asset.Key() == got.Asset.Key() {
					net.Qty = net.Qty.Sub(a.Fee.Qty)
				}
				realized = realized.Add(w.worth(net, a.Time)).Sub(book.Taken[a.ID])
			}
		case a.Outflow() && crypto(a.Base.Asset):
			switch {
			case a.Type == model.Fee || a.Type == model.Tax:
				realized = realized.Sub(book.Taken[a.ID])
			case w.external(a):
				realized = realized.Add(w.worth(a.Base, a.Time)).Sub(book.Taken[a.ID])
			}
		case a.Inflow() && crypto(a.Base.Asset) && a.Type != model.Deposit && a.Type != model.Adjustment:
			rewards = rewards.Add(w.worth(a.Base, a.Time))
		}
		if feeCounts {
			realized = realized.Sub(book.Fees[a.ID])
		}
	}
	return realized, rewards
}

// ---- history

// MaxPoints bounds a history: ten years of days.
const MaxPoints = 3700

// Point is the portfolio at the end of a day (UTC).
type Point struct {
	Day             time.Time
	Value, Invested decimal.Decimal
}

// Days lists the ends of each step ("day", "week" ending Sunday, "month")
// from from to to, to included; nil when there would be more than
// MaxPoints.
func Days(from, to time.Time, step string) []time.Time {
	from, to = from.UTC().Truncate(day), to.UTC().Truncate(day)
	var out []time.Time
	for d := from; !d.After(to); {
		end := d
		switch step {
		case "week":
			end = d.AddDate(0, 0, (7-int(d.Weekday()))%7)
		case "month":
			end = time.Date(d.Year(), d.Month()+1, 0, 0, 0, 0, 0, time.UTC)
		}
		if end.After(to) {
			end = to
		}
		if out = append(out, end); len(out) > MaxPoints {
			return nil
		}
		d = end.Add(day)
	}
	return out
}

// First is the day of the first activity that touches crypto (zero if none).
func First(acts []model.Activity) time.Time {
	var first time.Time
	for _, a := range acts {
		touches := crypto(a.Base.Asset) || a.Quote != nil && crypto(a.Quote.Asset)
		if touches && (first.IsZero() || a.Time.Before(first)) {
			first = a.Time
		}
	}
	return first.UTC().Truncate(day)
}

// History values what was held at the close of each of days (in order),
// with the money invested until then.
func History(acts []model.Activity, in map[string]string, price Price, days []time.Time) ([]Point, []model.Asset) {
	w := newWalker(Cached(price), in)
	order := model.ReplayOrder(acts, in)
	out := make([]Point, 0, len(days))
	i := 0
	for _, d := range days {
		end := d.Add(day)
		for ; i < len(order) && order[i].Time.Before(end); i++ {
			w.apply(order[i])
		}
		out = append(out, Point{Day: d, Value: w.value(d), Invested: w.invested})
	}
	return out, w.unpricedList()
}
