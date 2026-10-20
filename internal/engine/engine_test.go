package engine

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/dest"
	"github.com/Alexis-Marcel/fundarr/internal/secret"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/internal/store"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

// A fake platform: its history and balances are set by the test.
type fakeSrc struct {
	cfg  source.Config
	acts []model.Activity
	bal  []model.Balance
	err  error
}

var (
	fakeMu   sync.Mutex
	fakeActs []model.Activity
	fakeBal  []model.Balance
	fakeErr  error
	fakeSeen []source.Fetch
	sink     []dest.Batch
	sinkFail map[string]bool
	sinkErr  error
	sinkCcy  string
)

func init() {
	source.Register(source.Definition{Kind: "fake", Name: "Fake", Fields: []source.Field{{Name: "key"}, {Name: "secret", Secret: true}},
		New: func(cfg source.Config) (source.Source, error) {
			if cfg.Get("secret") != "s3cret" {
				return nil, errors.New("secret not decrypted")
			}
			fakeMu.Lock()
			defer fakeMu.Unlock()
			return &fakeSrc{cfg: cfg, acts: fakeActs, bal: fakeBal, err: fakeErr}, nil
		}})
	dest.Register(dest.Definition{Kind: "sink", Name: "Sink", New: func(cfg source.Config) (dest.Destination, error) { return sinkDest{}, nil }})
}

func (f *fakeSrc) Activities(ctx context.Context, fe source.Fetch) error {
	fakeMu.Lock()
	fakeSeen = append(fakeSeen, fe)
	fakeMu.Unlock()
	fe.Progress("fetching %d", len(f.acts))
	for _, a := range f.acts {
		if err := fe.Emit(a); err != nil {
			return err
		}
	}
	return f.err
}
func (f *fakeSrc) Balances(context.Context) ([]model.Balance, error) { return f.bal, nil }
func (f *fakeSrc) State() []byte                                     { return []byte("st") }

type sinkDest struct{}

func (sinkDest) Accounts(context.Context) ([]dest.Account, error) {
	return []dest.Account{{ID: "a1", Name: "Main", Currency: sinkCcy}}, nil
}
func (sinkDest) Push(_ context.Context, b dest.Batch) ([]dest.Result, error) {
	sink = append(sink, b)
	var out []dest.Result
	for _, a := range b.Activities {
		if sinkFail[a.NativeID] {
			out = append(out, dest.Result{ActivityID: a.ID, Status: dest.Failed, Message: "nope"})
			continue
		}
		out = append(out, dest.Result{ActivityID: a.ID, Status: dest.Delivered, ExternalID: "x-" + a.NativeID})
	}
	return out, sinkErr
}

func setup(t *testing.T) (*Engine, context.Context) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secret.New(bytes.Repeat([]byte{3}, 32))
	e := New(st, box, slog.New(slog.NewTextHandler(io.Discard, nil)))
	fakeActs, fakeBal, fakeErr, fakeSeen, sink, sinkFail, sinkErr = nil, nil, nil, nil, nil, map[string]bool{}, nil
	return e, context.Background()
}

func dep(native, qty string, at time.Time) model.Activity {
	return model.Activity{NativeID: native, Time: at, Type: model.Deposit, Base: model.Amt(model.CryptoOrFiat("BTC"), qty)}
}

