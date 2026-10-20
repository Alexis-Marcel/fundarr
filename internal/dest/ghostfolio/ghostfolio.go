// Package ghostfolio delivers activities to a Ghostfolio server through its
// REST API (POST /api/v1/import), signed in with the user's security token
// (POST /api/v1/auth/anonymous gives a JWT, renewed when it is refused).
//
// Ghostfolio knows six activity types (BUY, SELL, DIVIDEND, FEE, INTEREST,
// LIABILITY), no transfers and no cash movements: cash is the account
// balance, set by hand. Fundarr's activities are translated as follows.
//
//   - Asset bought or sold for cash (fiat, or a stablecoin when stablecoins
//     count as cash): BUY / SELL in that currency. A fee in the same cash is
//     the activity's fee; a fee taken from the units bought lowers the
//     quantity; a fee in another asset is its own activity (see below).
//   - Asset for asset (BTC for ETH, or for USDT when stablecoins are crypto):
//     SELL what left and BUY what came, both at the cost Fundarr carries over
//     (Batch.Costs), so the swap realizes nothing.
//   - Crypto deposit (from another of your accounts or from outside) and
//     balance corrections in: BUY at the cost Fundarr knows (carried over for
//     a transfer between your accounts, market value otherwise), at 0 when it
//     does not know it (the result says so).
//   - Crypto withdrawal, fee or tax paid in units, balance corrections out:
//     SELL at 0 (at its cost when Batch.Costs has one). Ghostfolio counts it
//     as a realized loss of what it cost; between two of your accounts the
//     other side's BUY brings that cost back.
//   - Rewards in units (staking, earn, airdrops): BUY at 0, so their whole
//     value shows as gain. Rewards and interest in cash: INTEREST.
//   - Dividends: DIVIDEND of the security (quantity 1, the amount as price).
//   - Fees and taxes in cash: FEE.
//   - Fiat deposits and withdrawals, and currency conversions: skipped (the
//     account balance is Ghostfolio's business).
//
// Symbols: securities go to YAHOO by ISIN when known (Ghostfolio resolves
// the ISIN to its Yahoo ticker), by ticker otherwise. Crypto goes to
// COINGECKO for the coins in coingeckoIDs (unambiguous ids), to YAHOO as
// "<SYMBOL>USD" (Ghostfolio's form of BTC-USD) for the others; a coin neither
// knows fails with Ghostfolio's message.
//
// Re-pushes: every activity's comment starts with "fundarr:<id>" (and
// "#2", "#3"... for its extra rows). Before importing, Fundarr reads the
// account's activities and leaves out the rows already there: Ghostfolio's
// own duplicate check does not work for FEE and INTEREST, whose symbol it
// replaces by a random id. One import is all or nothing when a row is
// invalid, so a refused import is split until the faulty activities are
// found; they fail alone.
package ghostfolio

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/dest"
	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

func init() {
	dest.Register(dest.Definition{
		Kind:        "ghostfolio",
		Name:        "Ghostfolio",
		Description: "Open-source wealth tracker.",
		Fields: []source.Field{
			{Name: "url", Label: "Ghostfolio URL", Help: "e.g. http://ghostfolio:3333"},
			{Name: "token", Label: "Security token", Secret: true, Help: "Given at sign-up"},
			{Name: "stablecoins", Label: "Stablecoins as cash", Optional: true, Help: "yes (default) or no"},
		},
		Setup: "Use the security token you sign in to Ghostfolio with.",
		New: func(cfg source.Config) (dest.Destination, error) {
			u := strings.TrimRight(strings.TrimSpace(cfg.Get("url")), "/")
			tok := strings.TrimSpace(cfg.Get("token"))
			if u == "" || tok == "" {
				return nil, errors.New("URL and security token are required")
			}
			client := cfg.HTTP
			if client == nil {
				client = &http.Client{Timeout: 2 * time.Minute}
			}
			return &Ghostfolio{
				url:         u,
				token:       tok,
				http:        client,
				stableCash:  !strings.EqualFold(strings.TrimSpace(cfg.Get("stablecoins")), "no"),
				accountCcys: map[string]string{},
			}, nil
		},
	})
}

