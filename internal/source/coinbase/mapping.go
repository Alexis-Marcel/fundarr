package coinbase

import (
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/shopspring/decimal"
)

// tx is one entry of an account's ledger (/v2/accounts/{id}/transactions).
// amount is signed: negative when it leaves the account.
type tx struct {
	ID           string    `json:"id"`
	Type         string    `json:"type"`
	Status       string    `json:"status"`
	Amount       money     `json:"amount"`
	NativeAmount money     `json:"native_amount"`
	Description  string    `json:"description"`
	CreatedAt    time.Time `json:"created_at"`
	Network      *struct {
		Status            string `json:"status"`
		Hash              string `json:"hash"`
		NetworkName       string `json:"network_name"`
		Name              string `json:"name"` // older answers
		TransactionFee    *money `json:"transaction_fee"`
		TransactionAmount *money `json:"transaction_amount"`
	} `json:"network"`
	To   *party `json:"to"`
	From *party `json:"from"`
	// Present for their own types. Their fields are not documented; these
	// are the ones Coinbase sends (fee and totals of a buy or sell).
	Buy   *detail `json:"buy"`
	Sell  *detail `json:"sell"`
	Trade *detail `json:"trade"`
	Fill  *struct {
		FillPrice  num    `json:"fill_price"`
		ProductID  string `json:"product_id"`
		OrderID    string `json:"order_id"`
		Commission num    `json:"commission"` // in the quote currency
		OrderSide  string `json:"order_side"` // BUY, SELL
	} `json:"advanced_trade_fill"`

	account string // the account it was listed under
}

type party struct {
	ID       string `json:"id"`
	Resource string `json:"resource"` // account, user, email, bitcoin_address...
	Address  string `json:"address"`
}

type detail struct {
	ID       string `json:"id"`
	Total    *money `json:"total"`    // paid, fee included (buy); received (sell)
	Subtotal *money `json:"subtotal"` // before the fee
	Fee      *money `json:"fee"`
}

// num is a decimal sent either as a JSON string or a number.
type num string

func (n *num) UnmarshalJSON(b []byte) error {
	var s string
	if json.Unmarshal(b, &s) == nil {
		*n = num(s)
		return nil
	}
	*n = num(strings.TrimSpace(string(b)))
	return nil
}

func (n num) dec() decimal.Decimal { return dec(string(n)) }

// ---- types

// Transaction types and what they are. Anything else is skipped with a note.
var (
	rewardTypes = map[string]bool{
		"staking_reward": true, "inflation_reward": true, "interest": true, "earn_payout": true,
		"incentives_rewards_payout": true, "subscription_rebate": true, "coinbase_earn": true,
	}
	// Moves between the user's own Coinbase accounts.
	internalTypes = map[string]bool{
		"transfer": true, "staking_transfer": true, "unstaking_transfer": true, "vault_withdrawal": true,
	}
	// Moves to or from other Coinbase products not listed in /v2/accounts
	// (Coinbase Pro, International): they leave or enter what Fundarr sees.
	productTypes = map[string]bool{
		"pro_deposit": true, "pro_withdrawal": true, "exchange_deposit": true, "exchange_withdrawal": true,
		"intx_deposit": true, "intx_withdrawal": true,
	}
	// Money taken back or charged.
	chargeTypes = map[string]bool{"clawback": true, "incentives_shared_clawback": true, "subscription": true}
)

// ---- mapping

// mapper turns a run's transactions, all accounts together, into activities.
type mapper struct {
	fs  fiatSet
	own map[string]bool // the user's account ids
	now time.Time
	// held: per account, the oldest transaction not emitted yet but to be
	// read again (pending, or a trade leg waiting for its other side).
	held     map[string]time.Time
	skipped  map[string]int
	internal int
}

func (m *mapper) hold(t tx) {
	if h, ok := m.held[t.account]; !ok || t.CreatedAt.Before(h) {
		m.held[t.account] = t.CreatedAt
	}
}

func (m *mapper) amt(ccy string, q decimal.Decimal) model.Amount {
	return model.Amount{Asset: m.fs.asset(ccy), Qty: q.Abs()}
}