func TestSyncPushReconcile(t *testing.T) {
	e, ctx := setup(t)
	sealed, err := e.Seal(map[string]string{"secret": "s3cret"})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutSource(ctx, &store.Entity{ID: "s", Kind: "fake", Name: "Fake", Settings: map[string]string{"key": "k"}, Secrets: sealed, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutDestination(ctx, &store.Entity{ID: "d", Kind: "sink", Name: "Sink", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := e.Store.PutRoute(ctx, store.Route{DestinationID: "d", SourceID: "s", Account: "a1"}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	fakeActs = []model.Activity{dep("1", "1", t0), dep("2", "0.5", t0.Add(time.Hour))}
	fakeBal = []model.Balance{{Amount: model.Amt(model.CryptoOrFiat("BTC"), "1.5")}}
	sinkFail["2"] = true
	changed := 0
	e.OnChange = func() { changed++ }

	res, err := e.Sync(ctx, "s")
	if err != nil || res.Fetched != 2 || res.New != 2 || len(res.Gaps) != 0 || changed != 1 {
		t.Fatalf("sync %+v %v, %d change(s) told", res, err, changed)
	}
	if len(sink) != 1 || sink[0].Account != "a1" || sink[0].SourceName != "Fake" || len(sink[0].Activities) != 2 {
		t.Fatalf("pushed %+v", sink)
	}
	if !fakeSeen[0].Since.IsZero() {
		t.Fatal("first sync starts from the beginning")
	}
	src, _ := e.Store.Source(ctx, "s")
	if string(src.State) != "st" || src.LastOK.IsZero() || src.LastErr != "" {
		t.Fatalf("source after sync %+v", src)
	}

	// Second run: history unchanged plus one new activity, and a gap.
	fakeActs = append(fakeActs, dep("3", "1", t0.Add(2*time.Hour)))
	sinkFail = map[string]bool{}
	res, err = e.Sync(ctx, "s")
	if err != nil || res.New != 1 || len(res.Gaps) != 0 || res.Adjusted != 1 {
		t.Fatalf("resync %+v %v", res, err)
	}
	adjs, _ := e.Store.Adjustments(ctx, "s")
	if len(adjs) != 1 || adjs[0].Side != model.Out || adjs[0].Base.Qty.String() != "1" || adjs[0].Delivered {
		t.Fatalf("correction %+v", adjs)
	}
	if !fakeSeen[1].Since.Equal(src.LastOK) {
		t.Fatal("incremental since last ok")
	}
	// The failed one is retried along with the new one; the correction waits.
	last := sink[len(sink)-1]
	if len(last.Activities) != 2 || last.Activities[0].NativeID != "2" || last.Activities[1].NativeID != "3" {
		t.Fatalf("retry %+v", last.Activities)
	}
	counts, _ := e.Store.DeliveryCounts(ctx, "d")
	if counts["delivered"] != 3 {
		t.Fatalf("counts %v", counts)
	}
	// Nothing left: a push does nothing.
	pr, err := e.Push(ctx, "d")
	if err != nil || len(pr.Counts) != 0 {
		t.Fatalf("push %+v %v", pr, err)
	}
	runs, _ := e.Store.Runs(ctx, "s", 10)
	if len(runs) != 2 || runs[0].Summary != "3 fetched, 1 new, 1 asset(s) corrected" {
		t.Fatalf("runs %+v", runs)
	}

	// A failing source keeps what it fetched and records the error.
	fakeErr = errors.New("rate limited")
	fakeActs = append(fakeActs, dep("4", "1", t0.Add(3*time.Hour)))
	if _, err := e.Sync(ctx, "s"); err == nil {
		t.Fatal("error expected")
	}
	src2, _ := e.Store.Source(ctx, "s")
	if src2.LastErr != "rate limited" || !src2.LastOK.Equal(src.LastOK) && src2.LastOK.Before(src.LastOK) {
		t.Fatalf("after error %+v", src2)
	}
	if n, _ := e.Store.CountActivities(ctx, "s"); n != 5 { // 4 and the correction
		t.Fatalf("kept %d", n)
	}

	// A destination error marks the rest failed and is reported.
	sinkErr = errors.New("destination down")
	fakeErr = nil
	if err := e.Store.ResetDeliveries(ctx, "d", "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Push(ctx, "d"); err == nil {
		t.Fatal("push error expected")
	}
	d, _ := e.Store.Destination(ctx, "d")
	if d.LastErr != "destination down" {
		t.Fatalf("dest error %q", d.LastErr)
	}
}

func TestBusyValuesTestAndSchedule(t *testing.T) {
	e, ctx := setup(t)
	sealed, _ := e.Seal(map[string]string{"secret": "s3cret"})
	ent := &store.Entity{ID: "s", Kind: "fake", Name: "Fake", Settings: map[string]string{"key": "k"}, Secrets: sealed, Enabled: true, Interval: time.Hour}
	_ = e.Store.PutSource(ctx, ent)
	vals, err := e.Values(ent)
	if err != nil || vals["key"] != "k" || vals["secret"] != "s3cret" {
		t.Fatalf("values %v %v", vals, err)
	}
	sec, _ := e.Secrets(ent)
	if sec["secret"] != "s3cret" || len(sec) != 1 {
		t.Fatal("secrets")
	}
	if s, _ := e.Seal(nil); s != nil {
		t.Fatal("nothing to seal")
	}
	if err := e.Test(ctx, ent); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Build(&store.Entity{Kind: "nope"}); err == nil {
		t.Fatal("unknown kind")
	}
	if !e.lock("s") {
		t.Fatal("lock")
	}
	if _, err := e.Sync(ctx, "s"); !errors.Is(err, ErrBusy) || !e.Running("s") {
		t.Fatal("busy expected")
	}
	e.unlock("s")

	// The scheduler syncs a never-run source, then waits for the interval.
	e.due(ctx)
	deadline := time.Now().Add(3 * time.Second)
	for {
		s, _ := e.Store.Source(ctx, "s")
		if !s.LastRun.IsZero() && !e.Running("s") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("scheduled sync did not run")
		}
		time.Sleep(10 * time.Millisecond)
	}
	fakeMu.Lock()
	n := len(fakeSeen)
	fakeMu.Unlock()
	e.due(ctx)
	time.Sleep(50 * time.Millisecond)
	fakeMu.Lock()
	defer fakeMu.Unlock()
	if len(fakeSeen) != n {
		t.Fatal("must wait for the interval")
	}
}

func TestResyncStartsFromScratch(t *testing.T) {
	e, ctx := setup(t)
	sealed, _ := e.Seal(map[string]string{"secret": "s3cret"})
	_ = e.Store.PutSource(ctx, &store.Entity{ID: "s", Kind: "fake", Name: "Fake", Secrets: sealed, Enabled: true})
	fakeActs = []model.Activity{dep("1", "1", time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC))}
	fakeBal = []model.Balance{{Amount: model.Amt(model.CryptoOrFiat("BTC"), "1")}}
	if _, err := e.Sync(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(ctx, "s"); err != nil || fakeSeen[1].Since.IsZero() {
		t.Fatal("second sync is incremental")
	}
	if _, err := e.Resync(ctx, "s"); err != nil || !fakeSeen[2].Since.IsZero() {
		t.Fatal("resync starts from the beginning")
	}
	if n, _ := e.Store.CountActivities(ctx, "s"); n != 1 {
		t.Fatal("no duplicates")
	}
}

// fakeSrc imports a "statement": one line per activity, "native qty".
func (f *fakeSrc) Import(name string, data []byte, existing []model.Activity) (*source.Statement, error) {
	t0 := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	st := &source.Statement{From: t0, To: t0.Add(2 * time.Hour), Notes: []string{"read " + name}}
	for i, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		var id, qty string
		fmt.Sscan(line, &id, &qty)
		st.Activities = append(st.Activities, dep("statement:"+id, qty, t0.Add(time.Duration(i)*time.Hour)))
	}
	return st, nil
}

func TestImportStatement(t *testing.T) {
	e, ctx := setup(t)
	sealed, _ := e.Seal(map[string]string{"secret": "s3cret"})
	_ = e.Store.PutSource(ctx, &store.Entity{ID: "s", Kind: "fake", Name: "Fake", Secrets: sealed, Enabled: true})
	t0 := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	// The API knows one activity inside the statement's period and one after.
	fakeActs = []model.Activity{dep("api-in", "5", t0.Add(30*time.Minute)), dep("api-after", "1", t0.Add(48*time.Hour))}
	fakeBal = []model.Balance{{Amount: model.Amt(model.CryptoOrFiat("BTC"), "4")}}
	if _, err := e.Sync(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	res, err := e.Import(ctx, "s", "stmt.csv", []byte("a 1\nb 2\n"))
	if err != nil || res.Activities != 2 || res.Replaced != 1 || len(res.Gaps) != 0 {
		t.Fatalf("import %+v %v", res, err)
	}
	acts, _ := e.Store.Activities(ctx, store.ActivityFilter{SourceID: "s"})
	if len(acts) != 3 || acts[0].NativeID != "statement:a" || acts[2].NativeID != "api-after" {
		t.Fatalf("history %+v", acts)
	}
	// A later full sync leaves the period to the statement.
	if _, err := e.Resync(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	if n, _ := e.Store.CountActivities(ctx, "s"); n != 3 {
		t.Fatalf("api activity came back inside the statement period: %d", n)
	}
	sts, _ := e.Store.Statements(ctx, "s")
	if len(sts) != 1 || sts[0].Activities != 2 || sts[0].Name != "stmt.csv" {
		t.Fatalf("statements %+v", sts)
	}
	runs, _ := e.Store.Runs(ctx, "s", 10)
	found := false
	for _, r := range runs {
		if r.Kind == "import" && r.Log == "read stmt.csv" {
			found = true
		}
	}
	if !found {
		t.Fatalf("import run %+v", runs)
	}
}

func TestCorrectionsWaitAndReclassify(t *testing.T) {
	e, ctx := setup(t)
	now := time.Date(2026, 1, 10, 0, 0, 0, 0, time.UTC)
	e.Now = func() time.Time { return now }
	e.Store.Clock = e.Now
	sealed, _ := e.Seal(map[string]string{"secret": "s3cret"})
	_ = e.Store.PutSource(ctx, &store.Entity{ID: "s", Kind: "fake", Name: "Fake", Secrets: sealed, Enabled: true})
	_ = e.Store.PutDestination(ctx, &store.Entity{ID: "d", Kind: "sink", Name: "Sink", Enabled: true})
	_ = e.Store.PutRoute(ctx, store.Route{DestinationID: "d", SourceID: "s", Account: "a1"})
	// 1 BTC arrived, 3 are held: 2 are missing from the history.
	fakeActs = []model.Activity{dep("1", "1", time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC))}
	fakeBal = []model.Balance{{Amount: model.Amt(model.CryptoOrFiat("BTC"), "3")}}
	res, err := e.Sync(ctx, "s")
	if err != nil || res.Adjusted != 1 || len(res.Gaps) != 0 {
		t.Fatalf("sync %+v %v", res, err)
	}
	if len(sink) != 1 || len(sink[0].Activities) != 1 {
		t.Fatalf("the correction waits: %+v", sink)
	}
	// The same gap the next day: the same correction, still waiting.
	now = now.Add(24 * time.Hour)
	e.Sync(ctx, "s")
	adjs, _ := e.Store.Adjustments(ctx, "s")
	if len(adjs) != 1 || !adjs[0].Created.Before(now.Add(-23*time.Hour)) || len(sink) != 1 {
		t.Fatalf("stable %+v, %d pushes", adjs, len(sink))
	}
	// The user says what it was: a purchase for 60000 EUR.
	_, err = e.Reclassify(ctx, "s", adjs[0].ID, Reclassify{As: "buy", Quote: model.Amt(model.CryptoOrFiat("EUR"), "60000")})
	if err != nil {
		t.Fatal(err)
	}
	// The BTC correction is gone; the EUR paid, which the history does not
	// show arriving, gets its own.
	if adjs, _ := e.Store.Adjustments(ctx, "s"); len(adjs) != 1 || adjs[0].Base.Asset.Symbol != "EUR" || adjs[0].Side != model.In {
		t.Fatalf("replaced %+v", adjs)
	}
	acts, _ := e.Store.Activities(ctx, store.ActivityFilter{SourceID: "s"})
	var manual model.Activity
	for _, a := range acts {
		if strings.HasPrefix(a.NativeID, model.ManualPrefix) {
			manual = a
		}
	}
	if manual.Type != model.Trade || manual.Quote == nil || manual.Quote.Asset.Kind != model.Fiat || manual.Base.Qty.String() != "2" {
		t.Fatalf("manual %+v", manual)
	}
	if _, err := e.Reclassify(ctx, "s", manual.ID, Reclassify{As: "reward"}); err == nil {
		t.Fatal("only corrections are reclassified")
	}
	// Removing it brings the correction back; after the wait it goes out.
	if err := e.DeleteManual(ctx, "s", manual.ID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(Hold + time.Hour)
	e.Sync(ctx, "s")
	last := sink[len(sink)-1]
	if len(last.Activities) != 1 || last.Activities[0].Type != model.Adjustment {
		t.Fatalf("pushed %+v", last.Activities)
	}
	adjs, _ = e.Store.Adjustments(ctx, "s")
	if len(adjs) != 1 || !adjs[0].Delivered {
		t.Fatalf("delivered %+v", adjs)
	}
	if _, err := e.Reclassify(ctx, "s", adjs[0].ID, Reclassify{As: "reward"}); !errors.Is(err, ErrDelivered) {
		t.Fatalf("delivered corrections stay: %v", err)
	}
	// History catches up: a counter-correction, since the first one went out.
	fakeActs = append(fakeActs, dep("2", "2", time.Date(2025, 6, 1, 0, 0, 0, 0, time.UTC)))
	e.Sync(ctx, "s")
	adjs, _ = e.Store.Adjustments(ctx, "s")
	if len(adjs) != 2 || adjs[1].Side != model.Out || adjs[1].Base.Qty.String() != "2" {
		t.Fatalf("counter %+v", adjs)
	}
	// Turned off: pending corrections go away, delivered ones stay.
	ent, _ := e.Store.Source(ctx, "s")
	ent.Settings = map[string]string{AdjustSetting: "off"}
	_ = e.Store.PutSource(ctx, ent)
	res, _ = e.Sync(ctx, "s")
	if adjs, _ := e.Store.Adjustments(ctx, "s"); len(adjs) != 1 || len(res.Gaps) != 1 {
		t.Fatalf("off %+v %+v", adjs, res)
	}
}

func TestPushCarriesTransfersAndCosts(t *testing.T) {
	e, ctx := setup(t)
	sealed, _ := e.Seal(map[string]string{"secret": "s3cret"})
	for _, id := range []string{"ex", "wallet"} {
		_ = e.Store.PutSource(ctx, &store.Entity{ID: id, Kind: "fake", Name: strings.ToUpper(id), Secrets: sealed, Enabled: false})
	}
	_ = e.Store.PutDestination(ctx, &store.Entity{ID: "d", Kind: "sink", Name: "Sink", Enabled: true})
	_ = e.Store.PutRoute(ctx, store.Route{DestinationID: "d", SourceID: "wallet", Account: "a1"})
	t0 := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	eur := model.Amt(model.CryptoOrFiat("EUR"), "30000")
	buy := model.Activity{NativeID: "b", Time: t0, Type: model.Trade, Side: model.Buy, Base: model.Amt(model.CryptoOrFiat("BTC"), "1"), Quote: &eur}
	out := model.Activity{NativeID: "w", Time: t0.Add(time.Hour), Type: model.Withdrawal, Base: model.Amt(model.CryptoOrFiat("BTC"), "1"), TxID: "aaaaaaaaaaaaaaaaaaaaaaaa"}
	in := model.Activity{NativeID: "d", Time: t0.Add(2 * time.Hour), Type: model.Deposit, Base: model.Amt(model.CryptoOrFiat("BTC"), "1"), TxID: "aaaaaaaaaaaaaaaaaaaaaaaa"}
	for src, a := range map[string]model.Activity{"ex": buy, "wallet": in} {
		a.SourceID, a.ID = src, model.ActivityID(src, a.NativeID)
		e.Store.AddActivities(ctx, []model.Activity{a})
	}
	out.SourceID, out.ID = "ex", model.ActivityID("ex", "w")
	e.Store.AddActivities(ctx, []model.Activity{out})
	sinkCcy = "EUR"
	defer func() { sinkCcy = "" }()
	if _, err := e.Push(ctx, "d"); err != nil {
		t.Fatal(err)
	}
	b := sink[0]
	id := model.ActivityID("wallet", "d")
	if b.Transfers[id] != "EX" || b.Costs[id].String() != "30000" {
		t.Fatalf("batch %+v", b)
	}
}
