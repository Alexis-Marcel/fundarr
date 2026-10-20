package server

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

// A source that signs in with a QR code, approved on the third poll.
type qrSource struct {
	state []byte
	polls int
}

var qrStarts atomic.Int32

func init() {
	source.Register(source.Definition{Kind: "qrsrc", Name: "QR", NoTest: true, SealState: true,
		New: func(cfg source.Config) (source.Source, error) { return &qrSource{state: cfg.State}, nil }})
}

func (q *qrSource) Session() source.Session {
	s := source.Session{Needed: true, Methods: []string{"qr"}}
	if string(q.state) == "ok" {
		s.Until = time.Now().Add(24 * time.Hour)
	}
	return s
}
func (q *qrSource) Login(context.Context, string) (*source.Challenge, error) {
	qrStarts.Add(1)
	return &source.Challenge{Kind: "qr", Data: "https://example.org/qr/1", Expires: time.Now().Add(time.Minute)}, nil
}
func (q *qrSource) Poll(context.Context) (*source.Challenge, error) {
	if q.polls++; q.polls < 3 {
		return &source.Challenge{Kind: "qr", Data: "https://example.org/qr/2", Expires: time.Now().Add(time.Minute)}, nil
	}
	q.state = []byte("ok")
	return nil, nil
}
func (q *qrSource) Answer(context.Context, string) (*source.Challenge, error) { return nil, nil }
func (q *qrSource) Activities(ctx context.Context, f source.Fetch) error {
	return f.Emit(model.Activity{NativeID: "a", Time: time.Now().Add(-time.Hour), Type: model.Deposit, Base: model.Amt(model.CryptoOrFiat("EUR"), "1")})
}
func (q *qrSource) Balances(context.Context) ([]model.Balance, error) {
	return []model.Balance{{Amount: model.Amt(model.CryptoOrFiat("EUR"), "1")}}, nil
}
func (q *qrSource) State() []byte { return q.state }

func TestSyncStartsSignIn(t *testing.T) {
	engine.LoginPoll = 5 * time.Millisecond
	ts, eng, c := setup(t, "none")
	var created sourceView
	if code := do(t, c, "POST", ts.URL+"/api/sources", entityInput{Kind: "qrsrc", Name: "QR", SkipTest: true}, &created); code != 200 {
		t.Fatalf("create %d", code)
	}
	waitIdle(t, eng, created.ID)
	var list []struct {
		ID      string          `json:"id"`
		Session *source.Session `json:"session"`
		Login   *loginView      `json:"login"`
		Error   string          `json:"lastError"`
	}
	do(t, c, "GET", ts.URL+"/api/sources", nil, &list)
	if len(list) != 1 || list[0].Session == nil || !list[0].Session.Needed || !strings.Contains(list[0].Error, "sign-in") {
		t.Fatalf("listed %+v", list)
	}

	// One tap: the sync starts the sign-in.
	var started struct {
		Started bool       `json:"started"`
		Login   *loginView `json:"login"`
	}
	if code := do(t, c, "POST", ts.URL+"/api/sources/"+created.ID+"/sync", map[string]any{}, &started); code != 202 {
		t.Fatalf("sync %d", code)
	}
	if started.Started || started.Login == nil || started.Login.Challenge.Kind != "qr" || !strings.HasPrefix(started.Login.QR, "<svg") ||
		started.Login.Challenge.Data != "" {
		t.Fatalf("sign-in %+v", started.Login)
	}
	// Tapping again shows the same sign-in.
	do(t, c, "POST", ts.URL+"/api/sources/"+created.ID+"/sync", map[string]any{}, &started)
	if qrStarts.Load() != 1 {
		t.Fatalf("%d sign-ins", qrStarts.Load())
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		ent, _ := eng.Store.Source(context.Background(), created.ID)
		if !ent.LastOK.IsZero() && !eng.Running(created.ID) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("no sync after the sign-in")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var st struct {
		Login *loginView `json:"login"`
	}
	do(t, c, "GET", ts.URL+"/api/sources/"+created.ID+"/login", nil, &st)
	if st.Login != nil && !st.Login.Done {
		t.Fatalf("login %+v", st.Login)
	}
	// Signed in: a sync is a sync.
	if code := do(t, c, "POST", ts.URL+"/api/sources/"+created.ID+"/sync", map[string]any{}, &started); code != 202 || !started.Started {
		t.Fatalf("second sync %d %+v", code, started)
	}
	waitIdle(t, eng, created.ID)
	if code := do(t, c, "DELETE", ts.URL+"/api/sources/"+created.ID+"/login", map[string]any{}, nil); code != 200 {
		t.Fatalf("cancel %d", code)
	}
	if code := do(t, c, "POST", ts.URL+"/api/sources/"+created.ID+"/login/code", map[string]string{"code": "1"}, nil); code != 422 {
		t.Fatalf("code without sign-in %d", code)
	}
}
