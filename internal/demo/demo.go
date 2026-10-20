// Package demo makes up a portfolio for FUNDARR_DEMO=1, so apps can be
// built on the read API without real accounts: three crypto sources
// (Binance, Kraken, a Ledger) from 2023 to today, with monthly buys,
// staking, moves to the Ledger, a swap to USDC and sales for euros; and a
// Trade Republic account (its CSV export imported) with ETF savings plans
// on a compte-titres and a PEA, and a stock paying dividends.
//
// Only the activities and balances are made up; the engine and the
// portfolio code compute everything else, as for real data. Prices are a
// deterministic random walk through plausible levels, the same on every
// run, and never come from the network.
package demo

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/store"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

const day = 24 * time.Hour

// Sources, with fixed ids.
var (
	Binance = &store.Entity{ID: "9b687b4899f5", Kind: "binance", Name: "Binance"}
	Kraken  = &store.Entity{ID: "4d2e8a1c7b30", Kind: "kraken", Name: "Kraken"}
	Ledger  = &store.Entity{ID: "0130c2d4e6f8", Kind: "wallet", Name: "Ledger", Settings: map[string]string{"app": "ledger"}}
	TR      = &store.Entity{ID: "7e1a5c3b9d02", Kind: "traderepublic", Name: "Trade Republic", Settings: map[string]string{"mode": "csv"}}
)

// Securities of the Trade Republic account, by ISIN.
var (
	world  = model.Asset{Kind: model.Security, Symbol: "IE00B4L5Y983", ISIN: "IE00B4L5Y983", Name: "iShares Core MSCI World"}
	peaETF = model.Asset{Kind: model.Security, Symbol: "LU1681043599", ISIN: "LU1681043599", Name: "Amundi MSCI World (PEA)"}
	total  = model.Asset{Kind: model.Security, Symbol: "FR0000120271", ISIN: "FR0000120271", Name: "TotalEnergies"}
)

// ---- prices

// first and last day of the made-up prices.
var (
	first = time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	last  = time.Date(2031, 12, 31, 0, 0, 0, 0, time.UTC)
)

type waypoint struct {
	day   string
	price float64
}

type walk struct {
	vol    float64 // daily volatility between waypoints
	points []waypoint
}

// walks: crypto in EUR, and USD in EUR (for USDC and the USD currency).
var walks = map[string]walk{
	"BTC": {0.028, []waypoint{{"2023-01-01", 15500}, {"2023-06-15", 24000}, {"2023-10-15", 25500}, {"2024-03-12", 64000}, {"2024-08-05", 49000},
		{"2024-12-17", 102000}, {"2025-04-08", 71000}, {"2025-08-14", 106000}, {"2025-11-20", 78000}, {"2026-03-01", 72000}, {"2026-07-01", 88000},
		{"2026-10-20", 95000}, {"2028-06-01", 120000}, {"2031-12-31", 150000}}},
	"ETH": {0.035, []waypoint{{"2023-01-01", 1120}, {"2023-04-15", 1900}, {"2023-10-15", 1450}, {"2024-03-12", 3700}, {"2024-08-05", 2200},
		{"2024-12-17", 3800}, {"2025-04-08", 1400}, {"2025-08-22", 4000}, {"2025-11-20", 2600}, {"2026-03-01", 2200}, {"2026-10-20", 3300},
		{"2028-06-01", 4200}, {"2031-12-31", 5000}}},
	"SOL": {0.05, []waypoint{{"2023-01-01", 9}, {"2023-06-15", 14}, {"2023-12-25", 100}, {"2024-03-18", 180}, {"2024-08-05", 125},
		{"2024-11-22", 240}, {"2025-04-08", 95}, {"2025-09-18", 210}, {"2025-11-20", 125}, {"2026-10-20", 160}, {"2028-06-01", 200},
		{"2031-12-31", 250}}},
	"USD": {0.002, []waypoint{{"2023-01-01", 0.94}, {"2024-01-01", 0.91}, {"2025-01-01", 0.96}, {"2025-07-01", 0.86}, {"2026-10-20", 0.88},
		{"2031-12-31", 0.9}}},
	// Securities, in EUR, by ISIN.
	"IE00B4L5Y983": {0.008, []waypoint{{"2023-01-01", 72}, {"2024-01-01", 82}, {"2025-02-15", 105}, {"2025-04-08", 88}, {"2026-10-20", 118},
		{"2031-12-31", 150}}},
	"LU1681043599": {0.008, []waypoint{{"2023-01-01", 380}, {"2024-01-01", 430}, {"2025-02-15", 550}, {"2025-04-08", 460}, {"2026-10-20", 610},
		{"2031-12-31", 780}}},
	"FR0000120271": {0.012, []waypoint{{"2023-01-01", 58}, {"2023-09-15", 62}, {"2024-04-15", 68}, {"2025-01-01", 54}, {"2026-10-20", 57},
		{"2031-12-31", 60}}},
}

