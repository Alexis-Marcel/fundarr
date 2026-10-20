package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/demo"
	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"github.com/Alexis-Marcel/fundarr/internal/secret"
	"github.com/Alexis-Marcel/fundarr/internal/store"
	"github.com/Alexis-Marcel/fundarr/pkg/api"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

// feedPage: what a Go app decodes the feed into.
type feedPage = api.Changes

// changes reads one page of the feed.
func changes(t *testing.T, base, after string, limit int) (feedPage, int) {
	t.Helper()
	u := base + "/api/v1/changes?limit=" + strconv.Itoa(limit)
	if after != "" {
		u += "&after=" + after
	}
	var p feedPage
	return p, do(t, http.DefaultClient, "GET", u, nil, &p)
}

// mirror applies pages to a copy until the end, as an app would.
func mirror(t *testing.T, base string, copy map[string]v1Activity, cursor string, limit int) string {
	t.Helper()
	for {
		p, code := changes(t, base, cursor, limit)
		if code != 200 {
			t.Fatalf("changes %d", code)
		}
		for _, c := range p.Changes {
			if c.Op == "remove" {
				delete(copy, c.ID)
			} else {
				copy[c.ID] = *c.Activity
			}
		}
		cursor = p.Cursor
		if !p.More {
			return cursor
		}
	}
}

func feedServer(t *testing.T) (*httptest.Server, *Server, *engine.Engine) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secret.New(bytes.Repeat([]byte{9}, 32))
	eng := engine.New(st, box, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(ctx, Config{Engine: eng, Auth: AuthConfig{Mode: "none"}, Background: ctx})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	return ts, srv, eng
}

func btc(id, src, native string, at time.Time, typ model.Type, qty string) model.Activity {
	a := model.Activity{SourceID: src, NativeID: native, Time: at, Type: typ, Base: model.Amt(model.CryptoOrFiat("BTC"), qty)}
	a.ID = model.ActivityID(src, native)
	return a
}

