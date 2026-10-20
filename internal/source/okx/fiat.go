package okx

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

// Fiat (bank) movements are not in asset/deposit-history, which is crypto
// only, and their funding bills (types 1 and 2) are skipped in favour of the
// dedicated histories: so they come from the fiat order histories.
//
//   - GET /api/v5/fiat/deposit-order-history and withdrawal-order-history:
//     "after"/"before" are an inclusive begin/end time filter (not cursors);
//     states completed, failed, pending, canceled, inqueue, processing.
//   - GET /api/v5/fiat/buy-sell/history (begin/end): documented as only
//     available to some users; when it answers, the funding bills it
//     produces are skipped so the trade is not counted twice.
//
// These endpoints are regional: an API error there means "not available for
// this account" and is reported, not fatal.

type fiatOrder struct {
	OrdID         string `json:"ordId"`
	Ccy           string `json:"ccy"`
	Amt           string `json:"amt"`
	Fee           string `json:"fee"`
	PaymentMethod string `json:"paymentMethod"`
	State         string `json:"state"`
	CTime         string `json:"cTime"`
	UTime         string `json:"uTime"`
}

// pageByEnd walks an endpoint filtered by an inclusive [begin, end] time
// window, newest first, moving the end back below the oldest record seen.
func pageByEnd[T any](ctx context.Context, c *client, path, beginKey, endKey string, cutoff time.Time,
	ts func(T) string, id func(T) string, visit func(T) error) error {
	end := ""
	seen := map[string]bool{}
	for {
		q := url.Values{"limit": {"100"}}
		if !cutoff.IsZero() {
			q.Set(beginKey, msStr(cutoff))
		}
		if end != "" {
			q.Set(endKey, end)
		}
		var page []T
		if err := c.do(ctx, http.MethodGet, path, q, nil, &page); err != nil {
			return err
		}
		oldest := int64(0)
		for _, r := range page {
			t := msTime(ts(r)).UnixMilli()
			if oldest == 0 || t < oldest {
				oldest = t
			}
			if seen[id(r)] {
				continue
			}
			seen[id(r)] = true
			if err := visit(r); err != nil {
				return err
			}
		}
		if len(page) < 100 || oldest <= 0 {
			return nil
		}
		next := strconv.FormatInt(oldest-1, 10)
		if next == end {
			return nil
		}
		end = next
	}
}

func fiatTime(o fiatOrder) string {
	if o.UTime != "" && o.UTime != "0" {
		return o.UTime
	}
	return o.CTime
}

// optional runs a fiat history; an OKX error means the feature is not
// offered to this account and only gets a progress line.
func optional(f source.Fetch, what string, err error) error {
	var ae *APIError
	if errors.As(err, &ae) {
		progress(f, "%s not available for this account (%s)", what, ae.Msg)
		return nil
	}
	return err
}

func (s *Source) fiatDeposits(ctx context.Context, f source.Fetch, cutoff time.Time, emit func(model.Activity) error) error {
	err := pageByEnd(ctx, s.c, "/api/v5/fiat/deposit-order-history", "after", "before", cutoff,
		func(o fiatOrder) string { return o.CTime }, func(o fiatOrder) string { return o.OrdID },
		func(o fiatOrder) error {
			if o.State != "completed" {
				return nil
			}
			a := model.Activity{NativeID: "fiatdep:" + o.OrdID, Time: msTime(fiatTime(o)), Type: model.Deposit,
				Base: model.Amount{Asset: model.CryptoOrFiat(o.Ccy), Qty: dec(o.Amt)},
				TxID: o.OrdID, Note: strings.TrimSpace("OKX bank deposit " + o.PaymentMethod)}
			if fee := dec(o.Fee); fee.IsPositive() {
				a.Fee = &model.Amount{Asset: model.CryptoOrFiat(o.Ccy), Qty: fee}
			}
			return emit(a)
		})
	return optional(f, "fiat deposit history", err)
}

func (s *Source) fiatWithdrawals(ctx context.Context, f source.Fetch, cutoff time.Time, emit func(model.Activity) error) error {
	err := pageByEnd(ctx, s.c, "/api/v5/fiat/withdrawal-order-history", "after", "before", cutoff,
		func(o fiatOrder) string { return o.CTime }, func(o fiatOrder) string { return o.OrdID },
		func(o fiatOrder) error {
			if o.State != "completed" {
				return nil
			}
			a := model.Activity{NativeID: "fiatwd:" + o.OrdID, Time: msTime(fiatTime(o)), Type: model.Withdrawal,
				Base: model.Amount{Asset: model.CryptoOrFiat(o.Ccy), Qty: dec(o.Amt)},
				TxID: o.OrdID, Note: strings.TrimSpace("OKX bank withdrawal " + o.PaymentMethod)}
			if fee := dec(o.Fee); fee.IsPositive() {
				a.Fee = &model.Amount{Asset: model.CryptoOrFiat(o.Ccy), Qty: fee}
			}
			return emit(a)
		})
	return optional(f, "fiat withdrawal history", err)
}

type buySell struct {
	OrdID       string `json:"ordId"`
	State       string `json:"state"`
	FromCcy     string `json:"fromCcy"`
	ToCcy       string `json:"toCcy"`
	FillFromAmt string `json:"fillFromAmt"`
	FillToAmt   string `json:"fillToAmt"`
	CTime       string `json:"cTime"`
	UTime       string `json:"uTime"`
}

// fiatBuySell emits completed buy/sell trades and reports how many there were.
func (s *Source) fiatBuySell(ctx context.Context, f source.Fetch, cutoff time.Time, emit func(model.Activity) error) (int, error) {
	n := 0
	err := pageByEnd(ctx, s.c, "/api/v5/fiat/buy-sell/history", "begin", "end", cutoff,
		func(b buySell) string { return b.CTime }, func(b buySell) string { return b.OrdID },
		func(b buySell) error {
			if b.State != "completed" {
				return nil
			}
			n++
			when := b.UTime
			if when == "" || when == "0" {
				when = b.CTime
			}
			return emit(model.Activity{NativeID: "buysell:" + b.OrdID, Time: msTime(when), Type: model.Trade, Side: model.Buy,
				Base:  model.Amount{Asset: model.CryptoOrFiat(b.ToCcy), Qty: dec(b.FillToAmt)},
				Quote: &model.Amount{Asset: model.CryptoOrFiat(b.FromCcy), Qty: dec(b.FillFromAmt)},
				Note:  "OKX buy/sell " + strings.ToUpper(b.FromCcy) + "→" + strings.ToUpper(b.ToCcy)})
		})
	return n, optional(f, "fiat buy/sell history", err)
}

// Funding bill types of buy/sell orders, skipped when the buy/sell history
// is available: [Fiat] place / fulfill / cancel an order. ([Convert] buy/sell
// crypto/fiat, 272/273, are not documented as buy/sell bills and stay paired
// as exchanges; reconciliation would show a double count if they were.)
var buySellBillTypes = set("116", "117", "118")