type Ghostfolio struct {
	url, token string
	http       *http.Client
	stableCash bool

	mu          sync.Mutex
	jwt         string
	accountCcys map[string]string
	listPath    string // where activities are listed: /api/v1/activities, /api/v1/order before 2.x renamed it
}

// apiError is a non-2xx answer.
type apiError struct {
	Status  int
	Message string
}

func (e *apiError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("Ghostfolio: HTTP %d", e.Status)
	}
	return "Ghostfolio: " + e.Message
}

// message reads NestJS's error body: {"message": "..." | ["...", ...]}.
func message(body []byte) string {
	var e struct {
		Message json.RawMessage `json:"message"`
		Error   string          `json:"error"`
	}
	if json.Unmarshal(body, &e) != nil {
		return strings.TrimSpace(string(body))
	}
	var one string
	if json.Unmarshal(e.Message, &one) == nil && one != "" {
		return one
	}
	var many []string
	if json.Unmarshal(e.Message, &many) == nil && len(many) > 0 {
		return strings.Join(many, "; ")
	}
	return e.Error
}

func (g *Ghostfolio) send(ctx context.Context, method, path, bearer string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, g.url+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := g.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &apiError{Status: resp.StatusCode, Message: message(data)}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("Ghostfolio: unexpected answer to %s %s: %w", method, path, err)
	}
	return nil
}

// login trades the security token for a JWT.
func (g *Ghostfolio) login(ctx context.Context) (string, error) {
	var out struct {
		AuthToken string `json:"authToken"`
	}
	err := g.send(ctx, http.MethodPost, "/api/v1/auth/anonymous", "", map[string]string{"accessToken": g.token}, &out)
	var ae *apiError
	if errors.As(err, &ae) && (ae.Status == http.StatusForbidden || ae.Status == http.StatusUnauthorized) {
		return "", errors.New("Ghostfolio refused the security token")
	}
	if err != nil {
		return "", err
	}
	if out.AuthToken == "" {
		return "", errors.New("Ghostfolio: no token in the sign-in answer")
	}
	return out.AuthToken, nil
}

// call sends an authenticated request, signing in again once when the JWT
// is refused (expired, or the server's secret changed).
func (g *Ghostfolio) call(ctx context.Context, method, path string, body, out any) error {
	for try := 0; ; try++ {
		g.mu.Lock()
		jwt := g.jwt
		g.mu.Unlock()
		if jwt == "" {
			var err error
			if jwt, err = g.login(ctx); err != nil {
				return err
			}
			g.mu.Lock()
			g.jwt = jwt
			g.mu.Unlock()
		}
		err := g.send(ctx, method, path, jwt, body, out)
		var ae *apiError
		if try == 0 && errors.As(err, &ae) && ae.Status == http.StatusUnauthorized {
			g.mu.Lock()
			if g.jwt == jwt {
				g.jwt = ""
			}
			g.mu.Unlock()
			continue
		}
		return err
	}
}

func (g *Ghostfolio) Accounts(ctx context.Context) ([]dest.Account, error) {
	var out struct {
		Accounts []struct {
			ID       string `json:"id"`
			Name     string `json:"name"`
			Currency string `json:"currency"`
		} `json:"accounts"`
	}
	if err := g.call(ctx, http.MethodGet, "/api/v1/account", nil, &out); err != nil {
		return nil, err
	}
	accs := make([]dest.Account, 0, len(out.Accounts))
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, a := range out.Accounts {
		g.accountCcys[a.ID] = a.Currency
		accs = append(accs, dest.Account{ID: a.ID, Name: a.Name, Currency: a.Currency})
	}
	return accs, nil
}

