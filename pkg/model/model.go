// Package model is the common language between sources and destinations:
// every platform's history is normalized into Activities, every platform's
// holdings into Balances. Amounts are exact decimals, never floats.
//
// It also holds what is computed across sources: transfers between your
// own accounts (PairTransfers), cost basis by replaying the history with
// average cost per source, transfers carrying their cost (Books, Costs),
// and the balance check (Reconcile, Adjustments). Activities from Fundarr's
// API decode into Activity; see pkg/api.
package model

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"time"

	"github.com/shopspring/decimal"
)

// AssetKind says how an asset is priced and where it can live.
type AssetKind string

const (
	Crypto   AssetKind = "crypto"   // coins and tokens, including stablecoins
	Fiat     AssetKind = "fiat"     // EUR, USD... held as cash
	Security AssetKind = "security" // stocks, ETFs, bonds (identified by ISIN when known)
)

// Asset identifies what is held. Symbol is the platform's ticker, upper-cased
// (BTC, EUR, IWDA); ISIN is set for securities when the platform gives it.
type Asset struct {
	Kind   AssetKind `json:"kind"`
	Symbol string    `json:"symbol"`
	ISIN   string    `json:"isin,omitempty"`
	Name   string    `json:"name,omitempty"`
}

// Key identifies an asset across activities and balances.
func (a Asset) Key() string {
	if a.ISIN != "" {
		return string(a.Kind) + ":" + a.ISIN
	}
	return string(a.Kind) + ":" + a.Symbol
}

func (a Asset) String() string {
	if a.Symbol != "" {
		return a.Symbol
	}
	return a.ISIN
}

// Common fiat currencies; anything else a platform calls a currency is treated
// as crypto unless the source says otherwise.
var fiats = map[string]bool{
	"EUR": true, "USD": true, "GBP": true, "CHF": true, "JPY": true, "CAD": true, "AUD": true,
	"PLN": true, "SEK": true, "NOK": true, "DKK": true, "CZK": true, "HUF": true, "RON": true,
	"TRY": true, "BRL": true, "MXN": true, "ZAR": true, "NZD": true, "SGD": true, "HKD": true,
	"KRW": true, "INR": true, "UAH": true, "NGN": true, "ARS": true,
}

// CryptoOrFiat builds the asset for a currency code as exchanges report them.
func CryptoOrFiat(symbol string) Asset {
	s := strings.ToUpper(strings.TrimSpace(symbol))
	if fiats[s] {
		return Asset{Kind: Fiat, Symbol: s}
	}
	return Asset{Kind: Crypto, Symbol: s}
}

// Amount is a quantity of an asset. Qty is always positive; the activity type
// and side say which way it moves.
type Amount struct {
	Asset Asset           `json:"asset"`
	Qty   decimal.Decimal `json:"qty"`
}

func Amt(a Asset, qty string) Amount {
	return Amount{Asset: a, Qty: decimal.RequireFromString(qty)}
}

// Type is what happened.
type Type string

const (
	// Trade: Side of Base against Quote (buy BTC with USDT, sell IWDA for EUR,
	// a "convert", a card purchase of crypto).
	Trade Type = "trade"
	// Deposit / Withdrawal: Base enters or leaves the platform (crypto
	// transfer, bank transfer, card top-up). TxID and Network help matching
	// the other side when it is another platform or wallet you own.
	Deposit    Type = "deposit"
	Withdrawal Type = "withdrawal"
	// Reward: Base received for nothing in return (staking, savings/earn
	// interest paid in crypto, airdrop, referral, distribution).
	Reward Type = "reward"
	// Dividend: cash Base paid by the security in Quote.Asset (Quote.Qty 0).
	Dividend Type = "dividend"
	// Interest: cash interest on a fiat balance.
	Interest Type = "interest"
	// Fee: a charge not tied to another activity (custody, account fees).
	Fee Type = "fee"
	// Tax: withholding or other tax taken by the platform.
	Tax Type = "tax"
	// Adjustment: a correction Fundarr makes when the history a platform
	// gives does not add up to what it holds. Side In adds Base, Out removes
	// it. NativeID starts with AdjustPrefix.
	Adjustment Type = "adjustment"
)

