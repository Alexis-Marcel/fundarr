package bitpanda

import (
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/shopspring/decimal"
)

// What /v1/operations returns (docs.public.bitpanda.com, "List operations").
// An operation is one thing the user did or received; its transactions are
// what it booked on each of the user's wallets (wallet_owner says which:
// "shared-default" for the main wallets, "staking-service", the card,
// trading reservations, margin credit...). An amount is positive, the flow
// says which way it went; fee_amount is taken on top of it.
//
// Operation types are not documented. Those seen in the wild include buy,
// sell, swap, dust_swap, savings_plan, stock_exchange_buy/sell,
// advanced_trading_buy/sell (Fusion), deposit, withdrawal, stake, unstake,
// reward (staking), passive_earn_reward, onetime_reward, best_reward,
// trading_premium, giveaway, earn_on_fiat_reward/swap (Cash Plus),
// merger_crypto, card_debit/credit/reserve, *_reserve/*_release, ca_dividend,
// securities_tax_*, refund, reversal. So the mapping relies on what the
// transactions do rather than on names:
//
//   - Legs of the same trade (trade.trade_id) make one Trade: what came in
//     against what went out. Bitpanda takes its fee through the rate, so the
//     fiat leg already includes trade.fee: the quote is the fiat amount net
//     of it, and the fee is set apart. When the operation charges the fee in
//     a leg of its own (stocks, fee paid in BEST), that leg is the fee.
//   - Other legs are summed per asset across the user's wallets: moves
//     between wallets (staking, reservations) add up to zero and vanish.
//     What remains entered or left Bitpanda: one asset in and one out is a
//     conversion (Trade); otherwise each asset is a Deposit, Withdrawal,
//     Reward, Dividend, Fee or Tax depending on the direction and the
//     operation type.
//   - Fee and tax legs (transaction_type fee, charge, tax, levy) become the
//     fee of the operation's activity, or Fee and Tax activities.
//   - Margin credit wallets (owner "...credit"/"...debt") hold loans, not the
//     user's assets: ignored.
//
// Every activity is built so that replaying it moves balances exactly as
// the transactions did, whatever the names.

type amount struct {
	Value      string `json:"value"`
	AssetID    string `json:"asset_id"`
	CurrencyID string `json:"currency_id"`
}

type transaction struct {
	TransactionID   string  `json:"transaction_id"`
	AssetID         string  `json:"asset_id"`
	CurrencyID      string  `json:"currency_id"`
	WalletID        string  `json:"wallet_id"`
	WalletOwner     string  `json:"wallet_owner"`
	AssetAmount     *amount `json:"asset_amount"`
	FeeAmount       *amount `json:"fee_amount"`
	TransactionType string  `json:"transaction_type"`
	Flow            string  `json:"flow"` // INCOMING, OUTGOING
	CreditedAt      string  `json:"credited_at"`
	Compensates     string  `json:"compensates"`
	IndexAssetID    string  `json:"index_asset_id"`
	Trade           *struct {
		TradeID     string  `json:"trade_id"`
		Fee         *amount `json:"fee"`
		Rate        string  `json:"rate"`
		RateWithFee string  `json:"rate_with_fee"`
	} `json:"trade"`
}

type operation struct {
	OperationID   string        `json:"operation_id"`
	OperationType string        `json:"operation_type"`
	Transactions  []transaction `json:"transactions"`
}

func (t transaction) unit() unit {
	if u := unitOf(t.AssetID, t.CurrencyID); u != "" {
		return u
	}
	if t.AssetAmount != nil {
		return unitOf(t.AssetAmount.AssetID, t.AssetAmount.CurrencyID)
	}
	return ""
}

// signed is the leg's amount, negative when it left the wallet.
func (t transaction) signed() decimal.Decimal {
	if t.AssetAmount == nil {
		return decimal.Zero
	}
	v := dec(t.AssetAmount.Value)
	switch strings.ToUpper(t.Flow) {
	case "OUTGOING":
		return v.Abs().Neg()
	case "INCOMING":
		return v.Abs()
	}
	return v
}

// units lists every asset an operation mentions, to resolve them.
func (op operation) units() []unit {
	var out []unit
	add := func(a *amount) {
		if a != nil {
			if u := unitOf(a.AssetID, a.CurrencyID); u != "" {
				out = append(out, u)
			}
		}
	}
	for _, t := range op.Transactions {
		if u := t.unit(); u != "" {
			out = append(out, u)
		}
		add(t.FeeAmount)
		if t.Trade != nil {
			add(t.Trade.Fee)
		}
	}
	return out
}

