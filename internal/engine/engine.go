// Package engine runs syncs (source → history) and pushes (history →
// destinations), on demand and on a schedule.
package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/dest"
	"github.com/Alexis-Marcel/fundarr/internal/secret"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/internal/store"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/Alexis-Marcel/fundarr/pkg/price"
	"github.com/shopspring/decimal"
)

var ErrBusy = errors.New("already running")

type Engine struct {
	Store *store.Store
	Box   *secret.Box
	HTTP  *http.Client
	Log   *slog.Logger
	// Prices values assets (corrections, rewards, transfers); nil leaves
	// them without a price.
	Prices *price.Service
	// Now is replaceable in tests.
	Now func() time.Time
	// OnChange, when set, is called after the history may have changed
	// (a sync, an import, a correction rewritten). It must not block.
	OnChange func()

	mu      sync.Mutex
	running map[string]bool
}

func New(st *store.Store, box *secret.Box, log *slog.Logger) *Engine {
	return &Engine{
		Store:   st,
		Box:     box,
		HTTP:    source.NewHTTP(),
		Log:     log,
		Now:     func() time.Time { return time.Now().UTC() },
		running: map[string]bool{},
	}
}

// ---- secrets

// Seal encrypts the secret fields of a settings map.
func (e *Engine) Seal(secrets map[string]string) ([]byte, error) {
	if len(secrets) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(secrets)
	if err != nil {
		return nil, err
	}
	return e.Box.Seal(raw), nil
}

// Values merges an entity's settings with its decrypted secrets.
func (e *Engine) Values(ent *store.Entity) (map[string]string, error) {
	out := map[string]string{}
	for k, v := range ent.Settings {
		out[k] = v
	}
	if len(ent.Secrets) > 0 {
		raw, err := e.Box.Open(ent.Secrets)
		if err != nil {
			return nil, err
		}
		var sec map[string]string
		if err := json.Unmarshal(raw, &sec); err != nil {
			return nil, err
		}
		for k, v := range sec {
			out[k] = v
		}
	}
	return out, nil
}

// Secrets returns the decrypted secrets alone (to keep them when a form is
// saved without retyping them).
func (e *Engine) Secrets(ent *store.Entity) (map[string]string, error) {
	sec := map[string]string{}
	if len(ent.Secrets) == 0 {
		return sec, nil
	}
	raw, err := e.Box.Open(ent.Secrets)
	if err != nil {
		return nil, err
	}
	return sec, json.Unmarshal(raw, &sec)
}

func (e *Engine) changed() {
	if e.OnChange != nil {
		e.OnChange()
	}
}

func (e *Engine) lock(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.running[id] {
		return false
	}
	e.running[id] = true
	return true
}

func (e *Engine) unlock(id string) {
	e.mu.Lock()
	delete(e.running, id)
	e.mu.Unlock()
}

// Running reports whether a sync or push is in progress for this id.
func (e *Engine) Running(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.running[id]
}

// runLog collects progress lines for the run record.
type runLog struct {
	mu    sync.Mutex
	lines []string
	start time.Time
}

func (l *runLog) add(format string, args ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.lines) >= 2000 {
		return
	}
	l.lines = append(l.lines, fmt.Sprintf("%6.1fs  ", time.Since(l.start).Seconds())+fmt.Sprintf(format, args...))
}

func (l *runLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return strings.Join(l.lines, "\n")
}

// ---- sync

// SyncResult summarizes one sync.
type SyncResult struct {
	Fetched int `json:"fetched"`
	New     int `json:"new"`
	// Adjusted: assets whose history did not add up and got a correction.
	Adjusted int `json:"adjusted"`
	// Gaps left unexplained (corrections turned off for the source).
	Gaps []model.Gap `json:"gaps"`
}

// Build instantiates the connector of a configured source.
func (e *Engine) Build(ent *store.Entity) (source.Source, error) {
	def, err := source.Get(ent.Kind)
	if err != nil {
		return nil, err
	}
	vals, err := e.Values(ent)
	if err != nil {
		return nil, err
	}
	return def.New(source.Config{Values: vals, HTTP: e.HTTP, State: ent.State})
}

// Sync fetches a source's history and balances, then pushes to its routes.
func (e *Engine) Sync(ctx context.Context, id string) (*SyncResult, error) {
	return e.sync0(ctx, id, false)
}