// order is one activity of Ghostfolio's import (CreateOrderDto). The server
// refuses unknown fields.
type order struct {
	AccountID  string  `json:"accountId"`
	Comment    string  `json:"comment"`
	Currency   string  `json:"currency"`
	DataSource string  `json:"dataSource,omitempty"` // left out for FEE and INTEREST (MANUAL, symbol made by the server)
	Date       string  `json:"date"`
	Fee        float64 `json:"fee"`
	Quantity   float64 `json:"quantity"`
	Symbol     string  `json:"symbol"`
	Type       string  `json:"type"`
	UnitPrice  float64 `json:"unitPrice"`

	activity int // index in the batch
}

const commentPrefix = "fundarr:"

// rowKey is what identifies a row Fundarr wrote: the comment's first word.
func rowKey(comment string) string {
	if !strings.HasPrefix(comment, commentPrefix) {
		return ""
	}
	if i := strings.IndexAny(comment, " \n\t"); i >= 0 {
		return comment[:i]
	}
	return comment
}

func f(d decimal.Decimal) float64 { return d.InexactFloat64() }

// coingeckoIDs maps the symbols of major coins to their CoinGecko id.
var coingeckoIDs = map[string]string{
	"BTC": "bitcoin", "ETH": "ethereum", "BNB": "binancecoin", "SOL": "solana", "XRP": "ripple",
	"ADA": "cardano", "DOGE": "dogecoin", "DOT": "polkadot", "AVAX": "avalanche-2", "LINK": "chainlink",
	"TRX": "tron", "TON": "the-open-network", "LTC": "litecoin", "BCH": "bitcoin-cash", "ETC": "ethereum-classic",
	"XLM": "stellar", "XMR": "monero", "ATOM": "cosmos", "NEAR": "near", "UNI": "uniswap",
	"AAVE": "aave", "ARB": "arbitrum", "OP": "optimism", "SUI": "sui", "APT": "aptos",
	"POL": "polygon-ecosystem-token", "MATIC": "matic-network", "SHIB": "shiba-inu", "PEPE": "pepe",
	"OKB": "okb", "CRO": "crypto-com-chain", "FIL": "filecoin", "ALGO": "algorand", "HBAR": "hedera-hashgraph",
	"ICP": "internet-computer", "VET": "vechain", "KAS": "kaspa", "WBTC": "wrapped-bitcoin", "STETH": "staked-ether",
	"USDT": "tether", "USDC": "usd-coin", "DAI": "dai", "FDUSD": "first-digital-usd", "USDE": "ethena-usde",
	"PYUSD": "paypal-usd", "EURC": "euro-coin",
}

// instrument returns Ghostfolio's data source and symbol for an asset.
func instrument(a model.Asset) (dataSource, symbol string) {
	if a.Kind == model.Crypto {
		if id, ok := coingeckoIDs[a.Symbol]; ok {
			return "COINGECKO", id
		}
		return "YAHOO", a.Symbol + "USD"
	}
	if a.ISIN != "" {
		return "YAHOO", a.ISIN
	}
	return "YAHOO", a.Symbol
}

// cash returns the currency an asset counts as cash in, or "" if it is not cash.
func (g *Ghostfolio) cash(a model.Asset) string {
	if a.Kind == model.Fiat {
		return a.Symbol
	}
	if g.stableCash {
		return model.PeggedTo(a)
	}
	return ""
}

// extra is what Fundarr knows about an activity beyond the activity itself.
type extra struct {
	cost    decimal.Decimal // what the incoming asset cost, in the account currency
	hasCost bool
	peer    string // the other side of a transfer between your accounts
}

// errCash is why cash movements are skipped.
var errCash = errors.New("cash: Ghostfolio keeps it as the account balance")

