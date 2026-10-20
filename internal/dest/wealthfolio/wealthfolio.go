// Package wealthfolio delivers activities to a Wealthfolio server through
// its MCP endpoint (import pipeline: validate, then commit as one run).
package wealthfolio

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/dest"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

func init() {
	dest.Register(dest.Definition{
		Kind:        "wealthfolio",
		Name:        "Wealthfolio",
		Description: "Open-source portfolio tracker. Activities are imported through its MCP endpoint.",
		Fields: []source.Field{
			{Name: "url", Label: "Wealthfolio URL", Help: "e.g. https://wealthfolio.example.com"},
			{Name: "token", Label: "Access token", Secret: true, Help: "Settings → AI Agent Access → new token with accounts:read, activities:read, activities:draft, activities:write"},
			{Name: "stablecoins", Label: "Stablecoins as cash", Optional: true, Help: "yes (default): USDT, USDC… are held as USD cash and EURC as EUR, so crypto bought with them has a price. no: they are crypto assets"},
		},
		Setup: "Enable the MCP endpoint on the Wealthfolio server (WF_MCP_ENABLED=true), then create an access token in Settings → AI Agent Access. Create one Wealthfolio account per source (e.g. \"Binance\", tracking mode Transactions) and map each source to it.",
		New: func(cfg source.Config) (dest.Destination, error) {
			u := strings.TrimRight(strings.TrimSpace(cfg.Get("url")), "/")
			if u == "" || cfg.Get("token") == "" {
				return nil, errors.New("URL and access token are required")
			}
			client := cfg.HTTP
			if client == nil {
				client = &http.Client{Timeout: 2 * time.Minute}
			}
			return &Wealthfolio{
				mcp:         &mcpClient{url: u + "/mcp", token: cfg.Get("token"), http: client},
				stableCash:  !strings.EqualFold(strings.TrimSpace(cfg.Get("stablecoins")), "no"),
				accountCcys: map[string]string{},
			}, nil
		},
	})
}

type Wealthfolio struct {
	mcp         *mcpClient
	stableCash  bool
	accountCcys map[string]string
}

func (w *Wealthfolio) Accounts(ctx context.Context) ([]dest.Account, error) {
	var out struct {
		Accounts []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Currency string `json:"currency"`
			IsActive bool   `json:"isActive"`
		} `json:"accounts"`
	}
	if err := w.mcp.call(ctx, "get_accounts", map[string]any{}, &out); err != nil {
		return nil, err
	}
	var accs []dest.Account
	for _, a := range out.Accounts {
		w.accountCcys[a.ID] = a.Currency
		if a.IsActive {
			accs = append(accs, dest.Account{ID: a.ID, Name: a.Name, Currency: a.Currency})
		}
	}
	return accs, nil
}

// row is one line of Wealthfolio's import (ActivityImportRow).
type row struct {
	Date           string   `json:"date"`
	ActivityType   string   `json:"activityType"`
	Currency       string   `json:"currency"`
	Symbol         string   `json:"symbol,omitempty"`
	InstrumentType string   `json:"instrumentType,omitempty"`
	ISIN           string   `json:"isin,omitempty"`
	Quantity       *float64 `json:"quantity,omitempty"`
	UnitPrice      *float64 `json:"unitPrice,omitempty"`
	Amount         *float64 `json:"amount,omitempty"`
	Fee            *float64 `json:"fee,omitempty"`
	AccountID      string   `json:"accountId"`
	Comment        string   `json:"comment,omitempty"`
	LineNumber     int      `json:"lineNumber"`
	// Fundarr records what it delivered; Wealthfolio's own duplicate check
	// (by content) would drop genuine repeats such as two equal buys a day.
	ForceImport bool `json:"forceImport"`

	activity int // index in the batch
}

func num(d decimal.Decimal) *float64 {
	f := d.InexactFloat64()
	return &f
}

// cash returns the currency an asset counts as cash in, or "" if it is not cash.
func (w *Wealthfolio) cash(a model.Asset) string {
	if a.Kind == model.Fiat {
		return a.Symbol
	}
	if w.stableCash {
		return model.PeggedTo(a)
	}
	return ""
}

func instrument(a model.Asset) string {
	if a.Kind == model.Crypto {
		return "CRYPTO"
	}
	return ""
}

// extra is what Fundarr knows about an activity beyond the activity itself.
type extra struct {
	cost    decimal.Decimal // what the incoming asset cost, in the account currency
	hasCost bool
	peer    string // the other side of a transfer between your accounts
}