// Resync fetches the whole history again, as on the first run (after a
// connector learned new kinds of activity). Known activities are deduplicated.
func (e *Engine) Resync(ctx context.Context, id string) (*SyncResult, error) {
	return e.sync0(ctx, id, true)
}

func (e *Engine) sync0(ctx context.Context, id string, full bool) (*SyncResult, error) {
	if !e.lock(id) {
		return nil, ErrBusy
	}
	defer e.unlock(id)
	ent, err := e.Store.Source(ctx, id)
	if err != nil {
		return nil, err
	}
	runID, _ := e.Store.StartRun(ctx, "sync", id)
	log := &runLog{start: time.Now()}
	started := e.Now()
	if full {
		ent.LastOK = time.Time{}
	}
	res, state, err := e.sync(ctx, ent, log)
	if err != nil {
		log.add("error: %v", err)
	}
	_ = e.Store.SourceRan(ctx, id, started, err, state)
	summary := ""
	if res != nil {
		summary = fmt.Sprintf("%d fetched, %d new", res.Fetched, res.New)
		if res.Adjusted > 0 {
			summary += fmt.Sprintf(", %d asset(s) corrected", res.Adjusted)
		}
		if len(res.Gaps) > 0 {
			summary += fmt.Sprintf(", %d balance gap(s)", len(res.Gaps))
		}
	}
	if err != nil {
		summary = strings.TrimPrefix(summary+"; ", "; ") + err.Error()
	}
	_ = e.Store.EndRun(ctx, runID, err == nil, summary, log.String())
	e.Log.Info("sync", "source", ent.Name, "summary", summary)
	e.changed()
	if err != nil {
		return res, err
	}
	// New history (and corrections whose wait is over) goes out right away;
	// a push failure does not fail the sync.
	e.pushSource(ctx, id)
	return res, nil
}

func (e *Engine) sync(ctx context.Context, ent *store.Entity, log *runLog) (*SyncResult, []byte, error) {
	src, err := e.Build(ent)
	if err != nil {
		return nil, nil, err
	}
	// Imported statements are the reference for their period.
	stmts, err := e.Store.Statements(ctx, ent.ID)
	if err != nil {
		return nil, nil, err
	}
	covered := func(t time.Time) bool {
		for _, s := range stmts {
			if !t.Before(s.From) && !t.After(s.To) {
				return true
			}
		}
		return false
	}
	skipped := 0
	res := &SyncResult{}
	var batch []model.Activity
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		n, err := e.Store.AddActivities(ctx, batch)
		res.New += n
		batch = batch[:0]
		return err
	}
	fetch := source.Fetch{
		Since:    ent.LastOK,
		Progress: log.add,
		Emit: func(a model.Activity) error {
			if a.NativeID == "" || a.Time.IsZero() || a.Type == "" {
				return fmt.Errorf("connector emitted an incomplete activity: %+v", a)
			}
			a.SourceID = ent.ID
			a.ID = model.ActivityID(ent.ID, a.NativeID)
			a.Time = a.Time.UTC()
			if covered(a.Time) {
				skipped++
				return nil
			}
			res.Fetched++
			batch = append(batch, a)
			if len(batch) >= 500 {
				return flush()
			}
			return nil
		},
	}
	if ent.LastOK.IsZero() {
		log.add("first sync: fetching the whole history")
	} else {
		log.add("fetching since %s", ent.LastOK.Format(time.RFC3339))
	}
	if err := src.Activities(ctx, fetch); err != nil {
		_ = flush() // keep what was fetched; the next run dedupes
		return res, nil, err
	}
	if err := flush(); err != nil {
		return res, nil, err
	}
	log.add("%d activities fetched, %d new", res.Fetched, res.New)
	if skipped > 0 {
		log.add("%d activities inside an imported statement's period left to the statement", skipped)
	}
	bal, err := src.Balances(ctx)
	if err != nil {
		return res, nil, fmt.Errorf("balances: %w", err)
	}
	if err := e.Store.PutBalances(ctx, ent.ID, e.Now(), bal); err != nil {
		return res, nil, err
	}
	log.add("%d balances", len(bal))
	gaps, _, err := e.Adjust(ctx, ent)
	if err != nil {
		return res, nil, err
	}
	for _, g := range gaps {
		log.add("balance gap %s: history gives %s, platform says %s", g.Asset, g.Replayed, g.Reported)
	}
	if res.Gaps, err = e.Reconcile(ctx, ent.ID); err != nil {
		return res, nil, err
	}
	res.Adjusted = len(gaps) - len(res.Gaps)
	if res.Adjusted > 0 {
		log.add("%d asset(s) corrected so balances match; corrections wait %s before going to destinations", res.Adjusted, Hold)
	}
	return res, src.State(), nil
}