// translate turns one activity into import rows. ccy is the account
// currency, the one costs are in. note is worth telling even when delivered.
func (g *Ghostfolio) translate(a model.Activity, account, ccy string, x extra) (rows []order, note string, err error) {
	date := a.Time.UTC().Format(time.RFC3339)
	desc := ""
	switch {
	case x.peer != "" && a.Inflow():
		desc = "transfer from " + x.peer
	case x.peer != "":
		desc = "transfer to " + x.peer
	case a.Type == model.Adjustment:
		desc = "balance correction (Fundarr)"
	case a.Type == model.Reward:
		desc = "reward"
	case a.Type == model.Tax:
		desc = "tax"
	}
	if a.Note != "" {
		desc = strings.TrimSpace(desc + " " + a.Note)
	}
	var notes []string
	add := func(o order) {
		o.AccountID, o.Date = account, date
		o.Comment = commentPrefix + a.ID
		if n := len(rows); n > 0 {
			o.Comment += fmt.Sprintf("#%d", n+1)
		}
		if desc != "" {
			o.Comment += " " + desc
		}
		if len(o.Comment) > 2000 {
			o.Comment = o.Comment[:2000]
		}
		rows = append(rows, o)
	}
	units := func(t string, amt model.Amount, currency string, price decimal.Decimal) order {
		ds, sym := instrument(amt.Asset)
		return order{Type: t, DataSource: ds, Symbol: sym, Currency: currency, Quantity: f(amt.Qty), UnitPrice: f(price)}
	}
	// costPrice is the unit price of the activity's known cost, if any.
	costPrice := func(qty decimal.Decimal) (decimal.Decimal, bool) {
		if x.hasCost && qty.IsPositive() {
			return x.cost.Div(qty), true
		}
		return decimal.Zero, false
	}
	// in: units arriving at the cost Fundarr knows.
	in := func(amt model.Amount) {
		p, ok := costPrice(amt.Qty)
		if !ok {
			notes = append(notes, "no price known: "+amt.Asset.String()+" enters at cost 0")
		}
		add(units("BUY", amt, ccy, p))
	}
	// out: units leaving with nothing back, at their cost when known.
	out := func(amt model.Amount, withCost bool) {
		p := decimal.Zero
		if withCost {
			p, _ = costPrice(amt.Qty)
		}
		add(units("SELL", amt, ccy, p))
	}
	cashRow := func(t string, amt model.Amount, symbol string) {
		o := order{Type: t, Symbol: symbol, Currency: g.cash(amt.Asset)}
		switch t {
		case "FEE":
			o.Fee = f(amt.Qty)
		default: // INTEREST, DIVIDEND: one unit at the amount
			o.Quantity, o.UnitPrice = 1, f(amt.Qty)
		}
		add(o)
	}
	feeRow := func() {
		if a.Fee == nil || a.Fee.Qty.IsZero() {
			return
		}
		if g.cash(a.Fee.Asset) != "" {
			cashRow("FEE", *a.Fee, "Fee")
			return
		}
		out(*a.Fee, false)
	}
	done := func() ([]order, string, error) { return rows, strings.Join(notes, "; "), nil }

	switch a.Type {
	case model.Trade:
		if a.Quote == nil {
			return nil, "", errors.New("trade without a counter amount")
		}
		baseCash, quoteCash := g.cash(a.Base.Asset), g.cash(a.Quote.Asset)
		switch {
		case baseCash == "" && quoteCash != "", baseCash != "" && quoteCash == "":
			// An asset bought or sold for cash; Fundarr may see it from the
			// cash side (buy USDT with BTC is selling BTC).
			asset, money, side, cur := a.Base, *a.Quote, a.Side, quoteCash
			if baseCash != "" {
				asset, money, cur = *a.Quote, a.Base, baseCash
				side = model.Buy
				if a.Side == model.Buy {
					side = model.Sell
				}
			}
			if asset.Qty.IsZero() {
				return nil, "", errors.New("trade of zero units")
			}
			t := "BUY"
			if side == model.Sell {
				t = "SELL"
			}
			o := units(t, asset, cur, money.Qty.Div(asset.Qty))
			switch {
			case a.Fee == nil || a.Fee.Qty.IsZero():
			case g.cash(a.Fee.Asset) == cur:
				o.Fee = f(a.Fee.Qty)
			case a.Fee.Asset.Key() == asset.Asset.Key() && side == model.Buy && a.Fee.Qty.LessThan(asset.Qty):
				// Fee taken from what was bought: fewer units arrive.
				q := asset.Qty.Sub(a.Fee.Qty)
				o.Quantity, o.UnitPrice = f(q), f(money.Qty.Div(q))
			default:
				add(o)
				feeRow()
				return done()
			}
			add(o)
			return done()
		case baseCash != "" && quoteCash != "":
			return nil, "", errors.New("currency conversion: Ghostfolio keeps cash as the account balance")
		default:
			// Asset for asset: what left is sold and what came is bought, both
			// at the cost carried over, so the swap realizes nothing.
			inAmt, outAmt := a.Base, *a.Quote
			if a.Side == model.Sell {
				inAmt, outAmt = *a.Quote, a.Base
			}
			p, ok := decimal.Zero, x.hasCost
			if ok && outAmt.Qty.IsPositive() {
				p = x.cost.Div(outAmt.Qty)
			}
			add(units("SELL", outAmt, ccy, p))
			feeInIn := a.Fee != nil && a.Fee.Asset.Key() == inAmt.Asset.Key() && a.Fee.Qty.LessThan(inAmt.Qty)
			if feeInIn {
				inAmt = model.Amount{Asset: inAmt.Asset, Qty: inAmt.Qty.Sub(a.Fee.Qty)}
			}
			in(inAmt)
			if !feeInIn {
				feeRow()
			}
			return done()
		}
	case model.Deposit, model.Withdrawal, model.Adjustment:
		if g.cash(a.Base.Asset) != "" {
			return nil, "", errCash
		}
		if a.Inflow() {
			in(a.Base)
		} else {
			out(a.Base, true)
		}
		feeRow()
		return done()
	case model.Reward, model.Interest:
		if g.cash(a.Base.Asset) != "" {
			cashRow("INTEREST", a.Base, "Interest")
			return done()
		}
		// Units received for nothing: free, their value is all gain.
		add(units("BUY", a.Base, ccy, decimal.Zero))
		return done()
	case model.Dividend:
		if a.Quote == nil {
			return nil, "", errors.New("dividend without the security that paid it")
		}
		cur := g.cash(a.Base.Asset)
		if cur == "" {
			add(units("BUY", a.Base, ccy, decimal.Zero))
			return done()
		}
		ds, sym := instrument(a.Quote.Asset)
		add(order{Type: "DIVIDEND", DataSource: ds, Symbol: sym, Currency: cur, Quantity: 1, UnitPrice: f(a.Base.Qty)})
		feeRow()
		return done()
	case model.Fee, model.Tax:
		if g.cash(a.Base.Asset) != "" {
			name := "Fee"
			if a.Type == model.Tax {
				name = "Tax"
			}
			cashRow("FEE", a.Base, name)
			return done()
		}
		out(a.Base, false)
		return done()
	}
	return nil, "", fmt.Errorf("activity type %q is not supported", a.Type)
}

