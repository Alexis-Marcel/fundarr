package traderepublic

import (
	"bytes"
	"crypto/sha256"
	"encoding/csv"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// Trade Republic's transaction export (Profile → Statements → Transaction
// export, since April 2026): one line per booking, UTF-8, ISO dates and a
// decimal point. Older files separate with ";", some quote every field,
// some add a value_date column. The columns used:
//
//	datetime, date      booking time (date only on older lines: T00:00:00Z)
//	account_type        DEFAULT, JUNIOR, the PEA's (see accountLabel)
//	category, type      TRADING / CASH / CORPORATE_ACTION and ~70 types
//	asset_class, symbol ISIN for securities, ticker when asset_class is CRYPTO
//	shares, amount      signed: what moved; amount is the gross cash leg
//	fee, tax            signed, apart from amount: cash = amount + fee + tax
//	transaction_id      a stable UUID: the activity's native id
//
// The same file comes from the app, the website, and Connect's sync.

const statementHelp = "In the Trade Republic app: Profile → Statements → Transaction export → the whole period. " +
	"Import the CSV here; importing a newer one later replaces what it covers."

type row struct {
	line                      int
	time                      time.Time
	account                   string // label: "" (main), "PEA", "Junior"…
	category, typ, class      string
	name, symbol              string
	shares, price, amount     decimal.Decimal
	fee, tax                  decimal.Decimal
	ccy                       string
	origAmount, origCcy, desc string
	id                        string
	// done: settled with another line (left out); paired: a payout that
	// has its corporate action; from: the shares an exchange gave up.
	done, paired bool
	from         *row
}

func (r *row) net() decimal.Decimal { return r.amount.Add(r.fee).Add(r.tax) }

// nativeID is "tr:" and the transaction id; a line without one gets a hash
// of its content, stable across exports.
func (r *row) nativeID(raw []string) string {
	if r.id != "" {
		return "tr:" + r.id
	}
	h := sha256.Sum256([]byte(strings.Join(raw, "\x00")))
	return "tr:line:" + hex.EncodeToString(h[:10])
}

var columnNames = []string{"datetime", "date", "account_type", "category", "type", "asset_class", "name", "symbol", "shares", "price",
	"amount", "fee", "tax", "currency", "original_amount", "original_currency", "description", "transaction_id"}

// parse reads the export: rows in time order.
func parse(data []byte) ([]*row, map[int]string, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	first, _, _ := bytes.Cut(data, []byte("\n"))
	comma := ','
	if bytes.Count(first, []byte(";")) > bytes.Count(first, []byte(",")) {
		comma = ';'
	}
	r := csv.NewReader(bytes.NewReader(data))
	r.Comma = comma
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	header, err := r.Read()
	if err != nil {
		return nil, nil, errors.New("not a CSV file")
	}
	idx := map[string]int{}
	for i, h := range header {
		h = strings.ToLower(strings.Trim(strings.TrimSpace(h), "\""))
		if _, dup := idx[h]; !dup {
			idx[h] = i
		}
	}
	for _, k := range []string{"category", "type", "amount", "currency"} {
		if _, ok := idx[k]; !ok {
			return nil, nil, errors.New("this is not Trade Republic's transaction export (expected columns like datetime, category, type, amount)")
		}
	}
	if _, ok := idx["datetime"]; !ok {
		if _, ok := idx["date"]; !ok {
			return nil, nil, errors.New("this is not Trade Republic's transaction export (no date column)")
		}
	}
	raws := map[int]string{}
	var rows []*row
	for line := 2; ; line++ {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", line, err)
		}
		get := func(k string) string {
			if i, ok := idx[k]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		if get("category") == "" && get("type") == "" {
			continue
		}
		x := &row{line: line, account: accountLabel(get("account_type")), category: strings.ToUpper(get("category")), typ: strings.ToUpper(get("type")),
			class: strings.ToUpper(get("asset_class")), name: get("name"), symbol: strings.ToUpper(get("symbol")), ccy: strings.ToUpper(get("currency")),
			origAmount: get("original_amount"), origCcy: strings.ToUpper(get("original_currency")), desc: get("description"), id: get("transaction_id")}
		if x.ccy == "" {
			x.ccy = "EUR"
		}
		if x.time, err = parseTime(get("datetime"), get("date")); err != nil {
			return nil, nil, fmt.Errorf("line %d: %w", line, err)
		}
		for _, f := range []struct {
			col string
			to  *decimal.Decimal
		}{{"shares", &x.shares}, {"price", &x.price}, {"amount", &x.amount}, {"fee", &x.fee}, {"tax", &x.tax}} {
			if *f.to, err = num(get(f.col)); err != nil {
				return nil, nil, fmt.Errorf("line %d: %s: %w", line, f.col, err)
			}
		}
		vals := make([]string, 0, len(columnNames))
		for _, c := range columnNames {
			vals = append(vals, get(c))
		}
		raws[line] = strings.Join(vals, "\x00")
		rows = append(rows, x)
	}
	if len(rows) == 0 {
		return nil, nil, errors.New("the file holds no transaction")
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].time.Before(rows[j].time) })
	return rows, raws, nil
}

