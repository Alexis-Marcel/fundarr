package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/internal/store"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

// A platform that needs a sign-in: approved after a few polls, or refused.
type gated struct {
	session []byte // state: "ok" once signed in
	polls   int
}

var (
	gateMu     sync.Mutex
	gateRefuse bool
	gateStarts int
	gateCode   bool
)

func init() {
	source.Register(source.Definition{Kind: "gated", Name: "Gated", SealState: true, New: func(cfg source.Config) (source.Source, error) {
		return &gated{session: cfg.State}, nil
	}})
}

func (g *gated) Session() source.Session {
	s := source.Session{Needed: true, Methods: []string{"app", "qr"}}
	if string(g.session) == "ok" {
		s.Until = time.Now().Add(time.Hour)
	}
	return s
}

func (g *gated) Login(ctx context.Context, method string) (*source.Challenge, error) {
	gateMu.Lock()
	defer gateMu.Unlock()
	gateStarts++
	if gateRefuse {
		g.session = []byte("refused")
		return nil, errors.New("PIN refused")
	}
	if gateCode {
		return &source.Challenge{Kind: "code", Digits: 6, Expires: time.Now().Add(time.Minute)}, nil
	}
	return &source.Challenge{Kind: method, Expires: time.Now().Add(time.Minute)}, nil
}

func (g *gated) Poll(ctx context.Context) (*source.Challenge, error) {
	if g.polls++; g.polls < 3 {
		return &source.Challenge{Kind: "approve", Expires: time.Now().Add(time.Minute)}, nil
	}
	g.session = []byte("ok")
	return nil, nil
}

func (g *gated) Answer(ctx context.Context, code string) (*source.Challenge, error) {
	if code != "123456" {
		return &source.Challenge{Kind: "code", Digits: 6, Expires: time.Now().Add(time.Minute)}, errors.New("wrong code")
	}
	g.session = []byte("ok")
	return nil, nil
}

func (g *gated) Activities(ctx context.Context, f source.Fetch) error {
	if string(g.session) != "ok" {
		return source.ErrLogin
	}
	return f.Emit(model.Activity{NativeID: "1", Time: time.Now().Add(-time.Hour), Type: model.Deposit, Base: model.Amt(model.CryptoOrFiat("EUR"), "5"), Account: "PEA"})
}

