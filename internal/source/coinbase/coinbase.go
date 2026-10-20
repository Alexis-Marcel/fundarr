// Package coinbase reads a Coinbase account (the retail app and Advanced
// Trade) with a view-only CDP API key. Coinbase keeps one account per
// currency (plus vaults); they are treated as one: moves between them are not
// activities, and balances are summed per currency.
//
// History comes from each account's ledger, GET /v2/accounts/{id}/transactions,
// which lists every balance change, Advanced Trade fills included. The v3
// fills endpoint is not used: it only covers the key's portfolio and may ask
// EU users for a 2FA proof token, while the v2 ledger has both legs of every
// fill; reading both would count each fill twice.
//
// A trade appears as one transaction in each account it touches (bought
// currency, paid currency); legs are joined into one Trade by the id they
// share (buy/sell/trade id, or the fill's order and time).
package coinbase

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

const defaultBase = "https://api.coinbase.com"

func init() {
	source.Register(source.Definition{
		Kind:        "coinbase",
		Name:        "Coinbase",
		Category:    "exchange",
		Description: "Crypto exchange.",
		Fields: []source.Field{
			{Name: "api_key", Label: "API key name", Help: "organizations/…/apiKeys/…"},
			{Name: "api_secret", Label: "Private key", Secret: true},
		},
		Setup: "Create a Secret API key on portal.cdp.coinbase.com with only the View permission. " +
			"Paste its name and private key.",
		Limits: "Coinbase does not detail wrapping (cbETH) or dust sweeps: corrections cover them.",
		New:    New,
	})
}

// New builds the connector from its settings.
func New(cfg source.Config) (source.Source, error) {
	key, secret := strings.TrimSpace(cfg.Get("api_key")), cfg.Get("api_secret")
	// The key file CDP downloads is JSON: {"name" or "id", "privateKey"}.
	if t := strings.TrimSpace(secret); strings.HasPrefix(t, "{") {
		var f struct {
			ID, Name   string
			PrivateKey string `json:"privateKey"`
		}
		if json.Unmarshal([]byte(t), &f) == nil && f.PrivateKey != "" {
			secret = f.PrivateKey
			if key == "" {
				key = f.Name
				if key == "" {
					key = f.ID
				}
			}
		}
	}
	if key == "" || strings.TrimSpace(secret) == "" {
		return nil, errors.New("coinbase: API key name and private key are required")
	}
	sig, err := parseKey(secret)
	if err != nil {
		return nil, err
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = source.NewHTTP()
	}
	s := &Source{
		c:   &client{base: defaultBase, host: "api.coinbase.com", key: key, signer: sig, http: hc, now: time.Now, sleep: sleepCtx},
		now: time.Now,
		st:  state{Accounts: map[string]time.Time{}},
	}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &s.st); err != nil {
			return nil, fmt.Errorf("coinbase: bad saved state: %w", err)
		}
		if s.st.Accounts == nil {
			s.st.Accounts = map[string]time.Time{}
		}
	}
	return s, nil
}

// Source is a configured Coinbase connector.
type Source struct {
	c   *client
	now func() time.Time
	st  state
}

// state is kept between runs.
type state struct {
	// Accounts: per account id, the time from which its ledger must be read
	// again (its newest transaction, or the oldest one still pending).
	Accounts map[string]time.Time `json:"accounts"`
}

func (s *Source) State() []byte {
	b, _ := json.Marshal(s.st)
	return b
}

// setBase points the client at another server (tests).
func (s *Source) setBase(base string) error {
	host, err := hostOf(base)
	if err != nil {
		return err
	}
	s.c.base, s.c.host = strings.TrimRight(base, "/"), host
	return nil
}

func progress(f source.Fetch, format string, args ...any) {
	if f.Progress != nil {
		f.Progress(format, args...)
	}
}

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return decimal.Zero
	}
	return d
}

// ---- Test: one cheap call, and the key must be view-only

func (s *Source) Test(ctx context.Context) error {
	var p struct {
		CanView     bool `json:"can_view"`
		CanTrade    bool `json:"can_trade"`
		CanTransfer bool `json:"can_transfer"`
	}
	if err := s.c.get(ctx, "/api/v3/brokerage/key_permissions", &p); err != nil {
		if isAuthError(err) {
			return fmt.Errorf("coinbase refused the key: check its name and private key were pasted whole (%w)", err)
		}
		return err
	}
	switch {
	case p.CanTrade || p.CanTransfer:
		return errors.New("coinbase: this key can trade or transfer; create one with only the View permission")
	case !p.CanView:
		return errors.New("coinbase: this key lacks the View permission")
	}
	return nil
}

// ---- accounts and balances

type money struct {
	Amount   string `json:"amount"`
	Currency string `json:"currency"`
}

type account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Type     string `json:"type"` // wallet, fiat, vault, collateral...
	Currency struct {
		Code string `json:"code"`
		Type string `json:"type"` // crypto, fiat
	} `json:"currency"`
	Balance money `json:"balance"`
}

type pagination struct {
	NextURI string `json:"next_uri"`
}