func parseTime(dt, d string) (time.Time, error) {
	if dt != "" {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999", "2006-01-02 15:04:05"} {
			if t, err := time.Parse(layout, dt); err == nil {
				return t.UTC(), nil
			}
		}
	}
	if d != "" {
		if t, err := time.Parse(time.DateOnly, d); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unknown date %q", dt+d)
}

func num(s string) (decimal.Decimal, error) {
	s = strings.ReplaceAll(strings.TrimSpace(s), " ", "")
	if s == "" {
		return decimal.Zero, nil
	}
	if strings.Contains(s, ",") && !strings.Contains(s, ".") {
		s = strings.ReplaceAll(s, ",", ".")
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, fmt.Errorf("bad number %q", s)
	}
	return d, nil
}

// accountLabel names the account a line belongs to: the main securities
// account is "" (a compte-titres in France, a Depot in Germany), the PEA
// (TAX_WRAPPER in Trade Republic's API) "PEA". Anything new keeps its name.
func accountLabel(t string) string {
	u := strings.ToUpper(strings.TrimSpace(t))
	switch {
	case u == "" || u == "DEFAULT":
		return ""
	case strings.Contains(u, "PEA") || strings.Contains(u, "TAX_WRAPPER"):
		return "PEA"
	case u == "JUNIOR":
		return "Junior"
	}
	return strings.ToUpper(u[:1]) + strings.ToLower(strings.ReplaceAll(u[1:], "_", " "))
}

var isinRe = regexp.MustCompile(`^[A-Z]{2}[A-Z0-9]{9}[0-9]$`)

// asset of a line: crypto by ticker, securities by ISIN.
func (r *row) asset() model.Asset {
	if r.class == "CRYPTO" {
		a := model.CryptoOrFiat(r.symbol)
		a.Name = r.name
		return a
	}
	a := model.Asset{Kind: model.Security, Symbol: r.symbol, Name: r.name}
	if isinRe.MatchString(r.symbol) {
		a.ISIN = r.symbol
	}
	if a.Symbol == "" {
		a.Symbol = strings.ToUpper(r.name)
	}
	return a
}

func (r *row) cash(q decimal.Decimal) model.Amount {
	return model.Amount{Asset: model.CryptoOrFiat(r.ccy), Qty: q.Abs()}
}

func (r *row) hasAsset() bool { return r.symbol != "" && !r.shares.IsZero() }

// note: the line's description when it says more than its type.
func (r *row) note(prefix string) string {
	d := strings.TrimSpace(r.desc)
	if d == "" || strings.EqualFold(d, r.typ+" "+r.symbol) {
		d = ""
	}
	if r.origCcy != "" && r.origCcy != r.ccy && r.origAmount != "" {
		o := strings.TrimPrefix(r.origAmount, "-")
		if d != "" {
			d += " "
		}
		d += "(" + o + " " + r.origCcy + ")"
	}
	switch {
	case prefix == "":
		return d
	case d == "":
		return prefix
	}
	return prefix + ": " + d
}

