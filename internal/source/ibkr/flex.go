package ibkr

import (
	"encoding/xml"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	_ "time/tzdata" // IBKR's times are New York's

	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// eastern is the time zone of the dates and times in Flex reports.
var eastern = mustZone("America/New_York")

func mustZone(name string) *time.Location {
	l, err := time.LoadLocation(name)
	if err != nil {
		panic(err)
	}
	return l
}

// ---- the XML of an Activity Flex Query (attribute names as IBKR writes them)

type flexResponse struct {
	XMLName    xml.Name        `xml:"FlexQueryResponse"`
	Statements []flexStatement `xml:"FlexStatements>FlexStatement"`
}

type flexStatement struct {
	AccountID        string          `xml:"accountId,attr"`
	FromDate         string          `xml:"fromDate,attr"`
	ToDate           string          `xml:"toDate,attr"`
	Trades           []flexTrade     `xml:"Trades>Trade"`
	CashTransactions []flexCash      `xml:"CashTransactions>CashTransaction"`
	OpenPositions    []flexPosition  `xml:"OpenPositions>OpenPosition"`
	CashReport       *flexCashReport `xml:"CashReport"`
	CorporateActions []flexCorporate `xml:"CorporateActions>CorporateAction"`
	Transfers        []flexTransfer  `xml:"Transfers>Transfer"`
}

// instrument is what every row says of the instrument it is about.
type instrument struct {
	AssetCategory string `xml:"assetCategory,attr"`
	Currency      string `xml:"currency,attr"`
	Symbol        string `xml:"symbol,attr"`
	Description   string `xml:"description,attr"`
	ISIN          string `xml:"isin,attr"`
	LevelOfDetail string `xml:"levelOfDetail,attr"`
}

type flexTrade struct {
	instrument
	TransactionID        string `xml:"transactionID,attr"`
	TradeID              string `xml:"tradeID,attr"`
	DateTime             string `xml:"dateTime,attr"`
	TradeDate            string `xml:"tradeDate,attr"`
	BuySell              string `xml:"buySell,attr"`
	Quantity             string `xml:"quantity,attr"`
	TradePrice           string `xml:"tradePrice,attr"`
	Proceeds             string `xml:"proceeds,attr"`
	Taxes                string `xml:"taxes,attr"`
	IBCommission         string `xml:"ibCommission,attr"`
	IBCommissionCurrency string `xml:"ibCommissionCurrency,attr"`
}

type flexCash struct {
	instrument
	TransactionID string `xml:"transactionID,attr"`
	Type          string `xml:"type,attr"`
	Amount        string `xml:"amount,attr"`
	DateTime      string `xml:"dateTime,attr"`
	SettleDate    string `xml:"settleDate,attr"`
}

type flexPosition struct {
	instrument
	Position string `xml:"position,attr"`
}

type flexCashReport struct {
	Currencies []flexCashCurrency `xml:"CashReportCurrency"`
}

type flexCashCurrency struct {
	Currency      string `xml:"currency,attr"`
	LevelOfDetail string `xml:"levelOfDetail,attr"`
	EndingCash    string `xml:"endingCash,attr"`
}

type flexCorporate struct {
	instrument
	TransactionID     string `xml:"transactionID,attr"`
	ActionID          string `xml:"actionID,attr"`
	Type              string `xml:"type,attr"`
	ActionDescription string `xml:"actionDescription,attr"`
	DateTime          string `xml:"dateTime,attr"`
	Quantity          string `xml:"quantity,attr"`
	Proceeds          string `xml:"proceeds,attr"`
}

type flexTransfer struct {
	instrument
	TransactionID string `xml:"transactionID,attr"`
	Type          string `xml:"type,attr"`
	Direction     string `xml:"direction,attr"`
	DateTime      string `xml:"dateTime,attr"`
	Date          string `xml:"date,attr"`
	Quantity      string `xml:"quantity,attr"`
}

// ---- parsing

// statement is one report, read for the first account it covers.
type statement struct {
	raw       flexStatement
	positions []flexPosition
	log       func(string, ...any)
}

func parse(body []byte, log func(string, ...any)) (*statement, error) {
	if log == nil {
		log = func(string, ...any) {}
	}
	var r flexResponse
	if err := xml.Unmarshal(body, &r); err != nil {
		if !strings.Contains(string(body[:min(len(body), 512)]), "<FlexQueryResponse") {
			return nil, errors.New("ibkr: the report is not XML: set the Flex Query's format to XML")
		}
		return nil, fmt.Errorf("ibkr: unreadable report: %w", err)
	}
	if len(r.Statements) == 0 {
		return nil, errors.New("ibkr: the report has no statement: check the query covers your account")
	}
	if n := len(r.Statements); n > 1 {
		log("the query covers %d accounts: only the first is read", n)
	}
	st := &statement{raw: r.Statements[0], log: log}
	for _, p := range st.raw.OpenPositions {
		if p.LevelOfDetail == "" || strings.EqualFold(p.LevelOfDetail, "SUMMARY") {
			st.positions = append(st.positions, p)
		}
	}
	return st, nil
}

// ---- small helpers

func dec(s string) decimal.Decimal {
	s = strings.ReplaceAll(strings.TrimSpace(s), ",", "")
	if s == "" || s == "-" || s == "--" {
		return decimal.Zero
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero
	}
	return d
}

// Date and time layouts a Flex Query can be set to write.
var (
	dateLayouts = []string{"20060102", "2006-01-02", "01/02/2006", "01/02/06"}
	timeLayouts = []string{"150405", "15:04:05"}
)

// flexTime reads a Flex date ("20260806") or date and time
// ("20260806;112923", "2026-08-06, 11:29:23"), New York time, into UTC.
func flexTime(s string) time.Time {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}
	}
	date, clock := s, ""
	for _, sep := range []string{";", ",", "T", " "} {
		if d, c, ok := strings.Cut(s, sep); ok {
			date, clock = strings.TrimSpace(d), strings.TrimSpace(c)
			break
		}
	}
	for _, dl := range dateLayouts {
		if clock == "" {
			if t, err := time.ParseInLocation(dl, date, eastern); err == nil {
				return t.UTC()
			}
			continue
		}
		for _, tl := range timeLayouts {
			if t, err := time.ParseInLocation(dl+" "+tl, date+" "+clock, eastern); err == nil {
				return t.UTC()
			}
		}
	}
	return time.Time{}
}

