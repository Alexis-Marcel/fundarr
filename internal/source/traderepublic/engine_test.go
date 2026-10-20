package traderepublic_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"github.com/Alexis-Marcel/fundarr/internal/secret"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/internal/source/traderepublic"
	"github.com/Alexis-Marcel/fundarr/internal/store"
)

// The whole Connect flow through the engine, against the fake: a sync
// without a session waits for a sign-in, the sign-in is approved in the
// background, the session is stored sealed, the sync then imports the
// export and reads the balances.
func TestConnectThroughEngine(t *testing.T) {
	ctx := context.Background()
	f := traderepublic.NewFake(t)
	f.Export = traderepublic.FakeExport()
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, _ := secret.New(bytes.Repeat([]byte{7}, 32))
	eng := engine.New(st, box, slog.New(slog.NewTextHandler(io.Discard, nil)))
	engine.LoginPoll = 10 * time.Millisecond

	sealed, _ := json.Marshal(map[string]string{"pin": "1234"})
	ent := &store.Entity{ID: "tr1", Kind: "traderepublic", Name: "Trade Republic", Enabled: true, Secrets: box.Seal(sealed),
		Settings: map[string]string{"mode": "connect", "phone": "+33612345678", "base_url": f.URL}}
	if err := st.PutSource(ctx, ent); err != nil {
		t.Fatal(err)
	}
	// No session: the sync stops before asking anything.
	if _, err := eng.Sync(ctx, "tr1"); err != source.ErrLogin {
		t.Fatalf("sync without session: %v", err)
	}
	ent, _ = st.Source(ctx, "tr1")
	if !engine.NeedsLogin(ent) || !eng.LoginNeeded(ent) || len(f.Requests) != 0 {
		t.Fatalf("needs login %q, %d requests", ent.LastErr, len(f.Requests))
	}

	l, err := eng.StartLogin(ctx, "tr1", "")
	if err != nil {
		t.Fatal(err)
	}
	if l.Method != "app" || l.Challenge == nil || l.Challenge.Kind != "approve" {
		t.Fatalf("login %+v", l)
	}
	// Tapping again while it waits sends nothing new.
	if _, err := eng.StartLogin(ctx, "tr1", ""); err != nil || f.Logins != 1 {
		t.Fatalf("second start: %v, %d sign-ins", err, f.Logins)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		ent, _ = st.Source(ctx, "tr1")
		if !ent.LastOK.IsZero() && !eng.Running("tr1") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no sync after sign-in: %+v %+v", eng.LoginState("tr1"), ent.LastErr)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if ent.LastErr != "" || engine.NeedsLogin(ent) || eng.LoginNeeded(ent) {
		t.Fatalf("after sign-in: %q", ent.LastErr)
	}
	// The session is sealed at rest.
	if bytes.Contains(ent.State, []byte("tr_session")) || bytes.Contains(ent.State, []byte("ses-")) {
		t.Fatal("session stored in clear")
	}
	if s := eng.Session(ent); s == nil || !s.Valid(time.Now()) {
		t.Fatalf("session %+v", s)
	}
	n, _ := st.CountActivities(ctx, "tr1")
	gaps, _ := eng.Reconcile(ctx, "tr1")
	if n < 30 || len(gaps) != 0 || st.BalancesDerived(ctx, "tr1") {
		t.Fatalf("%d activities, gaps %+v, derived %v", n, gaps, st.BalancesDerived(ctx, "tr1"))
	}
	stmts, _ := st.Statements(ctx, "tr1")
	if len(stmts) != 1 || !strings.Contains(stmts[0].Name, "synced") {
		t.Fatalf("statements %+v", stmts)
	}

	// Syncing again replaces the synced statement, adds nothing.
	if _, err := eng.Sync(ctx, "tr1"); err != nil {
		t.Fatal(err)
	}
	if n2, _ := st.CountActivities(ctx, "tr1"); n2 != n {
		t.Fatalf("%d activities, then %d", n, n2)
	}
	if stmts, _ = st.Statements(ctx, "tr1"); len(stmts) != 1 {
		t.Fatalf("%d statements", len(stmts))
	}

	// The session ends on Trade Republic's side: one sync finds out, then
	// the source waits for the user.
	f.Revoke()
	if _, err := eng.Sync(ctx, "tr1"); err != source.ErrLogin {
		t.Fatalf("revoked: %v", err)
	}
	ent, _ = st.Source(ctx, "tr1")
	if !eng.LoginNeeded(ent) {
		t.Fatal("no sign-in asked after the session ended")
	}
	calls := len(f.Requests)
	if _, err := eng.Sync(ctx, "tr1"); err != source.ErrLogin || len(f.Requests) != calls {
		t.Fatalf("asked Trade Republic again: %v", err)
	}
	if f.Logins != 1 {
		t.Fatalf("%d sign-ins started by themselves", f.Logins)
	}
}

// CSV mode: the import is the history, the balances what it adds up to.
func TestCSVThroughEngine(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	box, _ := secret.New(bytes.Repeat([]byte{7}, 32))
	eng := engine.New(st, box, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := st.PutSource(ctx, &store.Entity{ID: "tr", Kind: "traderepublic", Name: "TR", Enabled: true, Settings: map[string]string{}}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Sync(ctx, "tr"); err != nil {
		t.Fatal(err)
	}
	res, err := eng.Import(ctx, "tr", "export.csv", []byte(traderepublic.FakeExport()))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Gaps) != 0 || !st.BalancesDerived(ctx, "tr") {
		t.Fatalf("gaps %+v", res.Gaps)
	}
	bal, _, _ := st.Balances(ctx, "tr")
	if len(bal) < 9 {
		t.Fatalf("balances %+v", bal)
	}
	// The same file again: same activities, nothing duplicated.
	n, _ := st.CountActivities(ctx, "tr")
	if _, err := eng.Import(ctx, "tr", "export.csv", []byte(traderepublic.FakeExport())); err != nil {
		t.Fatal(err)
	}
	if n2, _ := st.CountActivities(ctx, "tr"); n2 != n {
		t.Fatalf("%d then %d", n, n2)
	}
	ent, _ := st.Source(ctx, "tr")
	if eng.Session(ent) != nil || eng.LoginNeeded(ent) {
		t.Fatal("CSV mode signs in")
	}
}