// Types of the CASH category.
var (
	dividendTypes = set("DIVIDEND", "DISTRIBUTION", "DIVIDEND_EQUIVALENT_PAYMENT", "CAPITAL_DISTRIBUTION", "ADDITIONAL_INCOME_SECURITY")
	cardTypes     = set("CARD_TRANSACTION", "CARD_TRANSACTION_INTERNATIONAL", "CARD_TRANSACTION_REFUND", "CARD_TRANSACTION_REFUND_REVERSAL",
		"CARD_ATM_WITHDRAWAL", "ATM_WITHDRAWAL")
	rewardTypes = set("BENEFITS_SAVEBACK", "BENEFITS_SPARE_CHANGE", "STOCKPERK", "PEA_MARKETING", "BONUS", "REFERRAL", "KINDERGELD_BONUS",
		"ADDITIONAL_INCOME", "COMPENSATION", "MARKETING", "CASHBACK")
	taxTypes    = set("TAX_OPTIMIZATION", "PRE_DETERMINED_TAX_BASE", "SEC_ACCOUNT", "TAX", "TAX_CORRECTION", "TAX_REFUND", "WITHHOLDING_TAX")
	feeTypes    = set("FEE", "CARD_ORDERING_FEE", "CARD_FEE", "ACCOUNT_FEE")
	payoutTypes = set("FINAL_MATURITY", "TILG", "EARLY_REDEMPTION", "LIQUIDATION_PROCEEDS", "REDEMPTION", "CALL", "KNOCK_OUT")
	// Corporate actions whose cash comes in a CASH line of payoutTypes.
	redemptionTypes = set("REDEMPTION", "FINAL_MATURITY", "WARRANT_EXERCISE", "KNOCK_OUT", "CALL", "EARLY_REDEMPTION", "LIQUIDATION")
	splitTypes      = set("SPLIT", "REVERSE_SPLIT", "STOCK_SPLIT")
)

func set(xs ...string) map[string]bool {
	m := map[string]bool{}
	for _, x := range xs {
		m[x] = true
	}
	return m
}

// statement turns the export into activities. Every line's cash effect
// (amount + fee + tax) is in them, and every change of a position, so
// replaying the activities gives what Trade Republic holds.
type statement struct {
	acts    []model.Activity
	notes   []string
	unknown map[string]int
	skipped int
	paired  int
	// outs: activities where a corporate action takes shares away (in
	// Quote for an exchange), to sweep the rounding left behind.
	outs  []outRef
	swept int
}

type outRef struct {
	i     int
	quote bool
}

func (s *statement) emit(r *row, raw string, suffix string, a model.Activity) {
	a.Time = r.time
	a.Account = r.account
	a.NativeID = r.nativeID([]string{raw})
	if suffix != "" {
		a.NativeID += ":" + suffix
	}
	s.acts = append(s.acts, a)
}

// taxOf books a line's tax apart: withheld (negative) as a Tax, refunded
// (positive) as a deposit.
func (s *statement) taxOf(r *row, raw string) {
	switch {
	case r.tax.IsNegative():
		s.emit(r, raw, "tax", model.Activity{Type: model.Tax, Base: r.cash(r.tax), Note: r.note("Tax")})
	case r.tax.IsPositive():
		s.emit(r, raw, "tax", model.Activity{Type: model.Deposit, Base: r.cash(r.tax), Note: r.note("Tax refund")})
	}
}

// byNet books what a line did to the cash: in or out, with fee and tax in it.
func (s *statement) byNet(r *row, raw string, typ model.Type, note string) {
	n := r.net()
	if n.IsZero() {
		return
	}
	if typ == "" {
		typ = model.Deposit
		if n.IsNegative() {
			typ = model.Withdrawal
		}
	}
	s.emit(r, raw, "", model.Activity{Type: typ, Base: r.cash(n), Note: note})
}

// feeOf is the line's fee as an Amount (nil when none).
func (r *row) feeOf() *model.Amount {
	if r.fee.IsZero() {
		return nil
	}
	f := r.cash(r.fee)
	return &f
}

func (s *statement) trade(r *row, raw string, side model.Side, note string) {
	q := r.cash(r.amount)
	a := model.Activity{Type: model.Trade, Side: side, Base: model.Amount{Asset: r.asset(), Qty: r.shares.Abs()}, Quote: &q, Note: note}
	if r.fee.IsNegative() {
		a.Fee = r.feeOf()
	} else if r.fee.IsPositive() { // a fee refunded: cash in
		s.emit(r, raw, "fee", model.Activity{Type: model.Deposit, Base: r.cash(r.fee), Note: r.note("Fee refund")})
	}
	s.emit(r, raw, "", a)
	s.taxOf(r, raw)
}