func (g *gated) Balances(ctx context.Context) ([]model.Balance, error) {
	return nil, source.ErrNoBalances
}
func (g *gated) State() []byte { return g.session }

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); !ok(); {
		if time.Now().After(end) {
			t.Fatalf("still waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestInteractiveLogin(t *testing.T) {
	e, ctx := setup(t)
	LoginPoll = 5 * time.Millisecond
	gateRefuse, gateStarts, gateCode = false, 0, false
	if err := e.Store.PutSource(ctx, &store.Entity{ID: "g", Kind: "gated", Name: "Gated", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Sync(ctx, "g"); !errors.Is(err, source.ErrLogin) {
		t.Fatalf("sync without a session: %v", err)
	}
	ent, _ := e.Store.Source(ctx, "g")
	if !NeedsLogin(ent) || !e.LoginNeeded(ent) {
		t.Fatalf("needs login: %q", ent.LastErr)
	}
	// The schedule leaves it alone.
	ent.LastRun = time.Time{}
	e.due(ctx)
	time.Sleep(20 * time.Millisecond)
	if gateStarts != 0 || e.Running("g") {
		t.Fatal("the schedule started a sign-in")
	}

	l, err := e.StartLogin(ctx, "g", "")
	if err != nil || l.Method != "app" || l.Challenge == nil {
		t.Fatalf("start %+v %v", l, err)
	}
	waitFor(t, "the sync after the sign-in", func() bool {
		ent, _ = e.Store.Source(ctx, "g")
		return !ent.LastOK.IsZero() && !e.Running("g")
	})
	if NeedsLogin(ent) || e.LoginNeeded(ent) || !strings.HasPrefix(string(ent.State), string(sealedState)) {
		t.Fatalf("after sign-in: %q, state %q", ent.LastErr, ent.State)
	}
	if n, _ := e.Store.CountActivities(ctx, "g"); n != 1 || !e.Store.BalancesDerived(ctx, "g") {
		t.Fatalf("%d activities", n)
	}
	// The balances come from the history, by account: nothing to correct.
	if gaps, _ := e.Reconcile(ctx, "g"); len(gaps) != 0 {
		t.Fatalf("gaps %+v", gaps)
	}
	if subs, _ := e.Store.SubAccounts(ctx, "g"); len(subs) != 1 || subs[0] != "PEA" {
		t.Fatalf("sub-accounts %v", subs)
	}
	waitFor(t, "the sign-in to be forgotten", func() bool { return e.LoginState("g") == nil })

	// A refusal is shown, never retried.
	if err := e.Store.PutSourceState(ctx, "g", nil); err != nil {
		t.Fatal(err)
	}
	gateRefuse = true
	if _, err := e.StartLogin(ctx, "g", "qr"); err == nil {
		t.Fatal("refused sign-in accepted")
	}
	if l := e.LoginState("g"); l == nil || !strings.Contains(l.Error, "PIN refused") {
		t.Fatalf("state %+v", l)
	}
	time.Sleep(30 * time.Millisecond)
	if gateStarts != 2 {
		t.Fatalf("%d sign-ins started", gateStarts)
	}

	// A code: a wrong one is asked again, the right one signs in.
	gateRefuse, gateCode = false, true
	if l, err := e.StartLogin(ctx, "g", ""); err != nil || l.Challenge.Kind != "code" {
		t.Fatalf("code %+v %v", l, err)
	}
	if _, err := e.AnswerLogin(ctx, "g", "000000"); err == nil {
		t.Fatal("wrong code accepted")
	}
	if l, err := e.AnswerLogin(ctx, "g", "123456"); err != nil || !l.Done {
		t.Fatalf("right code %+v %v", l, err)
	}
	waitFor(t, "the sync", func() bool { return e.LoginState("g") == nil && !e.Running("g") })
}

func TestSubAccountRoutes(t *testing.T) {
	e, ctx := setup(t)
	sealed, _ := e.Seal(map[string]string{"secret": "s3cret"})
	e.Store.PutSource(ctx, &store.Entity{ID: "s", Kind: "fake", Name: "Fake", Settings: map[string]string{"key": "k"}, Secrets: sealed, Enabled: true})
	e.Store.PutDestination(ctx, &store.Entity{ID: "d", Kind: "sink", Name: "Sink", Enabled: true})
	if err := e.Store.PutRoute(ctx, store.Route{DestinationID: "d", SourceID: "s", Account: "a1", Accounts: map[string]string{"PEA": "a2"}}); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	pea := dep("2", "1", t0)
	pea.Account = "PEA"
	fakeActs = []model.Activity{dep("1", "1", t0), pea}
	fakeBal = []model.Balance{{Amount: model.Amt(model.CryptoOrFiat("BTC"), "1")}, {Account: "PEA", Amount: model.Amt(model.CryptoOrFiat("BTC"), "1")}}
	if _, err := e.Sync(ctx, "s"); err != nil {
		t.Fatal(err)
	}
	to := map[string]string{}
	for _, b := range sink {
		for _, a := range b.Activities {
			to[a.NativeID] = b.Account
		}
	}
	if to["1"] != "a1" || to["2"] != "a2" {
		t.Fatalf("delivered %v", to)
	}
	routes, _ := e.Store.Routes(ctx)
	if len(routes) != 1 || routes[0].Accounts["PEA"] != "a2" || routes[0].To("") != "a1" {
		t.Fatalf("routes %+v", routes)
	}
}
