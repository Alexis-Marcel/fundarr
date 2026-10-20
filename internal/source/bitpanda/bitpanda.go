// Package bitpanda reads a Bitpanda account through the Public API
// (https://api.public.bitpanda.com/v1, docs.public.bitpanda.com) with a key
// limited to the Balances and Transaction scopes. It replaces the legacy API
// (api.bitpanda.com/v1: wallets, fiatwallets, trades...), which Bitpanda
// switches off on 31 January 2027.
//
//   - History: GET /v1/operations, the unified timeline of every movement
//     (trades, savings plans, deposits, withdrawals, staking, rewards, card,
//     dividends, taxes), cursor-paginated, filterable with "from". Each
//     operation lists the transactions it booked on the user's wallets; see
//     ops.go for how they become activities.
//   - Balances: GET /v1/portfolio, one line per asset or fiat currency held.
//   - Names: GET /v1/assets and /v1/currencies (public) turn the UUIDs of
//     the two above into symbols, kinds and ISINs.
//
// Bitpanda is treated as one account: moves between its wallets (to and
// from staking, card or trading reservations) are not activities.
package bitpanda

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

const defaultBase = "https://api.public.bitpanda.com"

// overlap re-reads a little before the last run: operations are booked
// some time after they start (bank transfers, stock orders).
const overlap = 7 * 24 * time.Hour

// Bitpanda opened in 2014: what a full read starts from, as a floor.
var genesis = time.Date(2014, 1, 1, 0, 0, 0, 0, time.UTC)

// pageSize is the documented maximum.
const pageSize = 100

func init() {
	source.Register(source.Definition{
		Kind:        "bitpanda",
		Name:        "Bitpanda",
		Category:    "broker",
		Description: "Crypto, stocks, ETFs and metals.",
		Fields: []source.Field{
			{Name: "api_key", Label: "API key", Secret: true},
			{Name: "base_url", Label: "API URL", Optional: true, Advanced: true, Help: "leave empty"},
		},
		Setup:  "Bitpanda → Account settings → API Keys → New. Tick only Balances and Transaction.",
		Limits: "Bitpanda's API gives no transaction hash nor network for crypto transfers.",
		New:    New,
	})
}

// New builds the connector from its settings.
func New(cfg source.Config) (source.Source, error) {
	key := strings.TrimSpace(cfg.Get("api_key"))
	if key == "" {
		return nil, errors.New("bitpanda: API key is required")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.Get("base_url")), "/")
	if base == "" {
		base = defaultBase
	} else if !strings.HasPrefix(base, "http") {
		base = "https://" + base
	}
	base = strings.TrimSuffix(base, "/v1")
	hc := &http.Client{Timeout: 60 * time.Second}
	if cfg.HTTP != nil {
		c := *cfg.HTTP
		hc = &c
	}
	// The key travels in a custom header, which Go would carry along to
	// wherever a redirect points: refuse them.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	s := &Source{c: &client{base: base, key: key, http: hc, sleep: sleepCtx}}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &s.st); err != nil {
			return nil, fmt.Errorf("bitpanda: bad saved state: %w", err)
		}
	}
	if s.st.Assets == nil {
		s.st.Assets = map[string]assetMeta{}
	}
	return s, nil
}

// Source is a configured Bitpanda connector.
type Source struct {
	c  *client
	st state
}

// state is kept between runs.
type state struct {
	// Floor: an operation still pending (not credited yet) was seen in a run
	// that read from here; later runs read from here too until it is booked.
	Floor time.Time `json:"floor,omitempty"`
	// Assets caches what a UUID is (asset or fiat currency), so a run only
	// asks for the new ones.
	Assets map[string]assetMeta `json:"assets,omitempty"`
}

// assetMeta is what /v1/assets or /v1/currencies says of a UUID.
type assetMeta struct {
	Symbol string `json:"s"`
	Type   string `json:"t,omitempty"` // cryptocoin, commodity, index, security, equity_security; "fiat" for currencies
	Group  string `json:"g,omitempty"`
	ISIN   string `json:"i,omitempty"`
	Name   string `json:"n,omitempty"`
}

func (s *Source) State() []byte {
	b, _ := json.Marshal(s.st)
	return b
}

func progress(f source.Fetch, format string, args ...any) {
	if f.Progress != nil {
		f.Progress(format, args...)
	}
}

// ---- Test