// assetTrade: a line that moves a position against cash, its side from
// its signs (shares in, cash out: a buy).
func (s *statement) assetTrade(r *row, raw string) {
	side := model.Buy
	if r.shares.IsNegative() || (r.shares.IsZero() && r.amount.IsPositive()) {
		side = model.Sell
	}
	s.trade(r, raw, side, r.note(""))
}

func (s *statement) row(r *row, raw string) {
	switch r.category {
	case "TRADING":
		switch {
		case r.typ == "BUY":
			s.trade(r, raw, model.Buy, r.note(""))
		case r.typ == "SELL":
			s.trade(r, raw, model.Sell, r.note(""))
		case r.hasAsset():
			s.assetTrade(r, raw)
		default:
			s.unknown[r.category+"/"+r.typ]++
			s.byNet(r, raw, "", r.note(r.typ))
		}
	case "CASH":
		s.cashRow(r, raw)
	case "CORPORATE_ACTION":
		s.corporate(r, raw)
	default:
		s.unknown[r.category+"/"+r.typ]++
		if r.hasAsset() {
			s.assetTrade(r, raw)
			return
		}
		s.byNet(r, raw, "", r.note(r.typ))
	}
}

func (s *statement) cashRow(r *row, raw string) {
	t := r.typ
	switch {
	case dividendTypes[t] || (t == "EARNINGS" && r.amount.IsPositive()) || (t == "INTEREST_PAYMENT" && r.symbol != "" && r.amount.IsPositive()):
		if r.amount.IsNegative() { // a reversal left unpaired (its original is in an earlier file)
			s.byNet(r, raw, model.Withdrawal, r.note("Dividend reversal"))
			return
		}
		quote := model.Amount{Asset: r.asset(), Qty: decimal.Zero}
		a := model.Activity{Type: model.Dividend, Base: r.cash(r.amount), Quote: &quote, Note: r.note("")}
		if r.fee.IsNegative() {
			a.Fee = r.feeOf()
		}
		s.emit(r, raw, "", a)
		s.taxOf(r, raw)
	case t == "INTEREST_PAYMENT":
		switch {
		case r.amount.IsPositive():
			s.emit(r, raw, "", model.Activity{Type: model.Interest, Base: r.cash(r.amount), Note: r.note("")})
			s.taxOf(r, raw)
			if !r.fee.IsZero() {
				s.emit(r, raw, "fee", model.Activity{Type: model.Fee, Base: r.cash(r.fee), Note: r.note("Fee")})
			}
		default: // negative interest
			s.byNet(r, raw, model.Fee, r.note("Interest"))
		}
	case cardTypes[t]:
		label := "Card"
		if strings.Contains(t, "ATM") {
			label = "Cash withdrawal"
		} else if strings.HasSuffix(t, "_REFUND") {
			label = "Card refund"
		}
		typ := model.Withdrawal
		if r.amount.IsPositive() {
			typ = model.Deposit
		}
		a := model.Activity{Type: typ, Base: r.cash(r.amount), Note: r.note(label)}
		if r.fee.IsNegative() {
			a.Fee = r.feeOf()
		}
		if r.amount.IsZero() {
			s.byNet(r, raw, "", a.Note)
			return
		}
		s.emit(r, raw, "", a)
		s.taxOf(r, raw)
	case rewardTypes[t] && r.hasAsset():
		s.emit(r, raw, "", model.Activity{Type: model.Reward, Base: model.Amount{Asset: r.asset(), Qty: r.shares.Abs()}, Note: r.note(label(t))})
		s.byNet(r, raw, "", r.note(label(t)))
	case rewardTypes[t]:
		if r.net().IsPositive() {
			s.byNet(r, raw, model.Reward, r.note(label(t)))
		} else {
			s.byNet(r, raw, model.Withdrawal, r.note(label(t)))
		}
	case taxTypes[t] || t == "EARNINGS":
		n := r.net()
		switch {
		case n.IsNegative():
			s.byNet(r, raw, model.Tax, r.note(label(t)))
		case n.IsPositive():
			s.byNet(r, raw, model.Deposit, r.note("Tax refund"))
		}
	case feeTypes[t]:
		if r.net().IsNegative() {
			s.byNet(r, raw, model.Fee, r.note(label(t)))
		} else {
			s.byNet(r, raw, model.Deposit, r.note("Fee refund"))
		}
	case payoutTypes[t] && r.symbol != "":
		// A bond or certificate paid back: the position goes for the cash.
		s.trade(r, raw, model.Sell, r.note(label(t)))
	case t == "PRIVATE_MARKET_BUY" || t == "PRIVATE_MARKET_SELL" || t == "CRYPTO_TRADING" && r.hasAsset() || t == "SAVINGS_PLAN" && r.hasAsset():
		if r.hasAsset() {
			s.assetTrade(r, raw)
			return
		}
		s.byNet(r, raw, "", r.note(label(t)))
	default:
		if r.hasAsset() { // an unknown type that moved a position
			s.unknown[r.category+"/"+t]++
			s.assetTrade(r, raw)
			return
		}
		if !knownCash[t] {
			s.unknown[r.category+"/"+t]++
		}
		n := r.net()
		typ := model.Deposit
		if n.IsNegative() {
			typ = model.Withdrawal
		}
		a := model.Activity{Type: typ, Base: r.cash(n), Note: r.note(label(t))}
		if !r.fee.IsZero() && r.amount.Sign() == n.Sign() && r.fee.IsNegative() {
			// A transfer in with a fee (a card top-up): the gross, and the fee.
			a.Base, a.Fee = r.cash(r.amount.Add(r.tax)), r.feeOf()
		}
		if n.IsZero() {
			return
		}
		s.emit(r, raw, "", a)
	}
}