// NativeID prefixes of activities that do not come from the platform.
const (
	AdjustPrefix = "adjust:" // written by Fundarr's balance check
	ManualPrefix = "manual:" // entered by the user (a reclassified adjustment)
)

type Side string

const (
	Buy  Side = "buy"
	Sell Side = "sell"
	In   Side = "in"  // adjustments only
	Out  Side = "out" //
)

// Activity is one movement on one platform, as faithful to the source as
// possible; destinations translate it into their own vocabulary.
type Activity struct {
	ID       string    `json:"id"`       // stable: hash of source and native id
	SourceID string    `json:"sourceId"` // the configured source it came from
	NativeID string    `json:"nativeId"` // the platform's own id (or a stable composite)
	Account  string    `json:"account"`  // sub-account within the platform, "" for the main one
	Time     time.Time `json:"time"`
	Type     Type      `json:"type"`
	Side     Side      `json:"side,omitempty"` // trades (buy, sell) and adjustments (in, out)

	Base  Amount  `json:"base"`            // the asset bought/sold/moved/received
	Quote *Amount `json:"quote,omitempty"` // trades: what was paid or received in exchange
	Fee   *Amount `json:"fee,omitempty"`   // fee charged with it, in any asset

	Network string `json:"network,omitempty"` // deposits/withdrawals: chain (BTC, ETH, TRX...)
	TxID    string `json:"txId,omitempty"`    // on-chain hash or bank reference
	Address string `json:"address,omitempty"`
	Note    string `json:"note,omitempty"` // human description from the platform
}

// ActivityID derives the stable id of an activity from where it came from.
func ActivityID(sourceID, nativeID string) string {
	h := sha256.Sum256([]byte(sourceID + "\x00" + nativeID))
	return hex.EncodeToString(h[:12])
}

// Balance is what a platform says is held right now.
type Balance struct {
	Account string `json:"account"` // AllAccounts: every account together
	Amount
	// Unread: the platform did not say how much of the asset this account
	// holds. Qty is then what the history adds up to, and nothing is
	// checked.
	Unread bool `json:"unread,omitempty"`
}

// Price returns the unit price of a trade (Quote per Base).
func (a Activity) Price() decimal.Decimal {
	if a.Quote == nil || a.Base.Qty.IsZero() {
		return decimal.Zero
	}
	return a.Quote.Qty.Div(a.Base.Qty)
}

// Stablecoins pegged to a fiat currency, by symbol.
var stables = map[string]string{
	"USDT": "USD", "USDC": "USD", "FDUSD": "USD", "BUSD": "USD", "TUSD": "USD", "USDP": "USD", "PAX": "USD",
	"DAI": "USD", "PYUSD": "USD", "USDE": "USD", "USD1": "USD", "USDS": "USD",
	"EURC": "EUR", "EURI": "EUR", "EUROC": "EUR", "EURT": "EUR", "EURQ": "EUR",
}

// PeggedTo returns the fiat currency a stablecoin follows ("" otherwise).
func PeggedTo(a Asset) string {
	if a.Kind != Crypto {
		return ""
	}
	return stables[a.Symbol]
}

// CashLike is true for fiat money and stablecoins: what assets are priced in.
func CashLike(a Asset) bool { return a.Kind == Fiat || PeggedTo(a) != "" }

// Inflow reports whether the activity brings its Base into the account.
func (a Activity) Inflow() bool {
	switch a.Type {
	case Deposit, Reward, Dividend, Interest:
		return true
	case Adjustment:
		return a.Side == In
	}
	return false
}

// Outflow reports whether the activity takes its Base out of the account.
func (a Activity) Outflow() bool {
	switch a.Type {
	case Withdrawal, Fee, Tax:
		return true
	case Adjustment:
		return a.Side == Out
	}
	return false
}