// pages follows next_uri from first, decoding each page's data into T.
// stop, when it returns true, ends the walk after the current page.
func pages[T any](ctx context.Context, c *client, first string, visit func([]T) (stop bool, err error)) error {
	next, seen := first, map[string]bool{}
	for next != "" && !seen[next] {
		seen[next] = true
		var page struct {
			Pagination pagination `json:"pagination"`
			Data       []T        `json:"data"`
		}
		if err := c.get(ctx, next, &page); err != nil {
			return err
		}
		stop, err := visit(page.Data)
		if err != nil || stop {
			return err
		}
		next = page.Pagination.NextURI
	}
	return nil
}

func (s *Source) accounts(ctx context.Context) ([]account, error) {
	var out []account
	err := pages(ctx, s.c, "/v2/accounts?limit=100", func(p []account) (bool, error) {
		out = append(out, p...)
		return false, nil
	})
	return out, err
}

// fiatSet remembers which codes Coinbase calls fiat, for assets
// model.CryptoOrFiat does not know.
type fiatSet map[string]bool

func (fs fiatSet) asset(code string) model.Asset {
	a := model.CryptoOrFiat(code)
	if fs[a.Symbol] {
		a.Kind = model.Fiat
	}
	return a
}

func fiatsOf(accts []account) fiatSet {
	fs := fiatSet{}
	for _, a := range accts {
		if strings.EqualFold(a.Currency.Type, "fiat") || a.Type == "fiat" {
			fs[strings.ToUpper(a.Currency.Code)] = true
		}
	}
	return fs
}

func (s *Source) Balances(ctx context.Context) ([]model.Balance, error) {
	accts, err := s.accounts(ctx)
	if err != nil {
		return nil, err
	}
	fs := fiatsOf(accts)
	total := map[string]decimal.Decimal{}
	for _, a := range accts {
		ccy := strings.ToUpper(strings.TrimSpace(a.Balance.Currency))
		if ccy == "" {
			ccy = strings.ToUpper(a.Currency.Code)
		}
		if v := dec(a.Balance.Amount); ccy != "" && v.IsPositive() {
			total[ccy] = total[ccy].Add(v)
		}
	}
	out := make([]model.Balance, 0, len(total))
	for ccy, v := range total {
		out = append(out, model.Balance{Amount: model.Amount{Asset: fs.asset(ccy), Qty: v}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset.Symbol < out[j].Asset.Symbol })
	return out, nil
}

// ---- Activities

const (
	// overlap re-reads a little before where the last run stopped: the legs
	// of a trade land in different accounts, maybe not at the same instant.
	overlap = 72 * time.Hour
	// settle: a trade with a leg missing is held back this long, waiting for
	// the other leg, before being read from the leg alone.
	settle = time.Hour
	// pendingWindow: pending transactions younger than this keep their
	// account's ledger open, so they are read once completed.
	pendingWindow = 14 * 24 * time.Hour
)

func (s *Source) Activities(ctx context.Context, f source.Fetch) error {
	progress(f, "accounts")
	accts, err := s.accounts(ctx)
	if err != nil {
		return err
	}
	fs := fiatsOf(accts)
	own := map[string]bool{}
	for _, a := range accts {
		own[a.ID] = true
	}

	var all []tx
	newest := map[string]time.Time{}
	for _, a := range accts {
		since, known := s.st.Accounts[a.ID]
		cutoff := time.Time{}
		if known {
			cutoff = since.Add(-overlap)
		}
		n := 0
		first := "/v2/accounts/" + url.PathEscape(a.ID) + "/transactions?limit=100"
		err := pages(ctx, s.c, first, func(p []tx) (bool, error) {
			for _, t := range p {
				if !cutoff.IsZero() && t.CreatedAt.Before(cutoff) {
					return true, nil // newest first: the rest is older
				}
				t.account = a.ID
				all = append(all, t)
				n++
				if t.CreatedAt.After(newest[a.ID]) {
					newest[a.ID] = t.CreatedAt
				}
			}
			return false, nil
		})
		if err != nil {
			return fmt.Errorf("%s account: %w", a.Currency.Code, err)
		}
		if n > 0 {
			progress(f, "%s (%s): %d transactions", a.Currency.Code, a.Name, n)
		}
	}

	m := &mapper{fs: fs, own: own, now: s.now(), held: map[string]time.Time{}, skipped: map[string]int{}}
	if err := m.run(all, f.Emit); err != nil {
		return err
	}
	for typ, n := range m.skipped {
		progress(f, "skipped %d %q transactions (not understood)", n, typ)
	}
	if m.internal > 0 {
		progress(f, "%d moves between your own Coinbase accounts ignored", m.internal)
	}
	if len(m.held) > 0 {
		progress(f, "%d accounts with pending transactions or trades awaiting their other side: read again next time", len(m.held))
	}

	// Only now that everything was emitted: move each account's start.
	for id, t := range newest {
		if h, ok := m.held[id]; ok && h.Before(t) {
			t = h
		}
		s.st.Accounts[id] = t
	}
	return nil
}
