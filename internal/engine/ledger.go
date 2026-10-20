package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/store"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// Hold is how long a balance correction waits before going to destinations:
// a platform's history sometimes catches up a sync or two later.
const Hold = 48 * time.Hour

// AdjustSetting, in a source's settings, turns balance corrections off ("off").
const AdjustSetting = "fundarr.adjust"

// Currency returns the currency values are shown in (Settings, default EUR).
func (e *Engine) Currency(ctx context.Context) string {
	if c, _ := e.Store.Setting(ctx, "currency"); c != "" {
		return c
	}
	return "EUR"
}

// Adjust brings a source's corrections in line with its gaps: corrections
// not delivered anywhere yet are recomputed from scratch; delivered ones
// stay and are corrected by new ones if they turn out wrong. Returns the
// gaps that existed before correcting.
func (e *Engine) Adjust(ctx context.Context, ent *store.Entity) (gaps []model.Gap, changed bool, err error) {
	acts, err := e.Store.Activities(ctx, store.ActivityFilter{SourceID: ent.ID})
	if err != nil {
		return nil, false, err
	}
	bal, at, err := e.Store.Balances(ctx, ent.ID)
	if err != nil || at.IsZero() {
		return nil, false, err
	}
	adjs, err := e.Store.Adjustments(ctx, ent.ID)
	if err != nil {
		return nil, false, err
	}
	pending := map[string]bool{}
	for _, a := range adjs {
		if !a.Delivered {
			pending[a.ID] = true
		}
	}
	kept := acts[:0:0]
	for _, a := range acts {
		if !pending[a.ID] {
			kept = append(kept, a)
		}
	}
	gaps = model.Reconcile(kept, bal, e.Tolerance())
	var want []model.Activity
	if ent.Settings[AdjustSetting] != "off" {
		want = model.Adjustments(kept, gaps, e.Now())
	}
	wanted := map[string]bool{}
	for i := range want {
		want[i].SourceID = ent.ID
		want[i].ID = model.ActivityID(ent.ID, want[i].NativeID)
		wanted[want[i].ID] = true
	}
	var drop []string
	for id := range pending {
		if !wanted[id] {
			drop = append(drop, id)
		}
	}
	if len(drop) > 0 {
		if err := e.Store.DeleteActivities(ctx, ent.ID, drop); err != nil {
			return gaps, false, err
		}
	}
	n, err := e.Store.AddActivities(ctx, want)
	return gaps, n > 0 || len(drop) > 0, err
}

// Readjust recomputes a source's corrections now (after a settings change).
func (e *Engine) Readjust(ctx context.Context, id string) error {
	if !e.lock(id) {
		return ErrBusy
	}
	defer e.unlock(id)
	ent, err := e.Store.Source(ctx, id)
	if err != nil {
		return err
	}
	defer e.changed()
	_, _, err = e.Adjust(ctx, ent)
	return err
}

// Reclassify replaces a balance correction with what really happened, told
// by the user: a reward, a transfer from another wallet of theirs, a
// purchase (or a sale) for some amount of another asset.
type Reclassify struct {
	As    string       `json:"as"` // "reward", "deposit", "withdrawal", "buy", "sell", "fee"
	Time  time.Time    `json:"time"`
	Quote model.Amount `json:"quote"` // buy and sell: what was paid or received
	Note  string       `json:"note"`
}

var ErrDelivered = errors.New("this correction was already sent to a destination: remove it there first, then forget its delivery")