// Cash types that are plain moves in or out.
var knownCash = set("CUSTOMER_INBOUND", "CUSTOMER_INPAYMENT", "CUSTOMER_INPAYMENT_REVERSAL", "CUSTOMER_OUTBOUND_REQUEST", "TRANSFER_INBOUND",
	"TRANSFER_INSTANT_INBOUND", "TRANSFER_IN", "TRANSFER_OUT", "TRANSFER_OUTBOUND", "TRANSFER_INSTANT_OUTBOUND", "VIBAN_TRANSFER_INBOUND",
	"VIBAN_TRANSFER_OUTBOUND", "TRANSFER_DIRECT_DEBIT_INBOUND", "GENERAL_INBOUND", "MANUAL_CASH_TRANSFER", "MANUAL", "GIFT", "PAYMENT",
	"INSTRUCTION", "OFFER", "EXCHANGE", "CRYPTO_TRADING", "SAVINGS_PLAN", "DIRECT_DEBIT", "SEPA_DIRECT_DEBIT")

// label names a type for people: "BENEFITS_SAVEBACK" → "Saveback".
func label(t string) string {
	switch t {
	case "BENEFITS_SAVEBACK":
		return "Saveback"
	case "BENEFITS_SPARE_CHANGE":
		return "Round-up"
	case "STOCKPERK":
		return "Stock perk"
	case "PEA_MARKETING":
		return "PEA bonus"
	case "TAX_OPTIMIZATION":
		return "Tax optimisation"
	case "PRE_DETERMINED_TAX_BASE", "EARNINGS":
		return "Tax on fund income"
	case "TILG":
		return "Redemption"
	}
	l := strings.ReplaceAll(strings.ToLower(t), "_", " ")
	if l == "" {
		return ""
	}
	return strings.ToUpper(l[:1]) + l[1:]
}

func (s *statement) corporate(r *row, raw string) {
	t := r.typ
	switch {
	case !r.hasAsset():
		s.skipped++
	case splitTypes[t]:
		// The new shares (or those taken back) cost nothing: the position's
		// cost stays, spread over more (or fewer) shares.
		zero := model.Amount{Asset: model.CryptoOrFiat(r.ccy), Qty: decimal.Zero}
		side, note := model.Buy, "Split"
		if r.shares.IsNegative() {
			side, note = model.Sell, "Reverse split"
		}
		s.emit(r, raw, "", model.Activity{Type: model.Trade, Side: side, Base: model.Amount{Asset: r.asset(), Qty: r.shares.Abs()}, Quote: &zero, Note: note})
	case r.shares.IsPositive():
		// Free receipts, stock dividends, spin-offs: shares for nothing.
		s.emit(r, raw, "", model.Activity{Type: model.Reward, Base: model.Amount{Asset: r.asset(), Qty: r.shares}, Note: r.note(label(t))})
		s.byNet(r, raw, "", r.note(label(t)))
	default:
		// Shares gone with nothing in return here (a worthless expiry, an
		// exchange whose other side is missing).
		zero := model.Amount{Asset: model.CryptoOrFiat(r.ccy), Qty: decimal.Zero}
		s.emit(r, raw, "", model.Activity{Type: model.Trade, Side: model.Sell, Base: model.Amount{Asset: r.asset(), Qty: r.shares.Abs()}, Quote: &zero,
			Note: r.note(label(t))})
		s.outs = append(s.outs, outRef{i: len(s.acts) - 1})
		s.byNet(r, raw, "", r.note(label(t)))
	}
}

