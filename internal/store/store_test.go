package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func act(src, native string, at time.Time) model.Activity {
	return model.Activity{ID: model.ActivityID(src, native), SourceID: src, NativeID: native, Time: at, Type: model.Deposit,
		Base: model.Amt(model.CryptoOrFiat("BTC"), "1")}
}

func TestStoreFlow(t *testing.T) {
	ctx := context.Background()
	s := open(t)
	if err := s.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	src := &Entity{ID: "s1", Kind: "binance", Name: "Binance", Settings: map[string]string{"x": "1"}, Secrets: []byte{1, 2}, Enabled: true}
	if err := s.PutSource(ctx, src); err != nil {
		t.Fatal(err)
	}
	got, err := s.Source(ctx, "s1")
	if err != nil || got.Name != "Binance" || got.Settings["x"] != "1" || got.Interval != 6*time.Hour || !got.Enabled || len(got.Secrets) != 2 {
		t.Fatalf("source %+v %v", got, err)
	}
	if _, err := s.Source(ctx, "nope"); err != ErrNotFound {
		t.Fatal("missing source")
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	if err := s.SourceRan(ctx, "s1", now, nil, []byte("cursor")); err != nil {
		t.Fatal(err)
	}
	if err := s.SourceRan(ctx, "s1", now.Add(time.Hour), errFake{}, []byte("ignored")); err != nil {
		t.Fatal(err)
	}
	got, _ = s.Source(ctx, "s1")
	if string(got.State) != "cursor" || !got.LastOK.Equal(now) || got.LastErr != "boom" || !got.LastRun.Equal(now.Add(time.Hour)) {
		t.Fatalf("ran %+v", got)
	}

	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	n, err := s.AddActivities(ctx, []model.Activity{act("s1", "a", t0), act("s1", "b", t0.Add(time.Hour))})
	if err != nil || n != 2 {
		t.Fatalf("add %d %v", n, err)
	}
	n, _ = s.AddActivities(ctx, []model.Activity{act("s1", "a", t0), act("s1", "c", t0.Add(2*time.Hour))})
	if n != 1 {
		t.Fatalf("dedupe: %d", n)
	}
	all, _ := s.Activities(ctx, ActivityFilter{SourceID: "s1"})
	if len(all) != 3 || all[0].NativeID != "a" || !all[0].Base.Qty.IsPositive() {
		t.Fatalf("list %+v", all)
	}
	if c, _ := s.CountActivities(ctx, "s1"); c != 3 {
		t.Fatal("count")
	}
	page, _ := s.Activities(ctx, ActivityFilter{Since: t0.Add(time.Hour), Limit: 1, Offset: 1})
	if len(page) != 1 || page[0].NativeID != "c" {
		t.Fatalf("page %+v", page)
	}

	if err := s.PutBalances(ctx, "s1", now, []model.Balance{{Amount: model.Amt(model.CryptoOrFiat("BTC"), "3")}}); err != nil {
		t.Fatal(err)
	}
	b, at, _ := s.Balances(ctx, "s1")
	if len(b) != 1 || !at.Equal(now) {
		t.Fatal("balances")
	}
	if b, _, _ := s.Balances(ctx, "none"); b != nil {
		t.Fatal("no balances")
	}

	d := &Entity{ID: "d1", Kind: "csv", Name: "CSV", Enabled: true}
	if err := s.PutDestination(ctx, d); err != nil {
		t.Fatal(err)
	}
	if err := s.PutRoute(ctx, Route{DestinationID: "d1", SourceID: "s1", Account: "acc"}); err != nil {
		t.Fatal(err)
	}
	rs, _ := s.Routes(ctx)
	if len(rs) != 1 || rs[0].Account != "acc" || !rs[0].Since.IsZero() {
		t.Fatalf("routes %+v", rs)
	}
	ds := []Delivery{
		{DestinationID: "d1", ActivityID: model.ActivityID("s1", "a"), Status: "delivered", At: now},
		{DestinationID: "d1", ActivityID: model.ActivityID("s1", "b"), Status: "failed", At: now},
	}
	if err := s.PutDeliveries(ctx, ds); err != nil {
		t.Fatal(err)
	}
	pending, _ := s.Activities(ctx, ActivityFilter{UndeliveredTo: "d1"})
	if len(pending) != 2 || pending[0].NativeID != "b" {
		t.Fatalf("pending %+v", pending)
	}
	counts, _ := s.DeliveryCounts(ctx, "d1")
	if counts["delivered"] != 1 || counts["failed"] != 1 {
		t.Fatalf("counts %v", counts)
	}
	if err := s.ResetDeliveries(ctx, "d1", "s1"); err != nil {
		t.Fatal(err)
	}
	if counts, _ := s.DeliveryCounts(ctx, "d1"); len(counts) != 0 {
		t.Fatal("reset")
	}

	open1, _ := s.StartRun(ctx, "sync", "s1")
	if n, err := s.CloseInterrupted(ctx); err != nil || n != 1 {
		t.Fatalf("interrupted %d %v", n, err)
	}
	if rs, _ := s.Runs(ctx, "s1", 5); len(rs) != 1 || rs[0].ID != open1 || *rs[0].OK || rs[0].Summary != "interrupted (Fundarr stopped)" {
		t.Fatalf("closed %+v", rs)
	}
	id, _ := s.StartRun(ctx, "sync", "s1")
	if err := s.EndRun(ctx, id, true, "3 new", "log"); err != nil {
		t.Fatal(err)
	}
	runs, _ := s.Runs(ctx, "s1", 10)
	if len(runs) != 2 || runs[0].OK == nil || !*runs[0].OK || runs[0].Summary != "3 new" {
		t.Fatalf("runs %+v", runs)
	}

	// Deleting a source removes its history, balances, routes and deliveries.
	if err := s.DeleteSource(ctx, "s1"); err != nil {
		t.Fatal(err)
	}
	if rest, _ := s.Activities(ctx, ActivityFilter{}); len(rest) != 0 {
		t.Fatal("cascade")
	}
	if rs, _ := s.Routes(ctx); len(rs) != 0 {
		t.Fatal("route cascade")
	}
	if err := s.DeleteDestination(ctx, "d1"); err != nil {
		t.Fatal(err)
	}
	if ds, _ := s.Destinations(ctx); len(ds) != 0 {
		t.Fatal("dest delete")
	}
}

type errFake struct{}

func (errFake) Error() string { return "boom" }
