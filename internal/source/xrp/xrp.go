// Package xrp reads an XRP Ledger account (Ledger, Xaman…) from its classic
// address: XRP and issued-currency history and balances. Nothing is signed
// and no secret is needed.
//
// Data comes from a public JSON-RPC server (account_tx, account_info,
// account_lines); every amount is taken from the transaction metadata, so
// partial payments, DEX trades and transfer fees count for what really moved.
//
// Limits of this first version:
//   - a DEX trade (OfferCreate, or an offer of the account filled by someone
//     else, or a cross-currency payment) shows as its legs: a Withdrawal of
//     what was given and a Deposit of what was received, not as a Trade;
//   - issued currencies count only from a known issuer (see official), or
//     when their code is readable and not a well-known symbol: a "USDC" from
//     anyone but Circle is spam and ignored. AMM LP tokens, MPTs and NFTs are
//     not read;
//   - XRP locked as reserve is part of the balance, as the ledger reports it.
package xrp

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// Network is what activities carry in Network.
const Network = "xrp"

// rippleEpoch is 2000-01-01T00:00:00Z, where ledger times start.
const rippleEpoch = 946684800

var xrpAsset = model.CryptoOrFiat("XRP")

// New builds the connector from its settings.
func New(cfg source.Config) (source.Source, error) {
	addr := strings.TrimSpace(cfg.Get("address"))
	if !Valid(addr) {
		return nil, errors.New("xrp: the address must be a classic XRP Ledger address (r…) with a correct checksum")
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	api := &client{base: servers[0], fallback: append([]string(nil), servers[1:]...), http: hc,
		pause: 200 * time.Millisecond, sleep: sleepCtx}
	if b := strings.TrimSpace(cfg.Get("base_url")); b != "" {
		api.base, api.fallback = b, nil
	}
	s := &Source{addr: addr, api: api}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &s.st); err != nil {
			return nil, fmt.Errorf("xrp: bad saved state: %w", err)
		}
	}
	return s, nil
}

var _ source.Tester = (*Source)(nil)

// Source is a configured account.
type Source struct {
	addr string
	api  *client
	st   state
}

type state struct {
	// Ledger of the last transaction processed: the next run starts there
	// (its transactions come again and are deduplicated by the engine).
	Ledger int64 `json:"ledger,omitempty"`
}

func (s *Source) State() []byte {
	b, _ := json.Marshal(s.st)
	return b
}

// ---- issued currencies

// official lists issuers of well-known currencies (checked on the ledger:
// each account's Domain, and gateway_balances for what it issues).
var official = map[string]struct {
	Name  string
	Codes map[string]string // currency code as on the ledger → symbol
}{
	"rMxCKbEDwqr76QuheSUMdEGf4B9xJ8m5De": {"Ripple", map[string]string{"524C555344000000000000000000000000000000": "RLUSD"}},
	"rGm7WCVp9gb4jZHWTEtGUr4dd74z2XuWhE": {"Circle", map[string]string{"5553444300000000000000000000000000000000": "USDC"}},
	"rvYAfWj5gh67oV6fW32ZzP3Aw4Eubs59B": {"Bitstamp", map[string]string{"USD": "USD", "EUR": "EUR", "GBP": "GBP",
		"CHF": "CHF", "JPY": "JPY", "AUD": "AUD", "BTC": "BTC", "ETH": "ETH"}},
}

// protected symbols only count from an official issuer.
var protected = func() map[string]bool {
	p := map[string]bool{"XRP": true, "USDT": true, "USDC": true, "RLUSD": true, "USD": true, "EUR": true, "BTC": true, "ETH": true}
	for _, o := range official {
		for _, sym := range o.Codes {
			p[sym] = true
		}
	}
	return p
}()

var spamWords = []string{"http", "www", ".com", ".io", ".net", ".org", ".xyz", ".app", "visit", "claim", "voucher", "airdrop", "reward"}

// currencyCode decodes a ledger currency: a 3-character code as is, a
// 40-hex code as its ASCII text ("" when it is not readable text, such as
// AMM LP tokens).
func currencyCode(c string) string {
	if len(c) != 40 {
		return c
	}
	b, err := hex.DecodeString(c)
	if err != nil {
		return ""
	}
	if b[0] == 0 {
		// Standard format: the code sits in bytes 12–14.
		b = b[12:15]
	} else {
		b = []byte(strings.TrimRight(string(b), "\x00"))
	}
	for _, r := range string(b) {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return ""
		}
	}
	return strings.TrimSpace(string(b))
}

