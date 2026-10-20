package server

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"github.com/Alexis-Marcel/fundarr/internal/store"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

// ---- balance corrections

type adjustmentView struct {
	store.Adjustment
	// ReleaseAt: when it goes to destinations (unless explained before).
	ReleaseAt time.Time `json:"releaseAt"`
	Value     string    `json:"value,omitempty"` // in Currency, at the day's price
	Currency  string    `json:"currency"`
}

// value prices an amount without letting a slow provider hold the page.
func (s *Server) value(ctx context.Context, a model.Amount, at time.Time, ccy string) string {
	if s.eng.Prices == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	p, err := s.eng.Prices.Price(ctx, a.Asset, at, ccy)
	if err != nil {
		return ""
	}
	return p.Mul(a.Qty).StringFixed(2)
}

func (s *Server) adjustments(w http.ResponseWriter, r *http.Request) {
	adjs, err := s.st.Adjustments(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ccy := s.eng.Currency(r.Context())
	out := []adjustmentView{}
	for _, a := range adjs {
		out = append(out, adjustmentView{Adjustment: a, ReleaseAt: a.Created.Add(engine.Hold), Currency: ccy,
			Value: s.value(r.Context(), a.Base, a.Time, ccy)})
	}
	writeJSON(w, out)
}

func (s *Server) reclassify(w http.ResponseWriter, r *http.Request) {
	var in engine.Reclassify
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a, err := s.eng.Reclassify(r.Context(), r.PathValue("id"), r.PathValue("aid"), in)
	switch {
	case errors.Is(err, engine.ErrBusy):
		writeErr(w, 409, "a sync is running; try again when it is done")
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, "no such correction (a sync may have replaced it)")
	case errors.Is(err, engine.ErrDelivered):
		writeErr(w, 409, err.Error())
	case err != nil:
		writeErr(w, 422, err.Error())
	default:
		writeJSON(w, a)
	}
}

func (s *Server) deleteManual(w http.ResponseWriter, r *http.Request) {
	err := s.eng.DeleteManual(r.Context(), r.PathValue("id"), r.PathValue("aid"))
	switch {
	case errors.Is(err, engine.ErrBusy):
		writeErr(w, 409, "a sync is running; try again when it is done")
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, 404, "no such activity")
	case err != nil:
		writeErr(w, 422, err.Error())
	default:
		writeJSON(w, map[string]bool{"ok": true})
	}
}

// ---- transfers between your own accounts

type sideView struct {
	model.Activity
	Source string `json:"source"` // its source's name
}

type pairView struct {
	By  string   `json:"by"`
	Out sideView `json:"out"`
	In  sideView `json:"in"`
	// Cost carried from one account to the other, in Currency.
	Cost string `json:"cost,omitempty"`
}

type unmatchedView struct {
	Out        sideView   `json:"out"`
	Candidates []sideView `json:"candidates"`
}

func (s *Server) transfers(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	l, err := s.eng.Ledger(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	names := map[string]string{}
	if srcs, err := s.st.Sources(ctx); err == nil {
		for _, src := range srcs {
			names[src.ID] = src.Name
		}
	}
	side := func(a model.Activity) sideView { return sideView{Activity: a, Source: names[a.SourceID]} }
	ccy := s.eng.Currency(ctx)
	costs := s.eng.Costs(ctx, l, ccy)
	pairs := []pairView{}
	for i := len(l.Pairs) - 1; i >= 0; i-- {
		p := l.Pairs[i]
		v := pairView{By: p.By, Out: side(l.ByID[p.OutID]), In: side(l.ByID[p.InID])}
		if c, ok := costs[p.InID]; ok {
			v.Cost = c.StringFixed(2)
		}
		pairs = append(pairs, v)
	}
	// Crypto that left a source and was not seen arriving on another one.
	var free []model.Activity
	for _, a := range l.Acts {
		if model.Transferable(a) && a.Type == model.Deposit && l.In[a.ID] == "" {
			free = append(free, a)
		}
	}
	unmatched := []unmatchedView{}
	for i := len(l.Acts) - 1; i >= 0 && len(unmatched) < 200; i-- {
		o := l.Acts[i]
		if !model.Transferable(o) || o.Type != model.Withdrawal || l.Out[o.ID] != "" {
			continue
		}
		u := unmatchedView{Out: side(o), Candidates: []sideView{}}
		for _, in := range free {
			if model.CanPair(o, in) {
				u.Candidates = append(u.Candidates, side(in))
			}
		}
		sort.Slice(u.Candidates, func(a, b int) bool {
			return u.Candidates[a].Time.Sub(o.Time).Abs() < u.Candidates[b].Time.Sub(o.Time).Abs()
		})
		unmatched = append(unmatched, u)
	}
	writeJSON(w, map[string]any{"currency": ccy, "pairs": pairs, "unmatched": unmatched, "sources": len(names)})
}

func (s *Server) putTransfer(w http.ResponseWriter, r *http.Request) {
	var in store.TransferLink
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.OutID == "" || in.InID == "" {
		writeErr(w, 400, "outId and inId are required")
		return
	}
	if in.Kind == "pair" {
		l, err := s.eng.Ledger(r.Context())
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		o, okO := l.ByID[in.OutID]
		i, okI := l.ByID[in.InID]
		if !okO || !okI || !model.CanPair(o, i) {
			writeErr(w, 422, "these two cannot be one transfer: same asset, two different sources, at most 45 days apart")
			return
		}
	}
	if err := s.st.PutTransferLink(r.Context(), in); err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	s.changed()
	writeJSON(w, map[string]bool{"ok": true})
}

// ---- settings

var currencies = []string{"EUR", "USD", "GBP", "CHF", "CAD", "AUD", "JPY", "SEK", "NOK", "DKK", "PLN"}

func (s *Server) getSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"currency": s.eng.Currency(r.Context()), "currencies": currencies, "prices": s.eng.Prices != nil})
}

func (s *Server) putSettings(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Currency string `json:"currency"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c := strings.ToUpper(strings.TrimSpace(in.Currency))
	ok := false
	for _, x := range currencies {
		ok = ok || x == c
	}
	if !ok {
		writeErr(w, 422, "unsupported currency")
		return
	}
	if err := s.st.PutSetting(r.Context(), "currency", c); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.getSettings(w, r)
}