func (m *mapper) run(txs []tx, emit func(model.Activity) error) error {
	out := func(a model.Activity) error {
		if a.Base.Asset.Symbol == "" || a.Base.Qty.IsZero() {
			return nil
		}
		if a.Fee != nil && !a.Fee.Qty.IsPositive() {
			a.Fee = nil
		}
		return emit(a)
	}
	groups := map[string][]tx{}
	var order []string
	group := func(key string, t tx) {
		if _, ok := groups[key]; !ok {
			order = append(order, key)
		}
		groups[key] = append(groups[key], t)
	}
	for _, t := range txs {
		switch t.Status {
		case "completed":
		case "canceled", "cancelled", "failed", "expired":
			continue
		default: // pending, waiting_for_*...
			if m.now.Sub(t.CreatedAt) < pendingWindow {
				m.hold(t)
			}
			continue
		}
		switch {
		case t.Type == "buy" || t.Type == "sell":
			d := t.Buy
			if t.Type == "sell" {
				d = t.Sell
			}
			if d != nil && d.ID != "" {
				group(t.Type+":"+d.ID, t)
			} else {
				group(t.Type+":tx:"+t.ID, t)
			}
		case t.Type == "trade":
			if t.Trade != nil && t.Trade.ID != "" {
				group("convert:"+t.Trade.ID, t)
			} else {
				group("convert:tx:"+t.ID, t)
			}
		case t.Type == "advanced_trade_fill" && t.Fill != nil:
			group("fill:"+t.Fill.OrderID+":"+strconv.FormatInt(t.CreatedAt.Unix(), 10), t)
		case t.Type == "send" || t.Type == "receive":
			if err := m.send(t, out); err != nil {
				return err
			}
		case internalTypes[t.Type]:
			m.internal++
		case t.Type == "fiat_deposit" || t.Type == "fiat_withdrawal" || productTypes[t.Type]:
			if err := out(m.flow(t, "Coinbase "+strings.ReplaceAll(t.Type, "_", " "))); err != nil {
				return err
			}
		case rewardTypes[t.Type] && dec(t.Amount.Amount).IsPositive():
			if err := out(model.Activity{
				NativeID: "tx:" + t.ID, Time: t.CreatedAt, Type: model.Reward,
				Base: m.amt(t.Amount.Currency, dec(t.Amount.Amount)),
				Note: "Coinbase " + strings.ReplaceAll(t.Type, "_", " "),
			}); err != nil {
				return err
			}
		case chargeTypes[t.Type] && dec(t.Amount.Amount).IsNegative():
			if err := out(model.Activity{
				NativeID: "tx:" + t.ID, Time: t.CreatedAt, Type: model.Fee,
				Base: m.amt(t.Amount.Currency, dec(t.Amount.Amount)),
				Note: "Coinbase " + strings.ReplaceAll(t.Type, "_", " "),
			}); err != nil {
				return err
			}
		default:
			m.skipped[t.Type]++
		}
	}
	for _, key := range order {
		legs := groups[key]
		var acts []model.Activity
		var complete bool
		switch {
		case strings.HasPrefix(key, "buy:") || strings.HasPrefix(key, "sell:"):
			acts, complete = m.buySell(key, legs)
		case strings.HasPrefix(key, "convert:"):
			acts, complete = m.convert(key, legs)
		default:
			acts, complete = m.fill(key, legs)
		}
		if !complete {
			youngest := legs[0].CreatedAt
			for _, l := range legs {
				if l.CreatedAt.After(youngest) {
					youngest = l.CreatedAt
				}
			}
			if m.now.Sub(youngest) < settle {
				for _, l := range legs {
					m.hold(l)
				}
				continue
			}
		}
		if acts == nil {
			m.skipped[legs[0].Type+" (one side missing)"]++
		}
		for _, a := range acts {
			if err := out(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// flow maps a movement in or out by the sign of its amount.
func (m *mapper) flow(t tx, note string) model.Activity {
	q := dec(t.Amount.Amount)
	typ := model.Deposit
	if q.IsNegative() {
		typ = model.Withdrawal
	}
	return model.Activity{NativeID: "tx:" + t.ID, Time: t.CreatedAt, Type: typ, Base: m.amt(t.Amount.Currency, q), Note: note}
}

// send: crypto (or a send to another Coinbase user) leaving or entering.
// A send's debited amount includes the network fee.
func (m *mapper) send(t tx, out func(model.Activity) error) error {
	for _, p := range []*party{t.To, t.From} {
		if p != nil && p.Resource == "account" && m.own[p.ID] && p.ID != t.account {
			m.internal++
			return nil
		}
	}
	q := dec(t.Amount.Amount)
	a := model.Activity{NativeID: "tx:" + t.ID, Time: t.CreatedAt, Type: model.Deposit, Base: m.amt(t.Amount.Currency, q), Note: "Coinbase receive"}
	if n := t.Network; n != nil {
		a.TxID = n.Hash
		a.Network = n.NetworkName
		if a.Network == "" {
			a.Network = n.Name
		}
	}
	if q.IsNegative() {
		a.Type, a.Note = model.Withdrawal, "Coinbase send"
		if t.To != nil {
			a.Address = t.To.Address
			if t.To.Resource == "email" || t.To.Resource == "user" {
				a.Note = "Coinbase send to another Coinbase user"
			}
		}
		if n := t.Network; n != nil && n.TransactionFee != nil && sameCcy(n.TransactionFee.Currency, t.Amount.Currency) {
			if fee := dec(n.TransactionFee.Amount); fee.IsPositive() && fee.LessThan(q.Abs()) {
				a.Base.Qty = q.Abs().Sub(fee)
				a.Fee = &model.Amount{Asset: a.Base.Asset, Qty: fee}
			}
		}
	}
	return out(a)
}

func sameCcy(a, b string) bool { return a == "" || strings.EqualFold(a, b) }

// legs sums a group's legs per currency, signed.
type sums struct {
	in, out map[string]decimal.Decimal // credited, debited (as positive)
	first   tx
}

func sumLegs(legs []tx) sums {
	s := sums{in: map[string]decimal.Decimal{}, out: map[string]decimal.Decimal{}, first: legs[0]}
	for _, l := range legs {
		ccy := strings.ToUpper(l.Amount.Currency)
		q := dec(l.Amount.Amount)
		if q.IsNegative() {
			s.out[ccy] = s.out[ccy].Add(q.Neg())
		} else if q.IsPositive() {
			s.in[ccy] = s.in[ccy].Add(q)
		}
	}
	return s
}

// one returns the only currency of a side ("" when none or several).
func one(side map[string]decimal.Decimal) (string, decimal.Decimal) {
	if len(side) != 1 {
		return "", decimal.Zero
	}
	for k, v := range side {
		return k, v
	}
	return "", decimal.Zero
}

func feeIn(d *detail, ccy string) decimal.Decimal {
	if d == nil || d.Fee == nil || !strings.EqualFold(d.Fee.Currency, ccy) {
		return decimal.Zero
	}
	return dec(d.Fee.Amount)
}

// buySell: the crypto leg, and the fiat leg when paid from (or paid into)
// the Coinbase fiat balance. Paid by card or bank, there is no fiat leg: the
// money is first deposited (or, for a sell to the bank, withdrawn), so the
// replayed fiat balance stays right.
func (m *mapper) buySell(key string, legs []tx) ([]model.Activity, bool) {
	s := sumLegs(legs)
	buy := strings.HasPrefix(key, "buy:")
	var d *detail
	for _, l := range legs {
		if buy && l.Buy != nil {
			d = l.Buy
		} else if !buy && l.Sell != nil {
			d = l.Sell
		}
	}
	got, give := s.in, s.out
	if !buy {
		got, give = s.out, s.in // a sell gives up crypto and gets fiat
	}
	cry, qty := one(got)
	if cry == "" {
		return nil, false
	}
	t := s.first
	for _, l := range legs {
		if strings.EqualFold(l.Amount.Currency, cry) {
			t = l
		}
	}
	a := model.Activity{NativeID: key, Time: t.CreatedAt, Type: model.Trade, Side: model.Buy, Base: m.amt(cry, qty), Note: "Coinbase buy"}
	if !buy {
		a.Side, a.Note = model.Sell, "Coinbase sell"
	}
	if fiat, paid := one(give); fiat != "" {
		fee := feeIn(d, fiat)
		quote := paid.Sub(fee) // buy: paid includes the fee
		if !buy {
			quote = paid.Add(fee) // sell: received is net of it
		}
		a.Quote = ptr(m.amt(fiat, quote))
		a.Fee = ptr(m.amt(fiat, fee))
		return []model.Activity{a}, true
	}

	// No fiat leg: card, bank, or the leg is not there yet.
	var total model.Amount
	switch {
	case d != nil && d.Total != nil && dec(d.Total.Amount).IsPositive():
		total = m.amt(d.Total.Currency, dec(d.Total.Amount))
		fee := feeIn(d, d.Total.Currency)
		quote := total.Qty.Sub(fee)
		if !buy {
			quote = total.Qty.Add(fee)
		}
		if d.Subtotal != nil && strings.EqualFold(d.Subtotal.Currency, d.Total.Currency) && dec(d.Subtotal.Amount).IsPositive() {
			quote = dec(d.Subtotal.Amount)
		}
		a.Quote = ptr(m.amt(d.Total.Currency, quote))
		a.Fee = ptr(m.amt(d.Total.Currency, fee))
	case t.NativeAmount.Currency != "" && !dec(t.NativeAmount.Amount).IsZero():
		// Only the value in the user's currency is known: it is what was paid.
		total = m.amt(t.NativeAmount.Currency, dec(t.NativeAmount.Amount))
		a.Quote = ptr(total)
		a.Note += " (price from its value in " + total.Asset.Symbol + ")"
	default:
		return nil, false
	}
	fund := model.Activity{NativeID: key + ":funding", Time: a.Time, Type: model.Deposit, Base: total, Note: "Coinbase buy paid by card or bank"}
	if !buy {
		fund.Type, fund.Note = model.Withdrawal, "Coinbase sell paid out to bank"
	}
	return []model.Activity{fund, a}, false
}

// convert: two legs sharing the trade id, one debited, one credited. The
// conversion fee is already taken from what is received.
func (m *mapper) convert(key string, legs []tx) ([]model.Activity, bool) {
	s := sumLegs(legs)
	gotCcy, got := one(s.in)
	gaveCcy, gave := one(s.out)
	if gotCcy == "" || gaveCcy == "" || gotCcy == gaveCcy {
		return nil, false
	}
	a := model.Activity{NativeID: key, Time: s.first.CreatedAt, Type: model.Trade, Side: model.Buy,
		Base: m.amt(gotCcy, got), Quote: ptr(m.amt(gaveCcy, gave)), Note: "Coinbase convert"}
	// Price crypto in cash: converting BTC to USDC is selling BTC.
	if model.CashLike(a.Base.Asset) && !model.CashLike(a.Quote.Asset) {
		a.Side, a.Base, a.Quote = model.Sell, *a.Quote, ptr(a.Base)
	}
	return []model.Activity{a}, true
}

// fill: an Advanced Trade fill, one leg in the base currency's account and
// one in the quote's. The commission (quote currency) is taken from the quote
// leg: a buy's debit includes it, a sell's credit is net of it.
func (m *mapper) fill(key string, legs []tx) ([]model.Activity, bool) {
	f := legs[0].Fill
	baseCcy, _, _ := strings.Cut(strings.ToUpper(f.ProductID), "-")
	sell := strings.EqualFold(f.OrderSide, "SELL")
	var base, quote, comm, computed decimal.Decimal
	quoteCcy := ""
	baseLegs, quoteLegs := 0, 0
	for _, l := range legs {
		q := dec(l.Amount.Amount).Abs()
		c := l.Fill.Commission.dec().Abs()
		price := l.Fill.FillPrice.dec()
		if strings.EqualFold(l.Amount.Currency, baseCcy) {
			baseLegs++
			base = base.Add(q)
			comm = comm.Add(c)
			computed = computed.Add(q.Mul(price))
			continue
		}
		if quoteCcy != "" && !strings.EqualFold(quoteCcy, l.Amount.Currency) {
			return nil, false
		}
		quoteLegs++
		quoteCcy = strings.ToUpper(l.Amount.Currency)
		quote = quote.Add(q)
		if baseLegs == 0 && price.IsPositive() { // in case the base leg is missing
			if sell {
				computed = computed.Add(q.Add(c).Div(price))
			} else {
				computed = computed.Add(q.Sub(c).Div(price))
			}
		}
	}
	complete := baseLegs > 0 && quoteLegs > 0
	switch {
	case complete:
		if sell {
			quote = quote.Add(comm)
		} else {
			quote = quote.Sub(comm)
		}
	case baseLegs > 0: // quote leg missing: price × size
		_, quoteCcy, _ = strings.Cut(strings.ToUpper(f.ProductID), "-")
		quote = computed
	case quoteLegs > 0: // base leg missing: size from the price
		for _, l := range legs {
			comm = comm.Add(l.Fill.Commission.dec().Abs())
		}
		base = computed
		if sell {
			quote = quote.Add(comm)
		} else {
			quote = quote.Sub(comm)
		}
	}
	if base.IsZero() || quoteCcy == "" {
		return nil, complete
	}
	a := model.Activity{NativeID: key, Time: legs[0].CreatedAt, Type: model.Trade, Side: model.Buy,
		Base: m.amt(baseCcy, base), Quote: ptr(m.amt(quoteCcy, quote)), Fee: ptr(m.amt(quoteCcy, comm)),
		Note: "Coinbase Advanced Trade " + f.ProductID}
	if sell {
		a.Side = model.Sell
	}
	return []model.Activity{a}, complete
}

func ptr[T any](v T) *T { return &v }