// token is an issued currency as Fundarr counts it.
type token struct {
	Symbol string
	Key    string // in native ids: the symbol, with the issuer when not official
	Note   string // where it comes from, when not from an official issuer
	Spam   bool
}

// classify decides what an issued currency is, and whether it is spam.
func classify(currency, issuer string) token {
	if o, ok := official[issuer]; ok {
		if sym, ok := o.Codes[currency]; ok {
			return token{Symbol: sym, Key: sym}
		}
	}
	code := currencyCode(currency)
	sym := strings.ToUpper(code)
	if sym == "" || protected[sym] || len(sym) > 20 {
		return token{Spam: true}
	}
	text := strings.ToLower(code)
	for _, w := range spamWords {
		if strings.Contains(text, w) {
			return token{Spam: true}
		}
	}
	return token{Symbol: sym, Key: sym + "." + issuer, Note: "issued by " + issuer}
}

// ---- amounts

// amount is an XRP amount (a string of drops) or an issued one
// ({currency, issuer, value}). OK is false for what cannot be read, such as
// delivered_amount "unavailable" on transactions from before 2014.
type amount struct {
	XRP      bool
	Value    decimal.Decimal // XRP in XRP (not drops)
	Currency string
	Issuer   string
	OK       bool
}

func (a *amount) UnmarshalJSON(b []byte) error {
	*a = amount{}
	var drops string
	if json.Unmarshal(b, &drops) == nil {
		if d, err := decimal.NewFromString(drops); err == nil {
			*a = amount{XRP: true, Value: d.Shift(-6), OK: true}
		}
		return nil
	}
	var iou struct {
		Currency string `json:"currency"`
		Issuer   string `json:"issuer"`
		Value    string `json:"value"`
	}
	if json.Unmarshal(b, &iou) == nil {
		if d, err := decimal.NewFromString(iou.Value); err == nil {
			*a = amount{Value: d, Currency: iou.Currency, Issuer: iou.Issuer, OK: true}
		}
	}
	return nil
}

func drops(s string) decimal.Decimal {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return decimal.Zero
	}
	return d.Shift(-6)
}

// ---- history

type txEntry struct {
	Tx        txFields `json:"tx"`
	Meta      meta     `json:"meta"`
	Validated bool     `json:"validated"`
}

type txFields struct {
	Account     string `json:"Account"`
	Destination string `json:"Destination"`
	Type        string `json:"TransactionType"`
	Fee         string `json:"Fee"`
	Flags       uint32 `json:"Flags"`
	Amount      amount `json:"Amount"`
	Hash        string `json:"hash"`
	Date        int64  `json:"date"`
	Ledger      int64  `json:"ledger_index"`
}

type meta struct {
	Result    string                       `json:"TransactionResult"`
	Delivered amount                       `json:"delivered_amount"`
	Nodes     []map[string]json.RawMessage `json:"AffectedNodes"`
}

type node struct {
	Type     string `json:"LedgerEntryType"`
	Final    fields `json:"FinalFields"`
	New      fields `json:"NewFields"`
	Previous *struct {
		Balance *amount `json:"Balance"`
	} `json:"PreviousFields"`
}

type fields struct {
	Account   string  `json:"Account"`
	Balance   *amount `json:"Balance"`
	LowLimit  *amount `json:"LowLimit"`
	HighLimit *amount `json:"HighLimit"`
}

type txPage struct {
	Transactions []txEntry      `json:"transactions"`
	Marker       map[string]any `json:"marker"`
}

// pageSize is the transactions asked per account_tx call.
var pageSize = 200

func (s *Source) Activities(ctx context.Context, f source.Fetch) error {
	if f.Emit == nil {
		return errors.New("xrp: no Emit")
	}
	progress := f.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	from := s.st.Ledger
	if from <= 0 {
		from = -1 // the oldest ledger the server has
		progress("xrp: transactions")
	} else {
		progress("xrp: transactions from ledger %d", from)
	}
	seen := map[string]bool{}
	out := func(a model.Activity) error {
		if seen[a.NativeID] {
			return nil
		}
		seen[a.NativeID] = true
		a.Network = Network
		return f.Emit(a)
	}
	last := s.st.Ledger
	var marker map[string]any
	for {
		p := map[string]any{"account": s.addr, "forward": true, "ledger_index_min": from, "ledger_index_max": -1,
			"limit": pageSize, "api_version": 1}
		if marker != nil {
			p["marker"] = marker
		}
		var page txPage
		err := s.api.call(ctx, "account_tx", p, &page)
		if isCode(err, "actNotFound") {
			return nil // never funded: no history
		}
		if err != nil {
			return err
		}
		for _, t := range page.Transactions {
			if !t.Validated || t.Tx.Hash == "" {
				continue
			}
			if err := s.tx(t, out); err != nil {
				return err
			}
			last = max(last, t.Tx.Ledger)
		}
		if len(page.Marker) == 0 {
			break
		}
		marker = page.Marker
	}
	s.st.Ledger = last
	return nil
}