// translate turns one activity into import rows. ccy is the account currency,
// used for rows that move units rather than cash.
func (w *Wealthfolio) translate(a model.Activity, account, ccy string, x extra) ([]row, error) {
	date := a.Time.UTC().Format(time.RFC3339)
	comment := "fundarr:" + a.ID
	switch {
	case x.peer != "" && a.Inflow():
		comment += " transfer from " + x.peer
	case x.peer != "":
		comment += " transfer to " + x.peer
	case a.Type == model.Adjustment:
		comment += " balance correction (Fundarr):"
	}
	if a.Note != "" {
		comment += " " + a.Note
	}
	base := func(t string) row {
		return row{Date: date, ActivityType: t, AccountID: account, Comment: comment, ForceImport: true}
	}
	cashRow := func(t string, amt model.Amount) row {
		r := base(t)
		r.Currency = w.cash(amt.Asset)
		r.Amount = num(amt.Qty)
		return r
	}
	unitsRow := func(t string, amt model.Amount) row {
		r := base(t)
		r.Currency = ccy
		r.Symbol = amt.Asset.Symbol
		r.ISIN = amt.Asset.ISIN
		r.InstrumentType = instrument(amt.Asset)
		r.Quantity = num(amt.Qty)
		return r
	}
	// in/out moves an amount in or out of the account, as cash or as units.
	// Units coming in are given their cost (Wealthfolio's cost basis) when
	// Fundarr knows it.
	move := func(in bool, amt model.Amount) row {
		if w.cash(amt.Asset) != "" {
			if in {
				return cashRow("DEPOSIT", amt)
			}
			return cashRow("WITHDRAWAL", amt)
		}
		if in {
			r := unitsRow("TRANSFER_IN", amt)
			if x.hasCost && amt.Qty.IsPositive() {
				r.UnitPrice = num(x.cost.Div(amt.Qty))
			}
			return r
		}
		return unitsRow("TRANSFER_OUT", amt)
	}
	// A fee in an asset other than the trade's cash leaves as its own row.
	feeRow := func() []row {
		if a.Fee == nil || a.Fee.Qty.IsZero() {
			return nil
		}
		if w.cash(a.Fee.Asset) != "" {
			return []row{cashRow("FEE", *a.Fee)}
		}
		return []row{unitsRow("TRANSFER_OUT", *a.Fee)}
	}

	switch a.Type {
	case model.Trade:
		if a.Quote == nil {
			return nil, errors.New("trade without a counter amount")
		}
		baseCash, quoteCash := w.cash(a.Base.Asset), w.cash(a.Quote.Asset)
		switch {
		case baseCash == "" && quoteCash != "":
			// The usual case: an asset bought or sold for cash.
			t := "BUY"
			if a.Side == model.Sell {
				t = "SELL"
			}
			r := unitsRow(t, a.Base)
			r.Currency = quoteCash
			r.UnitPrice = num(a.Price())
			rows := []row{r}
			if a.Fee != nil && !a.Fee.Qty.IsZero() && w.cash(a.Fee.Asset) == quoteCash {
				rows[0].Fee = num(a.Fee.Qty)
			} else if a.Fee != nil && a.Fee.Asset.Key() == a.Base.Asset.Key() && a.Side == model.Buy {
				// Fee taken from what was bought: fewer units arrive.
				rows[0].Quantity = num(a.Base.Qty.Sub(a.Fee.Qty))
				rows[0].UnitPrice = num(a.Quote.Qty.Div(a.Base.Qty.Sub(a.Fee.Qty)))
			} else {
				rows = append(rows, feeRow()...)
			}
			return rows, nil
		default:
			// Cash for cash (a currency conversion) or asset for asset: the
			// two legs move as transfers, which keeps quantities right.
			in, out := a.Base, *a.Quote
			if a.Side == model.Sell {
				in, out = *a.Quote, a.Base
			}
			rows := []row{move(false, out), move(true, in)}
			if a.Fee != nil && a.Fee.Asset.Key() == in.Asset.Key() {
				rows[1] = move(true, model.Amount{Asset: in.Asset, Qty: in.Qty.Sub(a.Fee.Qty)})
			} else {
				rows = append(rows, feeRow()...)
			}
			return rows, nil
		}
	case model.Deposit:
		return append([]row{move(true, a.Base)}, feeRow()...), nil
	case model.Withdrawal:
		return append([]row{move(false, a.Base)}, feeRow()...), nil
	case model.Reward:
		if w.cash(a.Base.Asset) != "" {
			return []row{cashRow("INTEREST", a.Base)}, nil
		}
		// Units received for free arrive as a transfer, at their market value
		// that day when Fundarr knows it.
		return []row{move(true, a.Base)}, nil
	case model.Adjustment:
		return []row{move(a.Side == model.In, a.Base)}, nil
	case model.Interest:
		return []row{cashRow("INTEREST", a.Base)}, nil
	case model.Dividend:
		r := cashRow("DIVIDEND", a.Base)
		if a.Quote != nil {
			r.Symbol, r.ISIN, r.InstrumentType = a.Quote.Asset.Symbol, a.Quote.Asset.ISIN, instrument(a.Quote.Asset)
		}
		return append([]row{r}, feeRow()...), nil
	case model.Fee, model.Tax:
		t := "FEE"
		if a.Type == model.Tax {
			t = "TAX"
		}
		if w.cash(a.Base.Asset) != "" {
			return []row{cashRow(t, a.Base)}, nil
		}
		return []row{unitsRow("TRANSFER_OUT", a.Base)}, nil
	}
	return nil, fmt.Errorf("activity type %q is not supported", a.Type)
}