// sweepDust closes a position a corporate action meant to empty (an
// exchange for new shares, a reverse split to a new ISIN, a removal) but
// left a crumb of: at most a thousandth of a share and of what it took,
// with nothing after it, is a rounding residue, not shares still held. It
// goes with the shares taken. A split of the same ISIN is left alone: what
// it leaves is the position.
func (s *statement) sweepDust() {
	limit, rel := decimal.New(1, -3), decimal.New(1, -3)
	for _, o := range s.outs {
		a := &s.acts[o.i]
		taken := &a.Base
		if o.quote {
			taken = a.Quote
		}
		key := taken.Asset.Key()
		left, later := decimal.Zero, false
		for j, b := range s.acts {
			if b.Account != a.Account {
				continue
			}
			d := model.Holdings([]model.Activity{b})[key].Qty
			if d.IsZero() {
				continue
			}
			if j > o.i {
				later = true
				break
			}
			left = left.Add(d)
		}
		if later || !left.IsPositive() || left.GreaterThan(limit) || left.GreaterThan(taken.Qty.Mul(rel)) {
			continue
		}
		taken.Qty = taken.Qty.Add(left)
		if a.Note != "" {
			a.Note += "; "
		}
		a.Note += "with the rounding left over (" + left.String() + ")"
		s.swept++
	}
}

// pair settles lines that belong together before they become activities.
func (s *statement) pair(rows []*row) {
	// A dividend booked again: Trade Republic reverses the first booking
	// (every amount negated) and books the right one. The original and its
	// reversal cancel out.
	for _, rv := range rows {
		if rv.category != "CASH" || !isDividend(rv) || !rv.amount.IsNegative() {
			continue
		}
		for _, o := range rows {
			if o == rv || o.done || o.category != "CASH" || !isDividend(o) || !o.amount.IsPositive() || o.time.After(rv.time) ||
				o.symbol != rv.symbol || o.account != rv.account {
				continue
			}
			if o.amount.Equal(rv.amount.Neg()) && o.tax.Equal(rv.tax.Neg()) && o.fee.Equal(rv.fee.Neg()) {
				o.done, rv.done = true, true
				s.paired++
				break
			}
		}
	}
	// Corporate actions that move shares against shares (a merger, an
	// ISIN change): one line out, one line in, same time and type.
	type key struct {
		t       int64
		account string
		typ     string
	}
	groups := map[key][]*row{}
	for _, r := range rows {
		// A split may come so too, when the ISIN changes.
		if r.category == "CORPORATE_ACTION" && r.hasAsset() && !redemptionTypes[r.typ] {
			k := key{r.time.Unix(), r.account, r.typ}
			groups[k] = append(groups[k], r)
		}
	}
	for _, g := range groups {
		if len(g) != 2 || g[0].shares.Sign() == g[1].shares.Sign() || g[0].symbol == g[1].symbol {
			continue
		}
		in, out := g[0], g[1]
		if in.shares.IsNegative() {
			in, out = out, in
		}
		in.from, out.done = out, true
	}
	// Bonds and certificates paid back: the corporate action removes the
	// shares, a CASH line pays (and removes them again). Keep the CASH line;
	// a corporate action with no payment means the shares went for nothing.
	for _, r := range rows {
		if r.category != "CORPORATE_ACTION" || !redemptionTypes[r.typ] || !r.shares.IsNegative() {
			continue
		}
		for _, c := range rows {
			if c.category == "CASH" && payoutTypes[c.typ] && c.symbol == r.symbol && c.account == r.account && !c.paired &&
				absDur(c.time.Sub(r.time)) <= 45*24*time.Hour {
				c.paired, r.done = true, true
				s.skipped++
				break
			}
		}
	}
}