// change is how one asset of the account moved in a transaction.
type change struct {
	asset model.Asset
	key   string
	note  string
	qty   decimal.Decimal // signed: positive is received
}

// changes reads the account's balance changes from the metadata: its
// AccountRoot for XRP, its trust lines (RippleState) for issued currencies.
// Spam currencies are left out.
func (s *Source) changes(m meta) ([]change, error) {
	by := map[string]*change{}
	add := func(c change) {
		if x := by[c.key]; x != nil {
			x.qty = x.qty.Add(c.qty)
			return
		}
		by[c.key] = &c
	}
	for _, wrap := range m.Nodes {
		for kind, raw := range wrap {
			var n node
			if err := json.Unmarshal(raw, &n); err != nil {
				return nil, err
			}
			now := n.Final
			if kind == "CreatedNode" {
				now = n.New
			}
			var before *amount
			if n.Previous != nil {
				before = n.Previous.Balance
			}
			if before == nil && kind != "CreatedNode" {
				continue // balance unchanged
			}
			if now.Balance == nil || !now.Balance.OK || before != nil && !before.OK {
				continue
			}
			prev := decimal.Zero
			if before != nil {
				prev = before.Value
			}
			delta := now.Balance.Value.Sub(prev)
			switch n.Type {
			case "AccountRoot":
				if now.Account == s.addr {
					add(change{asset: xrpAsset, key: "XRP", qty: delta})
				}
			case "RippleState":
				if now.LowLimit == nil || now.HighLimit == nil {
					continue
				}
				// The balance is seen from the low account: positive when
				// the high account owes it.
				var peer string
				switch s.addr {
				case now.LowLimit.Issuer:
					peer = now.HighLimit.Issuer
				case now.HighLimit.Issuer:
					peer, delta = now.LowLimit.Issuer, delta.Neg()
				default:
					continue
				}
				tk := classify(now.Balance.Currency, peer)
				if tk.Spam {
					continue
				}
				add(change{asset: model.CryptoOrFiat(tk.Symbol), key: tk.Key, note: tk.Note, qty: delta})
			}
		}
	}
	var out []change
	for _, c := range by {
		if !c.qty.IsZero() {
			out = append(out, *c)
		}
	}
	// XRP first, then by key: legs come in a stable order.
	sort.Slice(out, func(i, j int) bool {
		if (out[i].key == "XRP") != (out[j].key == "XRP") {
			return out[i].key == "XRP"
		}
		return out[i].key < out[j].key
	})
	return out, nil
}

const tfPartialPayment = 0x00020000