type importRow struct {
	LineNumber  int      `json:"lineNumber"`
	IsValid     bool     `json:"isValid"`
	IsDuplicate bool     `json:"isDuplicate"`
	Errors      []string `json:"errors"`
}

type importOutput struct {
	ImportRunID string `json:"importRunId"`
	Summary     struct {
		Imported     int    `json:"imported"`
		Duplicates   int    `json:"duplicates"`
		Success      bool   `json:"success"`
		ErrorMessage string `json:"errorMessage"`
	} `json:"summary"`
	Failed []importRow `json:"failed"`
	Rows   []importRow `json:"rows"`
}

func (w *Wealthfolio) Push(ctx context.Context, b dest.Batch) ([]dest.Result, error) {
	ccy, ok := w.accountCcys[b.Account]
	if !ok {
		if _, err := w.Accounts(ctx); err != nil {
			return nil, err
		}
		if ccy, ok = w.accountCcys[b.Account]; !ok {
			return nil, fmt.Errorf("Wealthfolio account %s does not exist (anymore)", b.Account)
		}
	}
	results := make([]dest.Result, len(b.Activities))
	var rows []row
	for i, a := range b.Activities {
		results[i] = dest.Result{ActivityID: a.ID, Status: dest.Delivered}
		x := extra{peer: b.Transfers[a.ID]}
		x.cost, x.hasCost = b.Costs[a.ID]
		rs, err := w.translate(a, b.Account, ccy, x)
		if err != nil {
			results[i] = dest.Result{ActivityID: a.ID, Status: dest.Skipped, Message: err.Error()}
			continue
		}
		for _, r := range rs {
			r.activity = i
			rows = append(rows, r)
		}
	}
	if len(rows) == 0 {
		return results, nil
	}
	// The commit imports nothing if one row is invalid: validate first and
	// leave out the activities with a rejected row.
	for len(rows) > 0 {
		for j := range rows {
			rows[j].LineNumber = j + 1
		}
		var prep importOutput
		if err := w.mcp.call(ctx, "prepare_activity_import", map[string]any{"activities": rows}, &prep); err != nil {
			return results[:0], err
		}
		bad := map[int]string{}
		for _, r := range prep.Rows {
			if !r.IsValid || len(r.Errors) > 0 {
				if r.LineNumber >= 1 && r.LineNumber <= len(rows) {
					bad[rows[r.LineNumber-1].activity] = strings.Join(r.Errors, "; ")
				}
			}
		}
		if len(bad) == 0 {
			break
		}
		kept := rows[:0]
		for _, r := range rows {
			if msg, isBad := bad[r.activity]; isBad {
				if msg == "" {
					msg = "rejected by Wealthfolio"
				}
				results[r.activity] = dest.Result{ActivityID: b.Activities[r.activity].ID, Status: dest.Failed, Message: msg}
				continue
			}
			kept = append(kept, r)
		}
		rows = kept
	}
	if len(rows) == 0 {
		return results, nil
	}
	var out importOutput
	if err := w.mcp.call(ctx, "commit_activity_import", map[string]any{"activities": rows}, &out); err != nil {
		return results[:0], err
	}
	for _, f := range out.Failed {
		if f.LineNumber >= 1 && f.LineNumber <= len(rows) {
			i := rows[f.LineNumber-1].activity
			msg := strings.Join(f.Errors, "; ")
			if msg == "" {
				msg = out.Summary.ErrorMessage
			}
			results[i] = dest.Result{ActivityID: b.Activities[i].ID, Status: dest.Failed, Message: msg}
		}
	}
	for i := range results {
		if results[i].Status == dest.Delivered {
			results[i].ExternalID = out.ImportRunID
		}
	}
	return results, nil
}