func (e *Engine) Reclassify(ctx context.Context, sourceID, activityID string, r Reclassify) (*model.Activity, error) {
	if !e.lock(sourceID) {
		return nil, ErrBusy
	}
	defer e.unlock(sourceID)
	adjs, err := e.Store.Adjustments(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	var adj *store.Adjustment
	for i := range adjs {
		if adjs[i].ID == activityID {
			adj = &adjs[i]
		}
	}
	if adj == nil {
		return nil, store.ErrNotFound
	}
	if adj.Delivered {
		return nil, ErrDelivered
	}
	in := adj.Side == model.In
	a := model.Activity{Time: adj.Time, Base: adj.Base, Note: strings.TrimSpace(r.Note)}
	if !r.Time.IsZero() {
		a.Time = r.Time.UTC()
	}
	switch r.As {
	case "reward", "deposit":
		if !in {
			return nil, errors.New("coins that left cannot be a reward or a deposit")
		}
		a.Type = model.Reward
		if r.As == "deposit" {
			a.Type = model.Deposit
		}
	case "withdrawal", "fee":
		if in {
			return nil, errors.New("coins that arrived cannot be a withdrawal or a fee")
		}
		a.Type = model.Withdrawal
		if r.As == "fee" {
			a.Type = model.Fee
		}
	case "buy", "sell":
		if (r.As == "buy") != in {
			return nil, errors.New("a purchase brings coins in, a sale takes them out")
		}
		r.Quote.Asset = model.CryptoOrFiat(r.Quote.Asset.Symbol)
		if r.Quote.Asset.Symbol == "" || !r.Quote.Qty.IsPositive() {
			return nil, errors.New("say what was paid or received, and how much")
		}
		if r.Quote.Asset.Key() == a.Base.Asset.Key() {
			return nil, errors.New("an asset cannot be traded for itself")
		}
		q := r.Quote
		a.Type, a.Quote, a.Side = model.Trade, &q, model.Buy
		if r.As == "sell" {
			a.Side = model.Sell
		}
	default:
		return nil, fmt.Errorf("unknown kind %q", r.As)
	}
	if a.Note == "" {
		a.Note = "entered in Fundarr"
	}
	b := make([]byte, 6)
	rand.Read(b)
	a.NativeID = model.ManualPrefix + hex.EncodeToString(b)
	a.SourceID, a.ID = sourceID, model.ActivityID(sourceID, a.NativeID)
	defer e.changed()
	if _, err := e.Store.AddActivities(ctx, []model.Activity{a}); err != nil {
		return nil, err
	}
	if err := e.Store.DeleteActivities(ctx, sourceID, []string{adj.ID}); err != nil {
		return nil, err
	}
	ent, err := e.Store.Source(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	if _, _, err := e.Adjust(ctx, ent); err != nil {
		return nil, err
	}
	return &a, nil
}

// DeleteManual removes an activity the user entered; the balance check
// corrects again whatever it explained.
func (e *Engine) DeleteManual(ctx context.Context, sourceID, activityID string) error {
	if !e.lock(sourceID) {
		return ErrBusy
	}
	defer e.unlock(sourceID)
	a, err := e.Store.Activity(ctx, sourceID, activityID)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(a.NativeID, model.ManualPrefix) {
		return errors.New("only activities entered in Fundarr can be deleted")
	}
	defer e.changed()
	if err := e.Store.DeleteActivities(ctx, sourceID, []string{activityID}); err != nil {
		return err
	}
	ent, err := e.Store.Source(ctx, sourceID)
	if err != nil {
		return err
	}
	_, _, err = e.Adjust(ctx, ent)
	return err
}

// ---- transfers between your own accounts

// Transfers pairs withdrawals and deposits across every source.
func (e *Engine) Transfers(ctx context.Context, acts []model.Activity) ([]model.Pair, error) {
	links, err := e.Store.TransferLinks(ctx)
	if err != nil {
		return nil, err
	}
	var forced []model.Pair
	refused := map[string]bool{}
	for _, l := range links {
		if l.Kind == "pair" {
			forced = append(forced, model.Pair{OutID: l.OutID, InID: l.InID})
		} else {
			refused[l.OutID+"\x00"+l.InID] = true
		}
	}
	return model.PairTransfers(acts, forced, refused), nil
}

// Ledger is everything that crosses sources, computed together.
type Ledger struct {
	Acts  []model.Activity
	Pairs []model.Pair
	// In maps a deposit to its withdrawal, Out a withdrawal to its deposit.
	In, Out map[string]string
	ByID    map[string]model.Activity
}

func (e *Engine) Ledger(ctx context.Context) (*Ledger, error) {
	acts, err := e.Store.Activities(ctx, store.ActivityFilter{})
	if err != nil {
		return nil, err
	}
	pairs, err := e.Transfers(ctx, acts)
	if err != nil {
		return nil, err
	}
	l := &Ledger{Acts: acts, Pairs: pairs, In: map[string]string{}, Out: map[string]string{}, ByID: map[string]model.Activity{}}
	for _, a := range acts {
		l.ByID[a.ID] = a
	}
	for _, p := range pairs {
		l.In[p.InID], l.Out[p.OutID] = p.OutID, p.InID
	}
	return l, nil
}

// Costs is what each incoming asset cost, in ccy (see model.Costs). Without
// a price service, only what was bought with ccy itself is known.
func (e *Engine) Costs(ctx context.Context, l *Ledger, ccy string) map[string]decimal.Decimal {
	value := func(a model.Amount, _ time.Time) (decimal.Decimal, bool) {
		if a.Asset.Kind == model.Fiat && a.Asset.Symbol == ccy {
			return a.Qty, true
		}
		return decimal.Zero, false
	}
	if e.Prices != nil {
		value = e.Prices.Valuer(ctx, ccy)
	}
	return model.Costs(l.Acts, l.In, value)
}

// UnitPrice prices one unit of an asset on the day of at, in ccy. Without a
// price service, only ccy itself is known.
func (e *Engine) UnitPrice(ctx context.Context, ccy string) func(model.Asset, time.Time) (decimal.Decimal, bool) {
	return func(a model.Asset, at time.Time) (decimal.Decimal, bool) {
		if a.Kind == model.Fiat && a.Symbol == ccy {
			return decimal.NewFromInt(1), true
		}
		if e.Prices == nil {
			return decimal.Zero, false
		}
		return e.Prices.UnitPrice(ctx, ccy)(a, at)
	}
}
