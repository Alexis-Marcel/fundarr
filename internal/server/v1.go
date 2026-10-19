package server

import (
	"encoding/csv"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
)

// The /api/v1 routes are Fundarr's stable, read-only API: any program can
// read the history with an API token (Authorization: Bearer fdr_…), so a
// tracker Fundarr does not push to still gets the data. See docs/api.md.

// reader lets in a signed-in user or a live API token, for reads only.
func (s *Server) reader(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth.user(r) == "" {
			token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
			if !ok || !s.st.CheckAPIToken(r.Context(), strings.TrimSpace(token)) {
				writeErr(w, http.StatusUnauthorized, "sign in, or send an API token: Authorization: Bearer fdr_…")
				return
			}
		}
		h(w, r)
	}
}

type v1Source struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// v1Activity is an activity with what Fundarr worked out across sources.
type v1Activity struct {
	model.Activity
	Source v1Source `json:"source"`
	// Cost: what an incoming asset cost, in Currency (bought with cash,
	// carried over a transfer, or valued at the day's price).
	Cost     string `json:"cost,omitempty"`
	Currency string `json:"currency,omitempty"`
	// TransferWith: the other side of a move between your own accounts.
	TransferWith string `json:"transferWith,omitempty"`
	// Correction: made by Fundarr's balance check, not by the platform.
	Correction bool `json:"correction,omitempty"`
}

// activitiesV1 builds the whole list, oldest first, filtered by the query
// (source, since, until).
func (s *Server) activitiesV1(r *http.Request) ([]v1Activity, error) {
	ctx := r.Context()
	l, err := s.eng.Ledger(ctx)
	if err != nil {
		return nil, err
	}
	srcs := map[string]v1Source{}
	if list, err := s.st.Sources(ctx); err == nil {
		for _, e := range list {
			srcs[e.ID] = v1Source{e.ID, e.Name, e.Kind}
		}
	}
	ccy := s.eng.Currency(ctx)
	costs := s.eng.Costs(ctx, l, ccy)
	q := r.URL.Query()
	since, _ := time.Parse(time.RFC3339, q.Get("since"))
	until, _ := time.Parse(time.RFC3339, q.Get("until"))
	out := []v1Activity{}
	for _, a := range l.Acts {
		if src := q.Get("source"); src != "" && a.SourceID != src {
			continue
		}
		if !since.IsZero() && a.Time.Before(since) || !until.IsZero() && !a.Time.Before(until) {
			continue
		}
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
	return out, nil
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
		out = append(out, v1Source{e.ID, e.Name, e.Kind})
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
		out = append(out, src{v1Source{e.ID, e.Name, e.Kind}, at, bal})
	}
	writeJSON(w, out)
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