// Reconcile compares a source's replayed history with its last balances.
func (e *Engine) Reconcile(ctx context.Context, sourceID string) ([]model.Gap, error) {
	acts, err := e.Store.Activities(ctx, store.ActivityFilter{SourceID: sourceID})
	if err != nil {
		return nil, err
	}
	bal, _, err := e.Store.Balances(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	return model.Reconcile(acts, bal, e.Tolerance()), nil
}

// Tolerance is the relative difference ignored when reconciling (rounding
// of displayed balances).
func (e *Engine) Tolerance() decimal.Decimal { return decimal.New(1, -6) }

// ImportResult summarizes a statement import.
type ImportResult struct {
	From       time.Time   `json:"from"`
	To         time.Time   `json:"to"`
	Activities int         `json:"activities"`
	Replaced   int         `json:"replaced"`
	Notes      []string    `json:"notes"`
	Gaps       []model.Gap `json:"gaps"`
}

var ErrNoImport = errors.New("this source cannot import statements")

// Import reads a statement exported from the platform and makes it the
// history of the period it covers.
func (e *Engine) Import(ctx context.Context, id, name string, data []byte) (*ImportResult, error) {
	if !e.lock(id) {
		return nil, ErrBusy
	}
	defer e.unlock(id)
	ent, err := e.Store.Source(ctx, id)
	if err != nil {
		return nil, err
	}
	src, err := e.Build(ent)
	if err != nil {
		return nil, err
	}
	imp, ok := src.(source.Importer)
	if !ok {
		return nil, ErrNoImport
	}
	existing, err := e.Store.Activities(ctx, store.ActivityFilter{SourceID: id})
	if err != nil {
		return nil, err
	}
	st, err := imp.Import(name, data, existing)
	if err != nil {
		return nil, err
	}
	if len(st.Activities) == 0 || st.To.Before(st.From) {
		return nil, errors.New("the file holds no activity")
	}
	replaced := 0
	for _, a := range existing {
		if !a.Time.Before(st.From) && !a.Time.After(st.To) {
			replaced++
		}
	}
	acts := make([]model.Activity, 0, len(st.Activities))
	for _, a := range st.Activities {
		if a.NativeID == "" || a.Time.IsZero() || a.Type == "" {
			return nil, fmt.Errorf("importer produced an incomplete activity: %+v", a)
		}
		a.SourceID = id
		a.ID = model.ActivityID(id, a.NativeID)
		a.Time = a.Time.UTC()
		acts = append(acts, a)
	}
	runID, _ := e.Store.StartRun(ctx, "import", id)
	err = e.Store.ReplacePeriod(ctx, store.Statement{SourceID: id, Name: name, From: st.From.UTC(), To: st.To.UTC()}, acts)
	summary := fmt.Sprintf("%s: %d activities from %s to %s (replacing %d)", name, len(acts), st.From.Format("2006-01-02"), st.To.Format("2006-01-02"), replaced)
	if err != nil {
		summary += "; " + err.Error()
	}
	_ = e.Store.EndRun(ctx, runID, err == nil, summary, strings.Join(st.Notes, "\n"))
	if err != nil {
		return nil, err
	}
	defer e.changed()
	if _, _, err := e.Adjust(ctx, ent); err != nil {
		return nil, err
	}
	gaps, err := e.Reconcile(ctx, id)
	if err != nil {
		return nil, err
	}
	return &ImportResult{From: st.From, To: st.To, Activities: len(acts), Replaced: replaced, Notes: st.Notes, Gaps: gaps}, nil
}

// Test checks a source's credentials without syncing.
func (e *Engine) Test(ctx context.Context, ent *store.Entity) error {
	src, err := e.Build(ent)
	if err != nil {
		return err
	}
	if t, ok := src.(source.Tester); ok {
		return t.Test(ctx)
	}
	_, err = src.Balances(ctx)
	return err
}

// ---- push

// PushResult summarizes one push.
type PushResult struct {
	Counts map[dest.Status]int `json:"counts"`
}

// BuildDest instantiates a configured destination.
func (e *Engine) BuildDest(ent *store.Entity) (dest.Destination, error) {
	def, err := dest.Get(ent.Kind)
	if err != nil {
		return nil, err
	}
	vals, err := e.Values(ent)
	if err != nil {
		return nil, err
	}
	return def.New(source.Config{Values: vals, HTTP: e.HTTP})
}

func (e *Engine) pushSource(ctx context.Context, sourceID string) {
	routes, err := e.Store.Routes(ctx)
	if err != nil {
		return
	}
	for _, r := range routes {
		if r.SourceID == sourceID {
			if _, err := e.Push(ctx, r.DestinationID); err != nil && !errors.Is(err, ErrBusy) {
				e.Log.Warn("push", "destination", r.DestinationID, "err", err)
			}
		}
	}
}

// Push delivers everything not yet delivered to a destination, route by route.
func (e *Engine) Push(ctx context.Context, destID string) (*PushResult, error) {
	if !e.lock("push:" + destID) {
		return nil, ErrBusy
	}
	defer e.unlock("push:" + destID)
	ent, err := e.Store.Destination(ctx, destID)
	if err != nil {
		return nil, err
	}
	if !ent.Enabled {
		return &PushResult{Counts: map[dest.Status]int{}}, nil
	}
	if n, err := e.pending(ctx, destID); err == nil && n == 0 && ent.LastErr == "" {
		return &PushResult{Counts: map[dest.Status]int{}}, nil
	}
	runID, _ := e.Store.StartRun(ctx, "push", destID)
	log := &runLog{start: time.Now()}
	res, err := e.push(ctx, ent, log)
	msg := ""
	if err != nil {
		msg = err.Error()
		log.add("error: %v", err)
	}
	_ = e.Store.DestinationError(ctx, destID, msg)
	parts := []string{}
	for _, st := range []dest.Status{dest.Delivered, dest.Duplicate, dest.Skipped, dest.Failed} {
		if n := res.Counts[st]; n > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", n, st))
		}
	}
	summary := strings.Join(parts, ", ")
	if summary == "" {
		summary = "nothing to deliver"
	}
	if err != nil {
		summary += "; " + msg
	}
	_ = e.Store.EndRun(ctx, runID, err == nil && res.Counts[dest.Failed] == 0, summary, log.String())
	e.Log.Info("push", "destination", ent.Name, "summary", summary)
	return res, err
}