func (s *Source) tx(t txEntry, out func(model.Activity) error) error {
	x := t.Tx
	when := time.Unix(x.Date+rippleEpoch, 0).UTC()
	base := model.Activity{Time: when, TxID: x.Hash}
	sender := x.Account == s.addr
	fee := model.Amount{Asset: xrpAsset}
	if sender {
		fee.Qty = drops(x.Fee)
	}
	feeOnly := func(note string) error {
		if !fee.Qty.IsPositive() {
			return nil
		}
		a := base
		a.NativeID, a.Type, a.Base, a.Note = x.Hash+":fee", model.Fee, fee, note
		return out(a)
	}
	if t.Meta.Result != "tesSUCCESS" {
		// A tec* result is in a ledger: it moved nothing but cost its fee.
		return feeOnly("fee of a failed transaction (" + t.Meta.Result + ")")
	}
	legs, err := s.changes(t.Meta)
	if err != nil {
		return fmt.Errorf("xrp %s: %w", x.Hash, err)
	}
	if sender {
		// The fee is gone from the XRP balance too: what is left of the
		// change is what the transaction itself moved.
		for i := range legs {
			if legs[i].key == "XRP" {
				legs[i].qty = legs[i].qty.Add(fee.Qty)
			}
		}
		n := legs[:0]
		for _, l := range legs {
			if !l.qty.IsZero() {
				n = append(n, l)
			}
		}
		legs = n
	}
	if len(legs) == 0 {
		return feeOnly(x.Type)
	}
	counterparty := x.Account
	if sender {
		counterparty = x.Destination
	}
	note := x.Type
	simple := x.Type == "Payment" && len(legs) == 1 && x.Destination != x.Account
	if simple {
		note = ""
		if d := t.Meta.Delivered; d.OK && x.Flags&tfPartialPayment != 0 && x.Amount.OK && d.Value.LessThan(x.Amount.Value) {
			note = "partial payment"
		}
	}
	// The fee rides on the first withdrawal (XRP first), or stands alone.
	feeAt := -1
	for i, l := range legs {
		if l.qty.IsNegative() && fee.Qty.IsPositive() {
			feeAt = i
			break
		}
	}
	for i, l := range legs {
		a := base
		a.NativeID, a.Address = x.Hash+":"+l.key, counterparty
		a.Note = strings.TrimSpace(note + " " + l.note)
		qty := l.qty.Abs()
		if simple && l.qty.IsPositive() {
			// What the payment delivered (never its Amount: partial payments
			// deliver less); the metadata says the same when it is readable.
			if d := t.Meta.Delivered; d.OK && sameAsset(d, l) {
				qty = d.Value
			}
		}
		a.Base = model.Amount{Asset: l.asset, Qty: qty}
		a.Type = model.Deposit
		if l.qty.IsNegative() {
			a.Type = model.Withdrawal
		}
		if i == feeAt {
			f := fee
			a.Fee = &f
		}
		if err := out(a); err != nil {
			return err
		}
	}
	if feeAt < 0 {
		return feeOnly(x.Type)
	}
	return nil
}

func sameAsset(a amount, c change) bool {
	if a.XRP {
		return c.key == "XRP"
	}
	tk := classify(a.Currency, a.Issuer)
	return !tk.Spam && tk.Key == c.key
}

// ---- balances

type accountInfo struct {
	Data struct {
		Balance string `json:"Balance"`
	} `json:"account_data"`
}

type linesPage struct {
	Lines []struct {
		Account  string `json:"account"` // the peer: the issuer for a holder
		Balance  string `json:"balance"`
		Currency string `json:"currency"`
	} `json:"lines"`
	Marker any `json:"marker"`
}

func (s *Source) Balances(ctx context.Context) ([]model.Balance, error) {
	var info accountInfo
	err := s.api.call(ctx, "account_info", map[string]any{"account": s.addr, "ledger_index": "validated"}, &info)
	if isCode(err, "actNotFound") {
		return nil, nil // never funded, or deleted: holds nothing
	}
	if err != nil {
		return nil, err
	}
	var out []model.Balance
	if q := drops(info.Data.Balance); q.IsPositive() {
		out = append(out, model.Balance{Amount: model.Amount{Asset: xrpAsset, Qty: q}})
	}
	sum := map[string]decimal.Decimal{}
	var order []string
	var marker any
	for {
		p := map[string]any{"account": s.addr, "ledger_index": "validated", "limit": 400}
		if marker != nil {
			p["marker"] = marker
		}
		var page linesPage
		if err := s.api.call(ctx, "account_lines", p, &page); err != nil {
			return nil, err
		}
		for _, l := range page.Lines {
			tk := classify(l.Currency, l.Account)
			q, err := decimal.NewFromString(l.Balance)
			if tk.Spam || err != nil || !q.IsPositive() {
				continue
			}
			if _, ok := sum[tk.Symbol]; !ok {
				order = append(order, tk.Symbol)
			}
			sum[tk.Symbol] = sum[tk.Symbol].Add(q)
		}
		if page.Marker == nil {
			break
		}
		marker = page.Marker
	}
	for _, sym := range order {
		out = append(out, model.Balance{Amount: model.Amount{Asset: model.CryptoOrFiat(sym), Qty: sum[sym]}})
	}
	return out, nil
}

// Test checks a server answers for the address (an unfunded one is fine).
func (s *Source) Test(ctx context.Context) error {
	var info accountInfo
	err := s.api.call(ctx, "account_info", map[string]any{"account": s.addr, "ledger_index": "validated"}, &info)
	if isCode(err, "actNotFound") {
		return nil
	}
	return err
}