// Test checks the key with the two cheapest reads it must allow: one
// operation (Transaction scope) and the portfolio (Balances scope).
func (s *Source) Test(ctx context.Context) error {
	var page opsPage
	if err := s.c.get(ctx, "/v1/operations", url.Values{"page_size": {"1"}}, &page); err != nil {
		var e *apiError
		if errors.As(err, &e) && e.Status == http.StatusForbidden {
			return errors.New("bitpanda: this key cannot read the history: tick the Transaction scope")
		}
		return err
	}
	if err := s.c.get(ctx, "/v1/portfolio", nil, nil); err != nil {
		var e *apiError
		if errors.As(err, &e) && e.Status == http.StatusForbidden {
			return errors.New("bitpanda: this key cannot read balances: tick the Balances scope")
		}
		return err
	}
	return nil
}

// ---- assets

// unit identifies what a transaction or a balance line moves: "a:" + asset
// UUID or "c:" + fiat currency UUID.
type unit string

func unitOf(assetID, currencyID string) unit {
	switch {
	case assetID != "":
		return unit("a:" + assetID)
	case currencyID != "":
		return unit("c:" + currencyID)
	}
	return ""
}

// asset turns a resolved UUID into the model's asset. Stocks and ETFs are
// securities keyed by ISIN (Bitpanda lists some twice, as a legacy
// "security" and a tradeable "equity_security": the ISIN makes them one;
// it also keeps the stock ETH, Eurotech, apart from the coin). Metals
// (type "commodity": XAU, XAG, XPT, XPD) have no ISIN: they are counted
// like crypto, by symbol, as are Bitpanda's crypto indices (BCI5...).
func (s *Source) asset(u unit) model.Asset {
	id := string(u[2:])
	m, ok := s.st.Assets[id]
	if !ok {
		// Not in Bitpanda's catalog: keep it apart, by UUID, so it still
		// adds up between history and balances.
		return model.Asset{Kind: model.Crypto, Symbol: "BITPANDA:" + strings.ToUpper(id)}
	}
	sym := strings.ToUpper(strings.TrimSpace(m.Symbol))
	switch {
	case u[0] == 'c':
		return model.Asset{Kind: model.Fiat, Symbol: sym, Name: m.Name}
	case m.Type == "security" || m.Type == "equity_security":
		return model.Asset{Kind: model.Security, Symbol: sym, ISIN: strings.ToUpper(m.ISIN), Name: m.Name}
	}
	return model.Asset{Kind: model.Crypto, Symbol: sym, Name: m.Name}
}

// resolve fetches what the cache does not know of the given units.
func (s *Source) resolve(ctx context.Context, units []unit) error {
	var assets []string
	currencies := false
	seen := map[string]bool{}
	for _, u := range units {
		id := string(u[2:])
		if _, ok := s.st.Assets[id]; ok || seen[id] {
			continue
		}
		seen[id] = true
		if u[0] == 'c' {
			currencies = true
		} else {
			assets = append(assets, id)
		}
	}
	if currencies {
		var res struct {
			Data []struct {
				ID     string `json:"id"`
				Symbol string `json:"symbol"`
				Name   string `json:"name"`
			} `json:"data"`
		}
		if err := s.c.get(ctx, "/v1/currencies", nil, &res); err != nil {
			return err
		}
		for _, c := range res.Data {
			s.st.Assets[c.ID] = assetMeta{Symbol: c.Symbol, Type: "fiat", Name: c.Name}
		}
	}
	sort.Strings(assets)
	// The id filter takes a comma-separated list; 50 at a time fit in one
	// page and in a URL.
	for len(assets) > 0 {
		n := min(50, len(assets))
		batch := assets[:n]
		assets = assets[n:]
		cursor := ""
		for {
			q := url.Values{"id": {strings.Join(batch, ",")}, "page_size": {fmt.Sprint(pageSize)}}
			if cursor != "" {
				q.Set("cursor", cursor)
			}
			var res struct {
				Data []struct {
					ID     string `json:"id"`
					Name   string `json:"name"`
					Symbol string `json:"symbol"`
					ISIN   string `json:"isin"`
					Group  string `json:"group"`
					Type   string `json:"type"`
				} `json:"data"`
				NextCursor  string `json:"next_cursor"`
				HasNextPage bool   `json:"has_next_page"`
			}
			if err := s.c.get(ctx, "/v1/assets", q, &res); err != nil {
				return err
			}
			for _, a := range res.Data {
				s.st.Assets[a.ID] = assetMeta{Symbol: a.Symbol, Type: strings.ToLower(a.Type), Group: strings.ToLower(a.Group), ISIN: a.ISIN, Name: a.Name}
			}
			if !res.HasNextPage || res.NextCursor == "" || res.NextCursor == cursor {
				break
			}
			cursor = res.NextCursor
		}
	}
	return nil
}