// kind says how an asset category is read: "security", "crypto", "fx"
// (a currency conversion) or "" (not supported: options, futures…).
func kind(category string) string {
	switch strings.ToUpper(category) {
	case "STK", "FUND", "BOND", "BILL", "ETF":
		return "security"
	case "CRYPTO":
		return "crypto"
	case "CASH":
		return "fx"
	}
	return ""
}

// asset is the instrument a row is about.
func (in instrument) asset() model.Asset {
	if kind(in.AssetCategory) == "crypto" {
		return model.CryptoOrFiat(in.Symbol)
	}
	return model.Asset{Kind: model.Security, Symbol: strings.ToUpper(strings.TrimSpace(in.Symbol)),
		ISIN: strings.ToUpper(strings.TrimSpace(in.ISIN)), Name: strings.TrimSpace(in.Description)}
}

func (in instrument) cash(q decimal.Decimal) model.Amount {
	return model.Amount{Asset: model.CryptoOrFiat(in.Currency), Qty: q.Abs()}
}

// detail: the row is a single transaction, not a summary of several.
func detail(level string, want ...string) bool {
	if level == "" {
		return true
	}
	for _, w := range want {
		if strings.EqualFold(level, w) {
			return true
		}
	}
	return false
}

// ---- activities

// activities emits the statement's history; n counts what it emitted.
func (st *statement) activities(emit func(model.Activity) error) (n int, err error) {
	var out []model.Activity
	skipped := map[string]int{}
	add := func(a model.Activity) {
		if a.Base.Qty.IsZero() && a.Type != model.Trade {
			return
		}
		out = append(out, a)
	}
	for _, t := range st.raw.Trades {
		if !detail(t.LevelOfDetail, "EXECUTION") {
			continue
		}
		if kind(t.AssetCategory) == "" {
			skipped[t.AssetCategory]++
			continue
		}
		st.trade(t, add)
	}
	for _, c := range st.raw.CashTransactions {
		if detail(c.LevelOfDetail, "DETAIL") {
			st.cashTx(c, add)
		}
	}
	for _, c := range st.raw.CorporateActions {
		if !detail(c.LevelOfDetail, "DETAIL") {
			continue
		}
		if kind(c.AssetCategory) == "" || kind(c.AssetCategory) == "fx" {
			skipped[c.AssetCategory]++
			continue
		}
		st.corporate(c, add)
	}
	for _, t := range st.raw.Transfers {
		if !detail(t.LevelOfDetail, "DETAIL", "TRANSACTION") {
			continue
		}
		if k := kind(t.AssetCategory); k == "" || k == "fx" {
			skipped[t.AssetCategory+" transfer"]++
			continue
		}
		st.transfer(t, add)
	}
	for cat, k := range skipped {
		st.log("skipped %d %s rows (not supported)", k, cat)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	for _, a := range out {
		if err := emit(a); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func (st *statement) trade(t flexTrade, add func(model.Activity)) {
	qty := dec(t.Quantity)
	if qty.IsZero() {
		return
	}
	id := t.TransactionID
	if id == "" {
		id = t.TradeID
	}
	id = "trade:" + id
	at := flexTime(t.DateTime)
	if at.IsZero() {
		at = flexTime(t.TradeDate)
	}
	// Signs say the side: a cancellation is the opposite of the trade.
	side := model.Buy
	if qty.IsNegative() {
		side = model.Sell
	}
	proceeds := dec(t.Proceeds)
	if proceeds.IsZero() {
		proceeds = qty.Mul(dec(t.TradePrice)).Neg()
	}
	quote := model.Amount{Asset: model.CryptoOrFiat(t.Currency), Qty: proceeds.Abs()}
	base := model.Amount{Asset: t.asset(), Qty: qty.Abs()}
	note := "IBKR " + strings.ToLower(string(side))
	if kind(t.AssetCategory) == "fx" {
		// "EUR.USD": EUR bought or sold, priced in USD.
		ccy, _, _ := strings.Cut(t.Symbol, ".")
		base.Asset = model.CryptoOrFiat(ccy)
		note = "IBKR currency conversion"
	}
	a := model.Activity{NativeID: id, Time: at, Type: model.Trade, Side: side, Base: base, Quote: &quote, Note: note}
	comm := dec(t.IBCommission)
	commCcy := t.IBCommissionCurrency
	if commCcy == "" {
		commCcy = t.Currency
	}
	switch {
	case comm.IsNegative():
		a.Fee = &model.Amount{Asset: model.CryptoOrFiat(commCcy), Qty: comm.Abs()}
	case comm.IsPositive():
		add(model.Activity{NativeID: id + ":commission", Time: at, Type: model.Reward, Note: "IBKR commission rebate",
			Base: model.Amount{Asset: model.CryptoOrFiat(commCcy), Qty: comm}})
	}
	add(a)
	switch tax := dec(t.Taxes); {
	case tax.IsNegative():
		add(model.Activity{NativeID: id + ":tax", Time: at, Type: model.Tax, Base: t.cash(tax), Note: "IBKR transaction tax"})
	case tax.IsPositive():
		add(model.Activity{NativeID: id + ":tax", Time: at, Type: model.Deposit, Base: t.cash(tax), Note: "IBKR tax refund"})
	}
}

func (st *statement) cashTx(c flexCash, add func(model.Activity)) {
	amt := dec(c.Amount)
	if amt.IsZero() {
		return
	}
	at := flexTime(c.DateTime)
	if at.IsZero() {
		at = flexTime(c.SettleDate)
	}
	typ := strings.ToLower(c.Type)
	note := "IBKR " + c.Type
	if d := strings.TrimSpace(c.Description); d != "" {
		note += ": " + d
	}
	a := model.Activity{NativeID: "cash:" + c.TransactionID, Time: at, Base: c.cash(amt), Note: note}
	bySign := func(in, out model.Type) model.Type {
		if amt.IsPositive() {
			return in
		}
		return out
	}
	switch {
	case strings.Contains(typ, "withholding"):
		a.Type = bySign(model.Deposit, model.Tax) // a refund comes back as cash
	case strings.Contains(typ, "dividend"):
		if amt.IsNegative() { // a reversal
			a.Type = model.Withdrawal
			break
		}
		a.Type = model.Dividend
		if c.Symbol != "" || c.ISIN != "" {
			a.Quote = &model.Amount{Asset: c.asset(), Qty: decimal.Zero}
		}
	case strings.Contains(typ, "interest"):
		a.Type = bySign(model.Interest, model.Fee)
	case strings.Contains(typ, "deposit"), strings.Contains(typ, "withdraw"):
		a.Type = bySign(model.Deposit, model.Withdrawal)
	case strings.Contains(typ, "fee"), strings.Contains(typ, "commission"):
		a.Type = bySign(model.Reward, model.Fee)
	default:
		st.log("cash transaction of an unknown type %q, booked as a deposit or withdrawal", c.Type)
		a.Type = bySign(model.Deposit, model.Withdrawal)
	}
	add(a)
}

// Corporate actions that hand out shares for nothing.
var freeShares = map[string]bool{"SD": true, "SO": true, "DI": true, "RI": true, "HI": true, "CO": true}

// corporate maps a corporate action. Each row moves one instrument: a
// split adds shares at no cost (the position's cost spreads over them), a
// reverse split or an exchange takes the old ones back for nothing (or the
// cash it paid) and gives the new ones on another row.
func (st *statement) corporate(c flexCorporate, add func(model.Activity)) {
	qty := dec(c.Quantity)
	if qty.IsZero() {
		return // cash-only actions (cash dividends) are expected in Cash Transactions
	}
	id := c.TransactionID
	if id == "" {
		id = c.ActionID + ":" + c.ISIN + c.Symbol
	}
	note := "IBKR corporate action " + c.Type
	if d := strings.TrimSpace(c.ActionDescription); d != "" {
		note += ": " + d
	}
	base := model.Amount{Asset: c.asset(), Qty: qty.Abs()}
	a := model.Activity{NativeID: "corp:" + id, Time: flexTime(c.DateTime), Base: base, Note: note}
	if freeShares[strings.ToUpper(c.Type)] && qty.IsPositive() {
		a.Type = model.Reward
		add(a)
		return
	}
	quote := c.cash(dec(c.Proceeds))
	a.Type, a.Side, a.Quote = model.Trade, model.Buy, &quote
	if qty.IsNegative() {
		a.Side = model.Sell
	}
	add(a)
}

func (st *statement) transfer(t flexTransfer, add func(model.Activity)) {
	qty := dec(t.Quantity)
	if qty.IsZero() {
		return
	}
	at := flexTime(t.DateTime)
	if at.IsZero() {
		at = flexTime(t.Date)
	}
	out := strings.EqualFold(t.Direction, "OUT") || (!strings.EqualFold(t.Direction, "IN") && qty.IsNegative())
	a := model.Activity{NativeID: "transfer:" + t.TransactionID, Time: at, Type: model.Deposit,
		Base: model.Amount{Asset: t.asset(), Qty: qty.Abs()}, Note: "IBKR " + t.Type + " transfer in"}
	if out {
		a.Type, a.Note = model.Withdrawal, "IBKR "+t.Type+" transfer out"
	}
	add(a)
}

// ---- balances

// balances: the open positions and the cash per currency at the end of
// the statement.
func (st *statement) balances() ([]model.Balance, error) {
	if st.raw.CashReport == nil {
		return nil, errors.New("ibkr: the report has no Cash Report: add that section to the Flex Query")
	}
	total := map[string]model.Amount{}
	add := func(a model.Asset, q decimal.Decimal) {
		cur, ok := total[a.Key()]
		if !ok {
			cur = model.Amount{Asset: a}
		}
		cur.Qty = cur.Qty.Add(q)
		total[a.Key()] = cur
	}
	skipped := 0
	for _, p := range st.positions {
		if k := kind(p.AssetCategory); k != "security" && k != "crypto" {
			skipped++
			continue
		}
		add(p.asset(), dec(p.Position))
	}
	if skipped > 0 {
		st.log("skipped %d open positions in options, futures or other derivatives (not supported)", skipped)
	}
	for _, c := range st.raw.CashReport.Currencies {
		ccy := strings.ToUpper(strings.TrimSpace(c.Currency))
		if ccy == "" || ccy == "BASE_SUMMARY" || !detail(c.LevelOfDetail, "Currency") {
			continue
		}
		add(model.CryptoOrFiat(ccy), dec(c.EndingCash))
	}
	out := make([]model.Balance, 0, len(total))
	for _, a := range total {
		out = append(out, model.Balance{Amount: a})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset.Key() < out[j].Asset.Key() })
	return out, nil
}