func TestChangesFeed(t *testing.T) {
	ts, srv, eng := feedServer(t)
	ctx := context.Background()
	st := eng.Store
	for _, id := range []string{"s1", "s2"} {
		if err := st.PutSource(ctx, &store.Entity{ID: id, Kind: "test", Name: strings.ToUpper(id), Settings: map[string]string{}}); err != nil {
			t.Fatal(err)
		}
	}
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	out := btc("", "s1", "out", t0, model.Withdrawal, "1")
	in := btc("", "s2", "in", t0.Add(time.Hour), model.Deposit, "1")
	reward := btc("", "s1", "rw", t0.Add(2*time.Hour), model.Reward, "0.1")
	corr := btc("", "s1", model.AdjustPrefix+"x", t0.Add(3*time.Hour), model.Adjustment, "0.5")
	corr.Side = model.In
	if _, err := st.AddActivities(ctx, []model.Activity{reward, in, out, corr}); err != nil {
		t.Fatal(err)
	}
	eng.OnChange()

	// The first copy, two by two, oldest first, transfers paired.
	copy := map[string]v1Activity{}
	p, _ := changes(t, ts.URL, "", 2)
	if len(p.Changes) != 2 || !p.More || p.Changes[0].ID != out.ID || p.Changes[1].ID != in.ID {
		t.Fatalf("first page %+v", p)
	}
	cur := mirror(t, ts.URL, copy, "", 2)
	if len(copy) != 4 || copy[out.ID].TransferWith != in.ID || copy[in.ID].TransferWith != out.ID || !copy[corr.ID].Correction ||
		copy[in.ID].Source.Name != "S2" || copy[out.ID].Cost != "" {
		t.Fatalf("copy %+v", copy)
	}
	// Nothing new: nothing returned, same cursor.
	if p, _ := changes(t, ts.URL, cur, 10); len(p.Changes) != 0 || p.Cursor != cur || p.More {
		t.Fatalf("idle %+v", p)
	}

	// A statement replaces the correction's period; the reward stays.
	stmt := btc("", "s1", "stmt", t0.Add(3*time.Hour), model.Deposit, "0.5")
	if err := st.ReplacePeriod(ctx, store.Statement{SourceID: "s1", Name: "x.csv", From: t0.Add(150 * time.Minute), To: t0.Add(4 * time.Hour)}, []model.Activity{stmt}); err != nil {
		t.Fatal(err)
	}
	eng.OnChange()
	p, _ = changes(t, ts.URL, cur, 10)
	if len(p.Changes) != 2 || p.Changes[0].Op != "remove" || p.Changes[0].ID != corr.ID || p.Changes[1].Op != "upsert" || p.Changes[1].ID != stmt.ID {
		t.Fatalf("statement %+v", p)
	}
	cur = mirror(t, ts.URL, copy, cur, 10)

	// Unpaired by hand: both sides change.
	if code := do(t, http.DefaultClient, "PUT", ts.URL+"/api/transfers", store.TransferLink{OutID: out.ID, InID: in.ID, Kind: "refuse"}, nil); code != 200 {
		t.Fatalf("refuse %d", code)
	}
	p, _ = changes(t, ts.URL, cur, 10)
	if len(p.Changes) != 2 || p.Changes[0].Activity.TransferWith != "" {
		t.Fatalf("unpaired %+v", p)
	}
	cur = mirror(t, ts.URL, copy, cur, 10)

	// A source removed: its activities go.
	if code := do(t, http.DefaultClient, "DELETE", ts.URL+"/api/sources/s2", map[string]any{}, nil); code != 200 {
		t.Fatalf("delete %d", code)
	}
	p, _ = changes(t, ts.URL, cur, 10)
	if len(p.Changes) != 1 || p.Changes[0].Op != "remove" || p.Changes[0].ID != in.ID || p.Changes[0].Activity != nil {
		t.Fatalf("source removed %+v", p)
	}
	cur = mirror(t, ts.URL, copy, cur, 10)
	if len(copy) != 3 {
		t.Fatalf("copy %v", copy)
	}

	// The copy is what a fresh one would be.
	fresh := map[string]v1Activity{}
	mirror(t, ts.URL, fresh, "", 1)
	a, _ := json.Marshal(copy)
	b, _ := json.Marshal(fresh)
	if !bytes.Equal(a, b) {
		t.Fatalf("copy differs from a fresh one\n%s\n%s", a, b)
	}

	// Bad and stale cursors.
	if _, code := changes(t, ts.URL, "nope", 10); code != 400 {
		t.Fatalf("bad cursor %d", code)
	}
	epoch, _, _ := strings.Cut(cur, ".")
	if _, code := changes(t, ts.URL, "zz"+epoch+".1", 10); code != 410 {
		t.Fatalf("other epoch %d", code)
	}
	// Tombstones forgotten after a while: a cursor from before them is gone.
	st.Clock = func() time.Time { return time.Now().Add(store.TombstoneKeep + 24*time.Hour) }
	srv.changed()
	if _, code := changes(t, ts.URL, epoch+".1", 10); code != 410 {
		t.Fatalf("too old %d", code)
	}
	if p, code := changes(t, ts.URL, cur, 10); code != 200 || len(p.Changes) != 0 {
		t.Fatalf("recent cursor %d %+v", code, p)
	}
}

