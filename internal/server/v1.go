package server

import (
	"context"
	"encoding/csv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"github.com/Alexis-Marcel/fundarr/pkg/api"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/Alexis-Marcel/fundarr/pkg/portfolio"
	"github.com/shopspring/decimal"
)

// The /api/v1 routes are Fundarr's stable, read-only API: any program can
// read the history with an API token (Authorization: Bearer fdr_…), so a
// tracker Fundarr does not push to still gets the data. See docs/api.md.

// reader lets in a live API token (marked used) or a signed-in user, for
// reads only.
func (s *Server) reader(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, bearer := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !(bearer && s.st.CheckAPIToken(r.Context(), strings.TrimSpace(token))) && s.auth.user(r) == "" {
			writeErr(w, http.StatusUnauthorized, "sign in, or send an API token: Authorization: Bearer fdr_…")
			return
		}
		h(w, r)
	}
}

// The read API's types live in pkg/api, for Go apps to decode them.
type (
	v1Source   = api.Source
	v1Activity = api.Activity
)

// activitiesV1 builds the whole list, oldest first, filtered by the query
// (source, since, until).
func (s *Server) activitiesV1(r *http.Request) ([]v1Activity, error) {
	ctx := r.Context()
	l, err := s.eng.Ledger(ctx)
	if err != nil {
		return nil, err
	}
	q := r.URL.Query()
	since, _ := time.Parse(time.RFC3339, q.Get("since"))
	until, _ := time.Parse(time.RFC3339, q.Get("until"))
	out := []v1Activity{}
	for _, v := range s.views(ctx, l, s.eng.Currency(ctx)) {
		if src := q.Get("source"); src != "" && v.SourceID != src {
			continue
		}
		if !since.IsZero() && v.Time.Before(since) || !until.IsZero() && !v.Time.Before(until) {
			continue
		}
		out = append(out, v)
	}
	return out, nil
}