// net sums amounts per unit, keeping the order units first appeared in.
type net struct {
	order []unit
	sum   map[unit]decimal.Decimal
}

func (n *net) add(u unit, d decimal.Decimal) {
	if n.sum == nil {
		n.sum = map[unit]decimal.Decimal{}
	}
	if _, ok := n.sum[u]; !ok {
		n.order = append(n.order, u)
	}
	n.sum[u] = n.sum[u].Add(d)
}

// nonZero lists what is left once moves that cancel out are gone.
func (n *net) nonZero() []unit {
	var out []unit
	for _, u := range n.order {
		if !n.sum[u].IsZero() {
			out = append(out, u)
		}
	}
	return out
}

// pair returns the only unit that came in and the only one that went out.
func (n *net) pair() (in, out unit, ok bool) {
	us := n.nonZero()
	if len(us) != 2 || n.sum[us[0]].Sign() == n.sum[us[1]].Sign() {
		return "", "", false
	}
	if n.sum[us[0]].IsPositive() {
		return us[0], us[1], true
	}
	return us[1], us[0], true
}

func debtWallet(owner string) bool {
	o := strings.ToLower(owner)
	return strings.Contains(o, "credit") || strings.Contains(o, "debt")
}

// tradeOf builds a trade from what came in and what went out: cash-like
// assets are what things are priced in.
func tradeOf(in, out model.Amount) model.Activity {
	if model.CashLike(in.Asset) && !model.CashLike(out.Asset) {
		return model.Activity{Type: model.Trade, Side: model.Sell, Base: out, Quote: &in}
	}
	return model.Activity{Type: model.Trade, Side: model.Buy, Base: in, Quote: &out}
}

// mapOperation turns one operation into activities. ok is false while it is
// pending (a transaction not credited yet): it is read again later.
func mapOperation(op operation, asset func(unit) model.Asset) (acts []model.Activity, ok bool) {
	var when time.Time
	for _, t := range op.Transactions {
		ts := parseTime(t.CreditedAt)
		if ts.IsZero() {
			return nil, false
		}
		if ts.After(when) {
			when = ts
		}
	}
	typ := strings.ToLower(strings.TrimSpace(op.OperationType))
	note := "Bitpanda " + strings.ReplaceAll(typ, "_", " ")
	amt := func(u unit, q decimal.Decimal) model.Amount { return model.Amount{Asset: asset(u), Qty: q.Abs()} }

	type trade struct {
		id      string
		net     net
		fee     decimal.Decimal
		feeUnit unit
	}
	var trades []*trade
	byTrade := map[string]*trade{}
	var rest, fees net
	separateFee := false

	for _, t := range op.Transactions {
		if debtWallet(t.WalletOwner) {
			continue
		}
		u := t.unit()
		if u == "" {
			continue
		}
		d := t.signed()
		if fa := t.FeeAmount; fa != nil {
			if q := dec(fa.Value).Abs(); q.IsPositive() {
				fu := unitOf(fa.AssetID, fa.CurrencyID)
				if fu == "" {
					fu = u
				}
				fees.add(fu, q)
			}
		}
		tt := strings.ToLower(t.TransactionType)
		switch {
		case t.Trade != nil && t.Trade.TradeID != "":
			g := byTrade[t.Trade.TradeID]
			if g == nil {
				g = &trade{id: t.Trade.TradeID}
				byTrade[g.id] = g
				trades = append(trades, g)
			}
			g.net.add(u, d)
			if f := t.Trade.Fee; f != nil && g.fee.IsZero() {
				if q := dec(f.Value).Abs(); q.IsPositive() {
					g.fee, g.feeUnit = q, unitOf(f.AssetID, f.CurrencyID)
				}
			}
		case (tt == "fee" || tt == "charge") && d.IsNegative():
			fees.add(u, d.Neg())
			separateFee = true
		case (tt == "levy" || strings.Contains(tt, "tax")) && d.IsNegative():
			acts = append(acts, model.Activity{NativeID: "tax:" + t.TransactionID, Type: model.Tax, Base: amt(u, d), Note: note + " (" + tt + ")"})
		default:
			rest.add(u, d)
		}
	}

	// Activities a fee can be attached to.
	var primary []int
	push := func(a model.Activity) {
		primary = append(primary, len(acts))
		acts = append(acts, a)
	}

	for _, g := range trades {
		in, out, ok := g.net.pair()
		if !ok { // not a plain exchange of two assets: count its legs as movements
			for _, u := range g.net.order {
				rest.add(u, g.net.sum[u])
			}
			continue
		}
		a := tradeOf(amt(in, g.net.sum[in]), amt(out, g.net.sum[out]))
		a.NativeID = "trade:" + g.id
		a.Note = note
		if !separateFee && g.fee.IsPositive() && g.feeUnit != "" {
			embedFee(&a, asset(g.feeUnit), g.fee)
		}
		push(a)
	}

	left := rest.nonZero()
	if in, out, ok := rest.pair(); ok {
		a := tradeOf(amt(in, rest.sum[in]), amt(out, rest.sum[out]))
		a.NativeID = "op:" + op.OperationID
		a.Note = note
		push(a)
	} else {
		for _, u := range left {
			d := rest.sum[u]
			push(model.Activity{NativeID: "op:" + op.OperationID + ":" + string(u), Type: classify(typ, d.IsPositive()), Base: amt(u, d), Note: note})
		}
	}

	for _, fu := range fees.nonZero() {
		f := model.Amount{Asset: asset(fu), Qty: fees.sum[fu]}
		target := -1
		if len(primary) == 1 {
			target = primary[0]
		} else {
			for _, i := range primary {
				if acts[i].Type != model.Trade && acts[i].Base.Asset.Key() == f.Asset.Key() {
					target = i
					break
				}
			}
		}
		if target >= 0 {
			a := &acts[target]
			if a.Fee == nil {
				a.Fee = &f
				continue
			}
			if a.Fee.Asset.Key() == f.Asset.Key() {
				sum := model.Amount{Asset: f.Asset, Qty: a.Fee.Qty.Add(f.Qty)}
				a.Fee = &sum
				continue
			}
		}
		acts = append(acts, model.Activity{NativeID: "fee:" + op.OperationID + ":" + string(fu), Type: model.Fee, Base: f, Note: note + " fee"})
	}

	for i := range acts {
		acts[i].Time = when
	}
	return acts, true
}