func TestChangesNotify(t *testing.T) {
	ts, srv, eng := feedServer(t)
	srv.feed.retry = []time.Duration{10 * time.Millisecond}
	ctx := context.Background()
	var mu sync.Mutex
	var got []string
	fails := 1
	var secretKey string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		defer mu.Unlock()
		ts, _, _ := strings.Cut(strings.TrimPrefix(r.Header.Get("Fundarr-Signature"), "t="), ",")
		at, _ := strconv.ParseInt(ts, 10, 64)
		if r.Header.Get("Fundarr-Signature") != sign([]byte(secretKey), time.Unix(at, 0), body) {
			w.WriteHeader(401)
			return
		}
		if fails > 0 {
			fails--
			w.WriteHeader(503)
			return
		}
		got = append(got, string(body))
	}))
	t.Cleanup(app.Close)

	var made struct {
		Info struct{ ID string }
	}
	do(t, http.DefaultClient, "POST", ts.URL+"/api/tokens", map[string]string{"name": "Cairn"}, &made)
	if code := do(t, http.DefaultClient, "PUT", ts.URL+"/api/tokens/"+made.Info.ID+"/notify", map[string]string{"url": "ftp://x"}, nil); code != 422 {
		t.Fatalf("bad url %d", code)
	}
	var sec struct{ Secret string }
	if code := do(t, http.DefaultClient, "PUT", ts.URL+"/api/tokens/"+made.Info.ID+"/notify", map[string]string{"url": app.URL + "/hook"}, &sec); code != 200 || !strings.HasPrefix(sec.Secret, "fns_") {
		t.Fatalf("notify %d %+v", code, sec)
	}
	mu.Lock()
	secretKey = sec.Secret
	mu.Unlock()
	var tokens []store.APIToken
	if do(t, http.DefaultClient, "GET", ts.URL+"/api/tokens", nil, &tokens); len(tokens) != 1 || tokens[0].NotifyURL != app.URL+"/hook" {
		t.Fatalf("tokens %+v", tokens)
	}

	if err := eng.Store.PutSource(ctx, &store.Entity{ID: "s1", Kind: "test", Name: "S1", Settings: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Store.AddActivities(ctx, []model.Activity{btc("", "s1", "a", time.Now(), model.Deposit, "1")}); err != nil {
		t.Fatal(err)
	}
	eng.OnChange()
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("not notified")
		}
		time.Sleep(20 * time.Millisecond)
	}
	var ev struct{ Event, Cursor string }
	json.Unmarshal([]byte(got[0]), &ev)
	p, _ := changes(t, ts.URL, "", 10)
	if ev.Event != "changes" || ev.Cursor != p.Cursor {
		t.Fatalf("event %+v, feed cursor %s", ev, p.Cursor)
	}
	// Stopped.
	if code := do(t, http.DefaultClient, "PUT", ts.URL+"/api/tokens/"+made.Info.ID+"/notify", map[string]string{"url": ""}, nil); code != 200 {
		t.Fatalf("stop %d", code)
	}
	if targets, _ := eng.Store.NotifyTargets(ctx); len(targets) != 0 {
		t.Fatalf("targets %+v", targets)
	}
}

// TestDemoChanges: the feed works on the made-up data, removals included.
func TestDemoChanges(t *testing.T) {
	st, err := demo.Open(context.Background(), time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	eng := engine.New(st, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))
	bg, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(context.Background(), Config{Engine: eng, Demo: true, Auth: AuthConfig{Mode: "none"}, Background: bg})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	copy := map[string]v1Activity{}
	cur := mirror(t, ts.URL, copy, "", 500)
	n := 0
	for _, a := range copy {
		if a.SourceID == demo.Ledger.ID {
			n++
		}
	}
	if len(copy) < 100 || n == 0 {
		t.Fatalf("demo copy %d, %d on the Ledger", len(copy), n)
	}
	if code := do(t, http.DefaultClient, "DELETE", ts.URL+"/api/sources/"+demo.Ledger.ID, map[string]any{}, nil); code != 200 {
		t.Fatalf("delete %d", code)
	}
	before := len(copy)
	mirror(t, ts.URL, copy, cur, 500)
	for _, a := range copy {
		if a.SourceID == demo.Ledger.ID || a.TransferWith != "" {
			t.Fatalf("left after removal: %+v", a)
		}
	}
	if len(copy) != before-n {
		t.Fatalf("copy %d, want %d", len(copy), before-n)
	}
}