// pending counts what is waiting to go to a destination.
func (e *Engine) pending(ctx context.Context, destID string) (int, error) {
	routes, err := e.Store.Routes(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, r := range routes {
		if r.DestinationID != destID {
			continue
		}
		p, err := e.Store.Activities(ctx, store.ActivityFilter{SourceID: r.SourceID, Since: r.Since, UndeliveredTo: destID, SettledBefore: e.Now().Add(-Hold)})
		if err != nil {
			return 0, err
		}
		n += len(p)
	}
	return n, nil
}

func (e *Engine) push(ctx context.Context, ent *store.Entity, log *runLog) (*PushResult, error) {
	res := &PushResult{Counts: map[dest.Status]int{}}
	d, err := e.BuildDest(ent)
	if err != nil {
		return res, err
	}
	routes, err := e.Store.Routes(ctx)
	if err != nil {
		return res, err
	}
	// What crosses sources (transfers, costs) is computed once per push.
	var ledger *Ledger
	var currencies map[string]string
	costs := map[string]map[string]decimal.Decimal{} // currency → activity → cost
	for _, r := range routes {
		if r.DestinationID != ent.ID {
			continue
		}
		src, err := e.Store.Source(ctx, r.SourceID)
		if err != nil {
			return res, err
		}
		pending, err := e.Store.Activities(ctx, store.ActivityFilter{SourceID: r.SourceID, Since: r.Since, UndeliveredTo: ent.ID, SettledBefore: e.Now().Add(-Hold)})
		if err != nil {
			return res, err
		}
		log.add("%s → %s: %d to deliver", src.Name, r.Account, len(pending))
		if len(pending) == 0 {
			continue
		}
		if ledger == nil {
			if ledger, err = e.Ledger(ctx); err != nil {
				return res, err
			}
			currencies = map[string]string{}
			if accs, err := d.Accounts(ctx); err == nil {
				for _, a := range accs {
					currencies[a.ID] = strings.ToUpper(a.Currency)
				}
			} else {
				log.add("accounts: %v (costs left out)", err)
			}
		}
		var cost map[string]decimal.Decimal
		if ccy := currencies[r.Account]; ccy != "" {
			if cost = costs[ccy]; cost == nil {
				log.add("valuing in %s", ccy)
				cost = e.Costs(ctx, ledger, ccy)
				costs[ccy] = cost
			}
		}
		from := map[string]string{}
		names := map[string]string{}
		for _, a := range pending {
			o, ok := ledger.In[a.ID]
			if !ok {
				if o, ok = ledger.Out[a.ID]; !ok {
					continue
				}
			}
			other := ledger.ByID[o]
			if names[other.SourceID] == "" {
				names[other.SourceID] = other.SourceID
				if s, err := e.Store.Source(ctx, other.SourceID); err == nil {
					names[other.SourceID] = s.Name
				}
			}
			from[a.ID] = names[other.SourceID]
		}
		for i := 0; i < len(pending); i += 200 {
			chunk := pending[i:min(i+200, len(pending))]
			results, err := d.Push(ctx, dest.Batch{SourceName: src.Name, Account: r.Account, Activities: chunk, Costs: cost, Transfers: from})
			now := e.Now()
			var ds []store.Delivery
			seen := map[string]bool{}
			for _, r := range results {
				seen[r.ActivityID] = true
				res.Counts[r.Status]++
				ds = append(ds, store.Delivery{DestinationID: ent.ID, ActivityID: r.ActivityID, Status: string(r.Status), ExternalID: r.ExternalID, Message: r.Message, At: now})
				if r.Status == dest.Failed || (r.Status == dest.Skipped && r.Message != "") {
					log.add("%s %s: %s", r.Status, r.ActivityID, r.Message)
				}
			}
			if err != nil {
				// Whatever the destination did not report on is retried next time.
				for _, a := range chunk {
					if !seen[a.ID] {
						res.Counts[dest.Failed]++
						ds = append(ds, store.Delivery{DestinationID: ent.ID, ActivityID: a.ID, Status: string(dest.Failed), Message: err.Error(), At: now})
					}
				}
			}
			if perr := e.Store.PutDeliveries(ctx, ds); perr != nil {
				return res, perr
			}
			if err != nil {
				return res, err
			}
		}
	}
	return res, nil
}

// ---- schedule

// Run syncs enabled sources whose interval has elapsed, every minute, until
// ctx is done. Retries failed pushes once an hour.
func (e *Engine) Run(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	lastRetry := e.Now()
	for {
		e.due(ctx)
		if e.Now().Sub(lastRetry) > time.Hour {
			lastRetry = e.Now()
			if ds, err := e.Store.Destinations(ctx); err == nil {
				for _, d := range ds {
					if d.Enabled && d.LastErr != "" {
						go e.Push(ctx, d.ID)
					}
				}
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (e *Engine) due(ctx context.Context) {
	srcs, err := e.Store.Sources(ctx)
	if err != nil {
		e.Log.Warn("schedule", "err", err)
		return
	}
	now := e.Now()
	for _, s := range srcs {
		if !s.Enabled || e.Running(s.ID) {
			continue
		}
		// After a failure, wait at least 15 minutes before retrying.
		wait := s.Interval
		if s.LastErr != "" && wait > 15*time.Minute {
			wait = 15 * time.Minute
		}
		if s.LastRun.IsZero() || now.Sub(s.LastRun) >= wait {
			go func(id string) {
				if _, err := e.Sync(ctx, id); err != nil && !errors.Is(err, ErrBusy) {
					e.Log.Warn("scheduled sync", "source", id, "err", err)
				}
			}(s.ID)
		}
	}
}