// walkOrder seeds each walk: the first ones as they always were.
var walkOrder = []string{"BTC", "ETH", "SOL", "USD", "IE00B4L5Y983", "LU1681043599", "FR0000120271"}

func dayIndex(t time.Time) int { return int(t.UTC().Truncate(day).Sub(first) / day) }

// series: the close of each walk on each day from first to last.
var series = sync.OnceValue(func() map[string][]decimal.Decimal {
	n := dayIndex(last) + 1
	out := map[string][]decimal.Decimal{}
	for i, sym := range walkOrder {
		w := walks[sym]
		rng := rand.New(rand.NewPCG(42, uint64(i)))
		noise := make([]float64, n)
		for d := 1; d < n; d++ {
			noise[d] = noise[d-1] + rng.NormFloat64()*w.vol
		}
		p := make([]decimal.Decimal, n)
		for k := 0; k+1 < len(w.points); k++ {
			ta, _ := time.Parse(time.DateOnly, w.points[k].day)
			tb, _ := time.Parse(time.DateOnly, w.points[k+1].day)
			a, b := dayIndex(ta), dayIndex(tb)
			la, lb := math.Log(w.points[k].price), math.Log(w.points[k+1].price)
			for d := a; d <= b; d++ {
				f := float64(d-a) / float64(b-a)
				// A bridge: the walk minus its straight line, 0 at both ends.
				bridge := noise[d] - (noise[a] + (noise[b]-noise[a])*f)
				p[d] = round(math.Exp(la + (lb-la)*f + bridge))
			}
		}
		out[sym] = p
	}
	return out
})

func round(v float64) decimal.Decimal {
	if v >= 1 {
		return decimal.NewFromFloat(v).Round(2)
	}
	return decimal.NewFromFloat(v).Round(6)
}

// Price is the made-up close of sym (BTC, ETH, SOL; USD) in EUR on t's day.
func Price(sym string, t time.Time) (decimal.Decimal, bool) {
	s, ok := series()[sym]
	i := dayIndex(t)
	if !ok || i < 0 || i >= len(s) {
		return decimal.Zero, false
	}
	return s[i], true
}