// views is every activity as the API shows it, oldest first; costs are in
// ccy, left out when ccy is "".
func (s *Server) views(ctx context.Context, l *engine.Ledger, ccy string) []v1Activity {
	srcs := s.sourceMap(ctx)
	var costs map[string]decimal.Decimal
	if ccy != "" {
		costs = s.eng.Costs(ctx, l, ccy)
	}
	out := make([]v1Activity, 0, len(l.Acts))
	for _, a := range l.Acts {
		v := v1Activity{Activity: a, Source: srcs[a.SourceID], Correction: a.Type == model.Adjustment}
		if c, ok := costs[a.ID]; ok {
			v.Cost, v.Currency = c.StringFixed(2), ccy
		}
		if o := l.In[a.ID]; o != "" {
			v.TransferWith = o
		} else if i := l.Out[a.ID]; i != "" {
			v.TransferWith = i
		}
		out = append(out, v)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Time.Equal(out[j].Time) {
			return out[i].Time.Before(out[j].Time)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func (s *Server) sourceMap(ctx context.Context) map[string]v1Source {
	srcs := map[string]v1Source{}
	if list, err := s.st.Sources(ctx); err == nil {
		for _, e := range list {
			srcs[e.ID] = v1Source{ID: e.ID, Name: e.Name, Kind: e.Kind}
		}
	}
	return srcs
}

func cursor(a v1Activity) string { return strconv.FormatInt(a.Time.UnixMilli(), 10) + "_" + a.ID }

// GET /api/v1/activities?source=&since=&until=&after=&limit=
func (s *Server) v1Activities(w http.ResponseWriter, r *http.Request) {
	all, err := s.activitiesV1(r)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	limit := min(max(atoi(r.URL.Query().Get("limit"), 500), 1), 5000)
	start := 0
	if after := r.URL.Query().Get("after"); after != "" {
		start = sort.Search(len(all), func(i int) bool { return cursor(all[i]) > after })
	}
	page := all[start:min(start+limit, len(all))]
	next := ""
	if start+limit < len(all) && len(page) > 0 {
		next = cursor(page[len(page)-1])
	}
	writeJSON(w, map[string]any{"activities": page, "next": next})
}

// GET /api/v1/sources
func (s *Server) v1Sources(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.Sources(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []v1Source{}
	for _, e := range list {
		out = append(out, v1Source{ID: e.ID, Name: e.Name, Kind: e.Kind})
	}
	writeJSON(w, out)
}

// GET /api/v1/holdings: what each source reported holding at its last sync.
func (s *Server) v1Holdings(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.Sources(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	type src struct {
		v1Source
		At       time.Time       `json:"at"`
		Balances []model.Balance `json:"balances"`
	}
	out := []src{}
	for _, e := range list {
		bal, at, err := s.st.Balances(r.Context(), e.ID)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		if bal == nil {
			bal = []model.Balance{}
		}
		out = append(out, src{v1Source{ID: e.ID, Name: e.Name, Kind: e.Kind}, at, bal})
	}
	writeJSON(w, out)
}

// ---- portfolio (crypto across sources) and French tax

// priceWait bounds the time spent asking providers for prices a page has
// not seen yet; what is still unknown after it counts as unpriced.
const priceWait = 30 * time.Second

func money(d decimal.Decimal) string { return d.StringFixed(2) }

// unitPrice keeps a few significant digits for coins worth little.
func unitPrice(d decimal.Decimal) string {
	if d.Abs().GreaterThanOrEqual(decimal.NewFromInt(1)) || d.IsZero() {
		return d.StringFixed(2)
	}
	return d.Round(10).String()
}

func assets(list []model.Asset) []model.Asset {
	if list == nil {
		return []model.Asset{}
	}
	return list
}

// GET /api/v1/portfolio: the crypto held now, across sources.
func (s *Server) v1Portfolio(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, err := s.eng.Ledger(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	srcs := s.sourceMap(ctx)
	ccy := s.eng.Currency(ctx)
	pctx, cancel := context.WithTimeout(ctx, priceWait)
	defer cancel()
	at := s.eng.Now().UTC()
	p := portfolio.Now(l.Acts, l.In, s.eng.UnitPrice(pctx, ccy), at)

	type heldIn struct {
		ID   string `json:"id"`
		Name string `json:"name"`
		Qty  string `json:"qty"`
	}
	type held struct {
		Asset      model.Asset `json:"asset"`
		Qty        string      `json:"qty"`
		Price      string      `json:"price"`
		Value      string      `json:"value"`
		Cost       string      `json:"cost"`
		Unrealized string      `json:"unrealized"`
		Share      string      `json:"share"`
		Sources    []heldIn    `json:"sources"`
	}
	type worth struct {
		v1Source
		Value string `json:"value"`
	}
	out := []held{}
	for _, h := range p.Assets {
		v := held{h.Asset, h.Qty.String(), unitPrice(h.Price), money(h.Value), money(h.Cost), money(h.Unrealized),
			h.Share.StringFixed(3), []heldIn{}}
		for _, q := range h.Sources {
			v.Sources = append(v.Sources, heldIn{q.Source, srcs[q.Source].Name, q.Amount.String()})
		}
		out = append(out, v)
	}
	bySource := []worth{}
	for _, v := range p.Sources {
		src := srcs[v.Source]
		src.ID = v.Source
		bySource = append(bySource, worth{src, money(v.Amount)})
	}
	writeJSON(w, map[string]any{
		"currency": ccy,
		"at":       at,
		"total": map[string]string{"value": money(p.Value), "invested": money(p.Invested), "cost": money(p.Cost),
			"unrealized": money(p.Unrealized), "realized": money(p.Realized), "rewards": money(p.Rewards)},
		"assets":   out,
		"sources":  bySource,
		"unpriced": assets(p.Unpriced),
	})
}

// GET /api/v1/portfolio/history?from=&to=&step=day|week|month
func (s *Server) v1History(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	step := q.Get("step")
	if step == "" {
		step = "day"
	}
	if step != "day" && step != "week" && step != "month" {
		writeErr(w, 400, "step: day, week or month")
		return
	}
	l, err := s.eng.Ledger(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	today := s.eng.Now().UTC().Truncate(24 * time.Hour)
	from, to := portfolio.First(l.Acts), today
	for name, d := range map[string]*time.Time{"from": &from, "to": &to} {
		if v := q.Get(name); v != "" {
			t, err := time.Parse(time.DateOnly, v)
			if err != nil {
				writeErr(w, 400, name+": YYYY-MM-DD")
				return
			}
			*d = t
		}
	}
	if to.After(today) {
		to = today
	}
	ccy := s.eng.Currency(ctx)
	if from.IsZero() || from.After(to) {
		writeJSON(w, map[string]any{"currency": ccy, "points": []any{}, "unpriced": []model.Asset{}})
		return
	}
	days := portfolio.Days(from, to, step)
	if days == nil {
		writeErr(w, 422, "more than 3700 points: use step=week or month, or a shorter range")
		return
	}
	pctx, cancel := context.WithTimeout(ctx, priceWait)
	defer cancel()
	points, unpriced := portfolio.History(l.Acts, l.In, s.eng.UnitPrice(pctx, ccy), days)
	type point struct {
		Date     string `json:"date"`
		Value    string `json:"value"`
		Invested string `json:"invested"`
	}
	out := make([]point, 0, len(points))
	for _, p := range points {
		out = append(out, point{p.Day.Format(time.DateOnly), money(p.Value), money(p.Invested)})
	}
	writeJSON(w, map[string]any{"currency": ccy, "points": out, "unpriced": assets(unpriced)})
}

// GET /api/v1/tax/fr?year=&rewards=income: the year's formulaire 2086.
func (s *Server) v1TaxFR(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	now := s.eng.Now().UTC()
	year := atoi(q.Get("year"), now.Year()-1)
	if year < 2009 || year > now.Year() {
		writeErr(w, 400, "year: 2009 to "+strconv.Itoa(now.Year()))
		return
	}
	rewards := q.Get("rewards")
	if rewards != "" && rewards != "income" {
		writeErr(w, 400, "rewards: income, or leave it out")
		return
	}
	l, err := s.eng.Ledger(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	srcs := s.sourceMap(ctx)
	pctx, cancel := context.WithTimeout(ctx, priceWait)
	defer cancel()
	t := portfolio.TaxFR(l.Acts, l.In, s.eng.UnitPrice(pctx, "EUR"), year, rewards == "income")
	type src struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	type line struct {
		Time         time.Time   `json:"time"`
		Source       src         `json:"source"`
		Asset        model.Asset `json:"asset"`
		Qty          string      `json:"qty"`
		Price        string      `json:"price"`
		Fees         string      `json:"fees"`
		Portfolio    string      `json:"portfolio"`
		Acquisition  string      `json:"acquisition"`
		CapitalShare string      `json:"capitalShare"`
		Gain         string      `json:"gain"`
	}
	lines := []line{}
	for _, d := range t.Disposals {
		lines = append(lines, line{d.Time.UTC(), src{d.Source, srcs[d.Source].Name}, d.Sold.Asset, d.Sold.Qty.String(),
			money(d.Price), money(d.Fees), money(d.Portfolio), money(d.Acquisition), money(d.CapitalShare), money(d.Gain)})
	}
	writeJSON(w, map[string]any{
		"year":      t.Year,
		"currency":  "EUR",
		"disposals": lines,
		"total": map[string]any{"disposals": money(t.Total), "gain": money(t.Gain), "exempt": t.Exempt,
			"tax": money(t.Tax), "rate": t.Rate.StringFixed(3)},
		"notes": t.Notes,
	})
}

// GET /api/v1/activities.csv?format=fundarr|koinly&source=…
func (s *Server) v1CSV(w http.ResponseWriter, r *http.Request) {
	all, err := s.activitiesV1(r)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "fundarr"
	}
	var rows [][]string
	switch format {
	case "fundarr":
		rows = fundarrCSV(all)
	case "koinly":
		rows = koinlyCSV(all)
	default:
		writeErr(w, 400, "format: fundarr or koinly")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="fundarr-`+format+`-`+time.Now().UTC().Format("2006-01-02")+`.csv"`)
	cw := csv.NewWriter(w)
	_ = cw.WriteAll(rows)
}

func amt(a *model.Amount) (qty, sym string) {
	if a == nil {
		return "", ""
	}
	sym = a.Asset.Symbol
	if sym == "" {
		sym = a.Asset.ISIN
	}
	return a.Qty.String(), sym
}

// fundarrCSV: every field, one row per activity.
func fundarrCSV(all []v1Activity) [][]string {
	rows := [][]string{{"id", "source", "account", "time", "type", "side", "qty", "asset", "isin", "quote_qty", "quote_asset",
		"fee_qty", "fee_asset", "cost", "currency", "transfer_with", "network", "tx_id", "address", "note"}}
	for _, a := range all {
		bq, bs := amt(&a.Base)
		qq, qs := amt(a.Quote)
		fq, fs := amt(a.Fee)
		rows = append(rows, []string{a.ID, a.Source.Name, a.Account, a.Time.UTC().Format(time.RFC3339), string(a.Type), string(a.Side),
			bq, bs, a.Base.Asset.ISIN, qq, qs, fq, fs, a.Cost, a.Currency, a.TransferWith, a.Network, a.TxID, a.Address, a.Note})
	}
	return rows
}

// koinlyCSV: Koinly's universal format, one wallet per import (filter by
// source). Moves between your own sources are left unlabelled: Koinly
// pairs them itself.
func koinlyCSV(all []v1Activity) [][]string {
	rows := [][]string{{"Date", "Sent Amount", "Sent Currency", "Received Amount", "Received Currency", "Fee Amount", "Fee Currency",
		"Net Worth Amount", "Net Worth Currency", "Label", "Description", "TxHash"}}
	for _, a := range all {
		var sq, ss, rq, rs, label string
		bq, bs := amt(&a.Base)
		qq, qs := amt(a.Quote)
		switch a.Type {
		case model.Trade:
			if a.Side == model.Sell {
				sq, ss, rq, rs = bq, bs, qq, qs
			} else {
				sq, ss, rq, rs = qq, qs, bq, bs
			}
		case model.Reward:
			rq, rs, label = bq, bs, "reward"
		case model.Dividend, model.Interest:
			rq, rs, label = bq, bs, "income"
		case model.Fee, model.Tax:
			sq, ss, label = bq, bs, "cost"
		case model.Adjustment:
			if a.Side == model.In {
				rq, rs = bq, bs
			} else {
				sq, ss = bq, bs
			}
		default:
			if a.Inflow() {
				rq, rs = bq, bs
			} else {
				sq, ss = bq, bs
			}
		}
		fq, fs := amt(a.Fee)
		desc := a.Note
		if a.Correction {
			desc = strings.TrimSpace("balance correction (Fundarr) " + desc)
		}
		rows = append(rows, []string{a.Time.UTC().Format("2006-01-02 15:04:05 UTC"), sq, ss, rq, rs, fq, fs,
			a.Cost, a.Currency, label, desc, a.TxID})
	}
	return rows
}

// ---- API tokens (managed by a signed-in user only)

func (s *Server) listTokens(w http.ResponseWriter, r *http.Request) {
	t, err := s.st.APITokens(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, t)
}

func (s *Server) createToken(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Name string `json:"name"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 60 {
		writeErr(w, 422, "give the token a name (what will use it)")
		return
	}
	t, token, err := s.st.CreateAPIToken(r.Context(), name)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"token": token, "info": t})
}

func (s *Server) deleteToken(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteAPIToken(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}