// existing returns the keys of the rows Fundarr already wrote to an account.
func (g *Ghostfolio) existing(ctx context.Context, account string) (map[string]bool, error) {
	var out struct {
		Activities []struct {
			Comment *string `json:"comment"`
		} `json:"activities"`
	}
	q := "?accounts=" + url.QueryEscape(account)
	g.mu.Lock()
	path := g.listPath
	g.mu.Unlock()
	if path == "" {
		path = "/api/v1/activities"
	}
	err := g.call(ctx, http.MethodGet, path+q, nil, &out)
	var ae *apiError
	if errors.As(err, &ae) && ae.Status == http.StatusNotFound && path == "/api/v1/activities" {
		path = "/api/v1/order"
		err = g.call(ctx, http.MethodGet, path+q, nil, &out)
	}
	if err != nil {
		return nil, fmt.Errorf("listing the account's activities: %w", err)
	}
	g.mu.Lock()
	g.listPath = path
	g.mu.Unlock()
	keys := map[string]bool{}
	for _, a := range out.Activities {
		if a.Comment != nil {
			if k := rowKey(*a.Comment); k != "" {
				keys[k] = true
			}
		}
	}
	return keys, nil
}

func (g *Ghostfolio) Push(ctx context.Context, b dest.Batch) ([]dest.Result, error) {
	g.mu.Lock()
	ccy, ok := g.accountCcys[b.Account]
	g.mu.Unlock()
	if !ok {
		if _, err := g.Accounts(ctx); err != nil {
			return nil, err
		}
		g.mu.Lock()
		ccy, ok = g.accountCcys[b.Account]
		g.mu.Unlock()
		if !ok {
			return nil, fmt.Errorf("Ghostfolio account %s does not exist (anymore)", b.Account)
		}
	}
	results := make([]dest.Result, len(b.Activities))
	type group struct {
		activity int
		rows     []order
		note     string
	}
	var groups []group
	var have map[string]bool
	for i, a := range b.Activities {
		x := extra{peer: b.Transfers[a.ID]}
		x.cost, x.hasCost = b.Costs[a.ID]
		rows, note, err := g.translate(a, b.Account, ccy, x)
		if err != nil {
			results[i] = dest.Result{ActivityID: a.ID, Status: dest.Skipped, Message: err.Error()}
			continue
		}
		if have == nil {
			if have, err = g.existing(ctx, b.Account); err != nil {
				return nil, err
			}
		}
		results[i] = dest.Result{ActivityID: a.ID, Status: dest.Duplicate}
		var missing []order
		for _, r := range rows {
			if !have[rowKey(r.Comment)] {
				r.activity = i
				missing = append(missing, r)
			}
		}
		if len(missing) > 0 {
			groups = append(groups, group{activity: i, rows: missing, note: note})
		}
	}
	// One import is refused whole when one row is invalid: split until the
	// faulty activities stand alone.
	var push func(gs []group) error
	push = func(gs []group) error {
		if len(gs) == 0 {
			return nil
		}
		var rows []order
		for _, gr := range gs {
			rows = append(rows, gr.rows...)
		}
		var out struct {
			Activities []struct {
				ID      string  `json:"id"`
				Comment *string `json:"comment"`
			} `json:"activities"`
		}
		err := g.call(ctx, http.MethodPost, "/api/v1/import", map[string]any{"activities": rows}, &out)
		var ae *apiError
		if errors.As(err, &ae) && ae.Status == http.StatusBadRequest {
			if len(gs) == 1 {
				i := gs[0].activity
				results[i] = dest.Result{ActivityID: b.Activities[i].ID, Status: dest.Failed, Message: ae.Error()}
				return nil
			}
			if err := push(gs[:len(gs)/2]); err != nil {
				return err
			}
			return push(gs[len(gs)/2:])
		}
		if err != nil {
			return err
		}
		created := map[string]string{}
		for _, a := range out.Activities {
			if a.Comment != nil {
				created[rowKey(*a.Comment)] = a.ID
			}
		}
		for _, gr := range gs {
			i := gr.activity
			// Rows missing from the answer were already there (Ghostfolio
			// drops exact duplicates without a word).
			res := dest.Result{ActivityID: b.Activities[i].ID, Status: dest.Duplicate}
			for _, r := range gr.rows {
				if id, ok := created[rowKey(r.Comment)]; ok {
					res.Status, res.Message = dest.Delivered, gr.note
					if res.ExternalID == "" {
						res.ExternalID = id
					}
				}
			}
			results[i] = res
		}
		return nil
	}
	if err := push(groups); err != nil {
		return results[:0], err
	}
	return results, nil
}