// Market answers for the price providers (price.Service.Market): Binance's
// SYMEUR and SYMUSDT markets, justETF's quotes in EUR, and the USD/EUR
// rates. Nothing else is listed, so other currencies and coins have no
// price.
func Market(pair string, from, to time.Time) map[string]decimal.Decimal {
	provider, market, _ := strings.Cut(pair, ":")
	var at func(t time.Time) (decimal.Decimal, bool)
	switch {
	case provider == "justetf" && strings.HasSuffix(market, ":EUR"):
		isin := strings.TrimSuffix(market, ":EUR")
		at = func(t time.Time) (decimal.Decimal, bool) { return Price(isin, t) }
	case provider == "fx" && market == "USDEUR":
		at = func(t time.Time) (decimal.Decimal, bool) { return Price("USD", t) }
	case provider == "fx" && market == "EURUSD":
		at = func(t time.Time) (decimal.Decimal, bool) {
			p, ok := Price("USD", t)
			if !ok {
				return p, false
			}
			return decimal.NewFromInt(1).DivRound(p, 6), true
		}
	case provider == "binance" && strings.HasSuffix(market, "EUR") && market != "USDEUR":
		sym := strings.TrimSuffix(market, "EUR")
		at = func(t time.Time) (decimal.Decimal, bool) { return Price(sym, t) }
	case provider == "binance" && strings.HasSuffix(market, "USDT") && market != "USDUSDT":
		sym := strings.TrimSuffix(market, "USDT")
		at = func(t time.Time) (decimal.Decimal, bool) {
			p, ok := Price(sym, t)
			usd, ok2 := Price("USD", t)
			if !ok || !ok2 {
				return p, false
			}
			return p.DivRound(usd, 2), true
		}
	default:
		return nil
	}
	out := map[string]decimal.Decimal{}
	for t := from.UTC().Truncate(day); !t.After(to); t = t.Add(day) {
		if p, ok := at(t); ok {
			out[t.Format(time.DateOnly)] = p
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// ---- activities

type sim struct {
	day    time.Time
	acts   []model.Activity
	held   map[string]map[string]decimal.Decimal // source → symbol → qty
	trHeld map[string]decimal.Decimal            // Trade Republic: asset key → qty
	seq    int
}

// trCashHeld: the euros of a Trade Republic account.
func (s *sim) trCashHeld(account string) decimal.Decimal {
	var mine []model.Activity
	for _, a := range s.acts {
		if a.SourceID == TR.ID && a.Account == account {
			mine = append(mine, a)
		}
	}
	return model.Holdings(mine)[eur.Key()].Qty
}

var eur = model.CryptoOrFiat("EUR")

func amt(sym string, q decimal.Decimal) model.Amount {
	return model.Amount{Asset: model.CryptoOrFiat(sym), Qty: q}
}
func ptr(a model.Amount) *model.Amount { return &a }
func dec(v string) decimal.Decimal     { return decimal.RequireFromString(v) }

func (s *sim) price(sym string) decimal.Decimal {
	p, _ := Price(sym, s.day)
	if sym == "USDC" {
		p, _ = Price("USD", s.day)
	}
	return p
}

func (s *sim) add(src *store.Entity, hour, minute int, a model.Activity) model.Activity {
	s.seq++
	a.SourceID = src.ID
	a.NativeID = fmt.Sprintf("demo-%s-%d", s.day.Format("20060102"), s.seq)
	a.ID = model.ActivityID(src.ID, a.NativeID)
	a.Time = s.day.Add(time.Duration(hour)*time.Hour + time.Duration(minute)*time.Minute)
	for k, h := range model.Holdings([]model.Activity{a}) {
		s.held[src.ID][h.Asset.Symbol] = s.held[src.ID][h.Asset.Symbol].Add(h.Qty)
		if src == TR {
			s.trHeld[k] = s.trHeld[k].Add(h.Qty)
		}
	}
	s.acts = append(s.acts, a)
	return a
}

func (s *sim) bank(src *store.Entity, euros int64) {
	s.add(src, 8, 3, model.Activity{Type: model.Deposit, Base: model.Amount{Asset: eur, Qty: decimal.NewFromInt(euros)}, Note: "SEPA transfer"})
}

// buy spends euros on sym, the fee included.
func (s *sim) buy(src *store.Entity, sym string, euros int64, feeRate string) {
	spent := decimal.NewFromInt(euros)
	fee := spent.Mul(dec(feeRate)).Round(2)
	paid := spent.Sub(fee)
	q := paid.Div(s.price(sym)).RoundDown(8)
	s.add(src, 9, 12, model.Activity{Type: model.Trade, Side: model.Buy, Base: amt(sym, q), Quote: ptr(model.Amount{Asset: eur, Qty: paid}),
		Fee: ptr(model.Amount{Asset: eur, Qty: fee})})
}

// sell sells part (0 to 1) of what src holds of sym, for euros.
func (s *sim) sell(src *store.Entity, sym, part, feeRate string) {
	q := s.held[src.ID][sym].Mul(dec(part)).RoundDown(8)
	s.sellQty(src, sym, q, feeRate)
}

func (s *sim) sellQty(src *store.Entity, sym string, q decimal.Decimal, feeRate string) {
	if !q.IsPositive() {
		return
	}
	got := q.Mul(s.price(sym)).Round(2)
	fee := got.Mul(dec(feeRate)).Round(2)
	s.add(src, 14, 37, model.Activity{Type: model.Trade, Side: model.Sell, Base: amt(sym, q), Quote: ptr(model.Amount{Asset: eur, Qty: got}),
		Fee: ptr(model.Amount{Asset: eur, Qty: fee})})
}

// swap exchanges part of src's from for to (0.1 % kept by the platform).
func (s *sim) swap(src *store.Entity, from, part, to string) {
	q := s.held[src.ID][from].Mul(dec(part)).RoundDown(8)
	got := q.Mul(s.price(from)).Mul(dec("0.999")).Div(s.price(to)).RoundDown(6)
	s.add(src, 16, 4, model.Activity{Type: model.Trade, Side: model.Buy, Base: amt(to, got), Quote: ptr(amt(from, q))})
}

// withdrawBank sends the euros src holds to the bank.
func (s *sim) withdrawBank(src *store.Entity) {
	if q := s.held[src.ID]["EUR"]; q.GreaterThan(decimal.NewFromInt(50)) {
		s.add(src, 10, 20, model.Activity{Type: model.Withdrawal, Base: model.Amount{Asset: eur, Qty: q}, Note: "SEPA transfer"})
	}
}

// move sends part of from's sym to another source, the network fee paid
// from it; both sides carry the same transaction hash.
func (s *sim) move(from, to *store.Entity, sym, part, fee, network string) {
	q := s.held[from.ID][sym].Mul(dec(part)).RoundDown(8)
	f := dec(fee)
	if q.LessThanOrEqual(f) {
		return
	}
	sent := q.Sub(f)
	h := sha256.Sum256([]byte(from.ID + to.ID + sym + s.day.String()))
	tx := hex.EncodeToString(h[:])
	if network == "ETH" {
		tx = "0x" + tx
	}
	s.add(from, 11, 30, model.Activity{Type: model.Withdrawal, Base: amt(sym, sent), Fee: ptr(amt(sym, f)), Network: network, TxID: tx})
	s.add(to, 11, 52, model.Activity{Type: model.Deposit, Base: amt(sym, sent), Network: network, TxID: tx})
}

// stake pays src rate × what it holds of sym.
func (s *sim) stake(src *store.Entity, sym, rate, note string) {
	if q := s.held[src.ID][sym].Mul(dec(rate)).RoundDown(8); q.IsPositive() {
		s.add(src, 2, 0, model.Activity{Type: model.Reward, Base: amt(sym, q), Note: note})
	}
}

// trBuy spends euros on a security, at the day's price, in an account.
func (s *sim) trBuy(account string, a model.Asset, euros int64, fee string) {
	spent := decimal.NewFromInt(euros)
	p, _ := Price(a.ISIN, s.day)
	q := spent.Div(p).RoundDown(6)
	act := model.Activity{Type: model.Trade, Side: model.Buy, Account: account, Base: model.Amount{Asset: a, Qty: q}, Quote: ptr(model.Amount{Asset: eur, Qty: q.Mul(p).Round(2)}),
		Note: "Savings plan execution"}
	if fee != "" {
		act.Fee, act.Note = ptr(model.Amount{Asset: eur, Qty: dec(fee)}), ""
	}
	s.add(TR, 9, 30, act)
}

// trCash moves euros in or out of a Trade Republic account.
func (s *sim) trCash(account string, typ model.Type, euros decimal.Decimal, note string) {
	if euros.IsPositive() {
		s.add(TR, 7, 15, model.Activity{Type: typ, Account: account, Base: model.Amount{Asset: eur, Qty: euros}, Note: note})
	}
}

// trDividend pays TotalEnergies' quarterly dividend, its tax apart.
func (s *sim) trDividend() {
	q := s.trHeld[total.Key()]
	gross := q.Mul(dec("0.79")).Round(2)
	if !gross.IsPositive() {
		return
	}
	s.add(TR, 6, 0, model.Activity{Type: model.Dividend, Base: model.Amount{Asset: eur, Qty: gross}, Quote: &model.Amount{Asset: total}})
	s.add(TR, 6, 0, model.Activity{Type: model.Tax, Base: model.Amount{Asset: eur, Qty: gross.Mul(dec("0.128")).Round(2)}, Note: "Tax"})
}

// Activities makes up the history from early 2023 to now's day.
func Activities(now time.Time) []model.Activity {
	s := &sim{held: map[string]map[string]decimal.Decimal{}, trHeld: map[string]decimal.Decimal{}}
	for _, src := range []*store.Entity{Binance, Kraken, Ledger, TR} {
		s.held[src.ID] = map[string]decimal.Decimal{}
	}
	end := now.UTC().Truncate(day)
	for s.day = first; !s.day.After(end); s.day = s.day.Add(day) {
		y, m, d := s.day.Date()
		ym := y*100 + int(m)
		switch d {
		case 1: // staking pays on the 1st
			s.stake(Binance, "ETH", "0.0028", "ETH staking")
			s.stake(Kraken, "SOL", "0.0055", "Staking")
		case 2: // coins go to the Ledger twice a year
			if (m == 1 || m == 7) && ym > 202302 {
				s.move(Binance, Ledger, "BTC", "0.9", "0.00002", "BTC")
			}
		case 3:
			if ym >= 202302 {
				s.bank(Binance, map[bool]int64{true: 350, false: 300}[y >= 2025])
			}
		case 5: // monthly buys
			if ym >= 202302 {
				s.buy(Binance, "BTC", map[bool]int64{true: 250, false: 200}[y >= 2025], "0.001")
				s.buy(Binance, "ETH", 100, "0.001")
			}
		case 8:
			if ym >= 202306 {
				s.bank(Kraken, 60)
				s.buy(Kraken, "SOL", 60, "0.0026")
			}
		case 25: // what sales left goes back to the bank
			s.withdrawBank(Binance)
			s.withdrawBank(Kraken)
		}
		if ym >= 202401 { // Trade Republic: savings plans on the CTO and the PEA
			switch d {
			case 1:
				s.trCash("", model.Interest, s.trCashHeld("").Mul(dec("0.0022")).Round(2), "Interest payment")
			case 2:
				s.trCash("", model.Deposit, decimal.NewFromInt(250), "Incoming transfer")
				s.trCash("PEA", model.Deposit, decimal.NewFromInt(300), "Incoming transfer")
			case 3:
				s.trBuy("", world, 150, "")
				s.trBuy("PEA", peaETF, 300, "")
			case 15:
				if m%3 == 1 {
					s.trBuy("", total, 200, "1")
				}
			case 28:
				if m%3 == 0 {
					s.trDividend()
				}
			}
		}
		switch s.day.Format(time.DateOnly) {
		case "2023-03-19":
			s.bank(Binance, 1500)
		case "2023-03-20":
			s.buy(Binance, "ETH", 1500, "0.001")
		case "2023-11-20": // under 305 €: the year is exempt
			s.sellQty(Kraken, "SOL", dec("250").Div(s.price("SOL")).RoundDown(4), "0.0026")
		case "2024-01-15":
			s.move(Binance, Ledger, "ETH", "0.5", "0.0004", "ETH")
		case "2024-03-14":
			s.sell(Binance, "ETH", "0.25", "0.001")
		case "2024-11-11":
			s.bank(Binance, 2000)
		case "2024-11-12":
			s.buy(Binance, "BTC", 2000, "0.001")
		case "2024-12-09":
			s.sell(Binance, "BTC", "0.4", "0.001")
		case "2025-03-01":
			s.swap(Binance, "ETH", "0.3", "USDC")
		case "2025-06-10":
			s.sell(Binance, "USDC", "0.5", "0.001")
		case "2025-11-15":
			s.sell(Kraken, "SOL", "0.4", "0.0026")
		case "2026-05-04":
			s.sell(Binance, "ETH", "0.2", "0.001")
		}
	}
	return s.acts
}

// Seed fills an empty store with the sources, their history and their
// balances, as if each had just synced at now. Trade Republic's history is
// an imported export: its balances are what it adds up to.
func Seed(ctx context.Context, st *store.Store, now time.Time) error {
	acts := Activities(now)
	for _, src := range []*store.Entity{Binance, Kraken, Ledger, TR} {
		e := *src
		e.Enabled, e.Created = true, first
		if e.Settings == nil {
			e.Settings = map[string]string{}
		}
		if err := st.PutSource(ctx, &e); err != nil {
			return err
		}
		var mine []model.Activity
		for _, a := range acts {
			if a.SourceID == e.ID {
				mine = append(mine, a)
			}
		}
		if src == TR {
			stmt := store.Statement{SourceID: e.ID, Name: "Transaction export.csv", From: mine[0].Time, To: mine[len(mine)-1].Time, Accounts: []string{"", "PEA"}}
			if err := st.ReplacePeriod(ctx, stmt, mine); err != nil {
				return err
			}
		} else if _, err := st.AddActivities(ctx, mine); err != nil {
			return err
		}
		byAccount := map[string][]model.Activity{}
		for _, a := range mine {
			byAccount[a.Account] = append(byAccount[a.Account], a)
		}
		bal := []model.Balance{}
		for acc, list := range byAccount {
			for _, h := range model.Holdings(list) {
				if h.Qty.IsPositive() {
					bal = append(bal, model.Balance{Account: acc, Amount: h})
				}
			}
		}
		sort.Slice(bal, func(i, j int) bool {
			if bal[i].Account != bal[j].Account {
				return bal[i].Account < bal[j].Account
			}
			return bal[i].Asset.Key() < bal[j].Asset.Key()
		})
		if err := st.PutBalancesFrom(ctx, e.ID, now, bal, src == TR); err != nil {
			return err
		}
		if err := st.SourceRan(ctx, e.ID, now, nil, nil); err != nil {
			return err
		}
	}
	return nil
}

// Open returns a store in memory, seeded at now: the demo never reads or
// writes a file.
func Open(ctx context.Context, now time.Time) (*store.Store, error) {
	st, err := store.Open(":memory:")
	if err != nil {
		return nil, err
	}
	if err := Seed(ctx, st, now); err != nil {
		st.Close()
		return nil, err
	}
	return st, nil
}