// ---- Balances

func (s *Source) Balances(ctx context.Context) ([]model.Balance, error) {
	var res struct {
		Data []struct {
			AssetID    string  `json:"asset_id"`
			CurrencyID string  `json:"currency_id"`
			Balance    *amount `json:"balance"`
		} `json:"data"`
	}
	if err := s.c.get(ctx, "/v1/portfolio", nil, &res); err != nil {
		return nil, err
	}
	type line struct {
		u unit
		q decimal.Decimal
	}
	var lines []line
	var units []unit
	for _, p := range res.Data {
		if p.Balance == nil {
			continue
		}
		u := unitOf(p.AssetID, p.CurrencyID)
		if u == "" {
			u = unitOf(p.Balance.AssetID, p.Balance.CurrencyID)
		}
		q := dec(p.Balance.Value)
		if u == "" || !q.IsPositive() {
			continue
		}
		lines = append(lines, line{u, q})
		units = append(units, u)
	}
	if err := s.resolve(ctx, units); err != nil {
		return nil, err
	}
	total := map[string]model.Amount{}
	for _, l := range lines {
		a := s.asset(l.u)
		cur, ok := total[a.Key()]
		if !ok {
			cur = model.Amount{Asset: a}
		}
		cur.Qty = cur.Qty.Add(l.q)
		total[a.Key()] = cur
	}
	out := make([]model.Balance, 0, len(total))
	for _, a := range total {
		out = append(out, model.Balance{Amount: a})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset.Key() < out[j].Asset.Key() })
	return out, nil
}

// ---- Activities

type opsPage struct {
	Data        []operation `json:"data"`
	NextCursor  string      `json:"next_cursor"`
	HasNextPage bool        `json:"has_next_page"`
}

func (s *Source) Activities(ctx context.Context, f source.Fetch) error {
	var from time.Time
	if !f.Since.IsZero() {
		from = f.Since.Add(-overlap)
		if !s.st.Floor.IsZero() && s.st.Floor.Before(from) {
			from = s.st.Floor
		}
	}
	s.c.onWait = func(path string, d time.Duration, why string) {
		progress(f, "%s: %s, waiting %s", path, why, d.Round(time.Second))
	}
	if from.IsZero() {
		progress(f, "operations (whole history)")
	} else {
		progress(f, "operations since %s", from.Format("2006-01-02"))
	}

	var ops []operation
	seen := map[string]bool{}
	cursor := ""
	for pages := 0; ; pages++ {
		q := url.Values{"page_size": {fmt.Sprint(pageSize)}}
		if !from.IsZero() {
			q.Set("from", from.UTC().Format("2006-01-02T15:04:05.000Z"))
		}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var page opsPage
		if err := s.c.get(ctx, "/v1/operations", q, &page); err != nil {
			return err
		}
		for _, op := range page.Data {
			if op.OperationID == "" || seen[op.OperationID] {
				continue
			}
			seen[op.OperationID] = true
			ops = append(ops, op)
		}
		if pages > 0 && pages%20 == 0 {
			progress(f, "%d operations read", len(ops))
		}
		if !page.HasNextPage || page.NextCursor == "" || page.NextCursor == cursor {
			break
		}
		cursor = page.NextCursor
	}

	var units []unit
	for _, op := range ops {
		units = append(units, op.units()...)
	}
	if err := s.resolve(ctx, units); err != nil {
		return err
	}

	pending := false
	emitted := map[string]bool{}
	unknown := map[string]int{}
	for _, op := range ops {
		acts, ok := mapOperation(op, s.asset)
		if !ok {
			pending = true
			continue
		}
		if !knownType(op.OperationType) {
			unknown[op.OperationType]++
		}
		for _, a := range acts {
			if emitted[a.NativeID] {
				continue
			}
			emitted[a.NativeID] = true
			if err := f.Emit(a); err != nil {
				return err
			}
		}
	}
	for t, n := range unknown {
		progress(f, "operation type %q not known (%d): imported by direction (deposit/withdrawal)", t, n)
	}
	if pending {
		fl := from
		if fl.IsZero() {
			fl = genesis
		}
		if s.st.Floor.IsZero() || fl.Before(s.st.Floor) {
			s.st.Floor = fl
		}
	} else {
		s.st.Floor = time.Time{}
	}
	return nil
}