// embedFee sets apart a trade fee Bitpanda took inside one of the legs: the
// leg's amount is what moved, fee included, so the trade's own amount is
// what is left without it. A fee in neither asset of the trade is only
// informative (the fee was paid by a leg of its own): it is ignored.
func embedFee(a *model.Activity, feeAsset model.Asset, fee decimal.Decimal) {
	var leg *model.Amount
	var incoming bool
	switch feeAsset.Key() {
	case a.Quote.Asset.Key():
		leg, incoming = a.Quote, a.Side == model.Sell
	case a.Base.Asset.Key():
		leg, incoming = &a.Base, a.Side == model.Buy
	default:
		return
	}
	// Replayed, the leg moves ±qty and the fee −fee: that must equal what
	// the transactions moved.
	qty := leg.Qty.Sub(fee)
	if incoming {
		qty = leg.Qty.Add(fee)
	}
	if !qty.IsPositive() {
		return
	}
	if leg == a.Quote {
		q := model.Amount{Asset: a.Quote.Asset, Qty: qty}
		a.Quote = &q
	} else {
		a.Base.Qty = qty
	}
	a.Fee = &model.Amount{Asset: feeAsset, Qty: fee}
}

func hasAny(s string, words ...string) bool {
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

var rewardWords = []string{"reward", "cashback", "bonus", "airdrop", "giveaway", "premium", "promo", "referral", "interest", "staking", "gift"}

// classify names what entered (in) or left Bitpanda in an operation.
func classify(typ string, in bool) model.Type {
	switch {
	case hasAny(typ, "swap", "merger", "transfer", "deposit", "withdraw"):
	case !in && hasAny(typ, "tax", "levy"):
		return model.Tax
	case in && hasAny(typ, "dividend"):
		return model.Dividend
	case in && hasAny(typ, rewardWords...):
		return model.Reward
	case !in && hasAny(typ, "fee", "charge"):
		return model.Fee
	}
	if in {
		return model.Deposit
	}
	return model.Withdrawal
}

// knownType says whether the operation type is one this mapping was written
// for (others are logged, and still imported by direction).
func knownType(typ string) bool {
	t := strings.ToLower(typ)
	return hasAny(t, "buy", "sell", "swap", "saving", "deposit", "withdraw", "transfer", "stake", "merger", "card",
		"reserve", "release", "dividend", "tax", "refund", "revers", "fee", "earn") || hasAny(t, rewardWords...)
}

// ---- small helpers

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return decimal.Zero
	}
	return d
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(s))
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}