func isDividend(r *row) bool {
	return dividendTypes[r.typ] || r.typ == "EARNINGS" && !r.amount.IsZero() || r.typ == "INTEREST_PAYMENT" && r.symbol != ""
}

func absDur(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

// Import implements source.Importer.
func (s *Source) Import(name string, data []byte, existing []model.Activity) (*source.Statement, error) {
	return importStatement(data)
}

func importStatement(data []byte) (*source.Statement, error) {
	rows, raws, err := parse(data)
	if err != nil {
		return nil, err
	}
	st := &statement{unknown: map[string]int{}}
	st.pair(rows)
	accounts := map[string]bool{}
	for _, r := range rows {
		accounts[r.account] = true
		raw := raws[r.line]
		switch {
		case r.from != nil:
			// The new shares for the old: their cost carries over.
			old := model.Amount{Asset: r.from.asset(), Qty: r.from.shares.Abs()}
			st.emit(r, raw, "", model.Activity{Type: model.Trade, Side: model.Buy, Base: model.Amount{Asset: r.asset(), Qty: r.shares.Abs()},
				Quote: &old, Note: r.note(label(r.typ))})
			st.outs = append(st.outs, outRef{i: len(st.acts) - 1, quote: true})
			st.byNet(r, raw, "", r.note(label(r.typ)))
			st.byNet(r.from, raws[r.from.line], "", r.from.note(label(r.from.typ)))
		case r.done:
		default:
			st.row(r, raw)
		}
	}
	st.sweepDust()
	out := &source.Statement{From: rows[0].time, To: rows[len(rows)-1].time, Activities: st.acts}
	for a := range accounts {
		out.Accounts = append(out.Accounts, a)
	}
	sort.Strings(out.Accounts)
	out.Notes = append(out.Notes, fmt.Sprintf("%d lines read, %d activities", len(rows), len(st.acts)))
	if st.paired > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("%d dividend(s) booked again by Trade Republic: the first booking and its reversal cancel out", st.paired))
	}
	if st.swept > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("%d position(s) closed with the rounding a corporate action left (a reverse split's)", st.swept))
	}
	if st.skipped > 0 {
		out.Notes = append(out.Notes, fmt.Sprintf("%d corporate action line(s) left out: their cash line tells the same", st.skipped))
	}
	var unknown []string
	for k, n := range st.unknown {
		unknown = append(unknown, fmt.Sprintf("type %s not recognized (%d lines): imported as cash in or out", k, n))
	}
	sort.Strings(unknown)
	out.Notes = append(out.Notes, unknown...)
	// Every line's cash must be in the activities.
	want := map[string]decimal.Decimal{}
	for _, r := range rows {
		want[r.account+"\x00"+r.ccy] = want[r.account+"\x00"+r.ccy].Add(r.net())
	}
	for k, w := range want {
		acc, ccy, _ := strings.Cut(k, "\x00")
		var mine []model.Activity
		for _, a := range st.acts {
			if a.Account == acc {
				mine = append(mine, a)
			}
		}
		got := model.Holdings(mine)[model.CryptoOrFiat(ccy).Key()].Qty
		if !got.Equal(w) {
			out.Notes = append(out.Notes, fmt.Sprintf("check: %s cash adds up to %s in the activities but %s in the file", accountName(acc), got, w))
		}
	}
	return out, nil
}

// exportCash is each account's cash in an export (label → currency →
// amount), what its lines add up to; nil when it does not read.
func exportCash(data []byte) map[string]map[string]decimal.Decimal {
	rows, _, err := parse(data)
	if err != nil {
		return nil
	}
	out := map[string]map[string]decimal.Decimal{}
	for _, r := range rows {
		if out[r.account] == nil {
			out[r.account] = map[string]decimal.Decimal{}
		}
		out[r.account][r.ccy] = out[r.account][r.ccy].Add(r.net())
	}
	return out
}

// accountName is how an account is called in messages.
func accountName(label string) string {
	if label == "" {
		return "main account"
	}
	return label
}
