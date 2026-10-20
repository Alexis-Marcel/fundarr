package traderepublic

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// fakeExport is the fixture without the junior account (the fake's
// websocket knows two accounts).
func fakeExport() string {
	var lines []line
	for _, l := range fixture {
		if l["account_type"] != "JUNIOR" {
			lines = append(lines, l)
		}
	}
	return render(lines, ",", false, false)
}

type logBook struct {
	mu    sync.Mutex
	lines []string
}

func (b *logBook) add(f string, a ...any) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.lines = append(b.lines, fmt.Sprintf(f, a...))
}

func (b *logBook) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return strings.Join(b.lines, "\n")
}

func connect(t *testing.T, f *Fake, vals map[string]string, state []byte, log *logBook) *Source {
	t.Helper()
	v := map[string]string{"mode": "connect", "phone": "+33 6 12 34 56 78", "pin": "1234", "base_url": f.URL}
	for k, x := range vals {
		v[k] = x
	}
	cfg := source.Config{Values: v, State: state}
	if log != nil {
		cfg.Log = log.add
	}
	src, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	s := src.(*Source)
	s.c.sleep = func(context.Context, time.Duration) error { return nil }
	return s
}

// approve polls until signed in.
func approve(t *testing.T, s *Source, c *source.Challenge) {
	t.Helper()
	ctx := context.Background()
	for i := 0; c != nil; i++ {
		if i > 10 {
			t.Fatalf("still %+v", c)
		}
		var err error
		if c, err = s.Poll(ctx); err != nil {
			t.Fatal(err)
		}
	}
}

func TestConnect(t *testing.T) {
	ctx := context.Background()
	f := NewFake(t)
	f.Export = fakeExport()
	log := &logBook{}
	s := connect(t, f, nil, nil, log)

	ses := s.Session()
	if !ses.Needed || !ses.Until.IsZero() || strings.Join(ses.Methods, ",") != "app,qr" || ses.Valid(time.Now()) {
		t.Fatalf("session before sign-in %+v", ses)
	}
	if s.Offline() {
		t.Fatal("connect mode reads Trade Republic")
	}
	// No session: nothing is asked.
	if _, _, err := s.FetchStatement(ctx, source.Fetch{}); !errors.Is(err, source.ErrLogin) {
		t.Fatalf("fetch without session: %v", err)
	}
	if n := len(f.Requests); n != 0 {
		t.Fatalf("%d requests without a session", n)
	}

	c, err := s.Login(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != "approve" || c.Expires.Before(time.Now().Add(time.Minute)) {
		t.Fatalf("challenge %+v", c)
	}
	approve(t, s, c)
	ses = s.Session()
	if !ses.Valid(time.Now()) || ses.Until.Before(time.Now().Add(23*time.Hour)) {
		t.Fatalf("session after sign-in %+v", ses)
	}

	// The state brings the session back in a new connector.
	state := s.State()
	s2 := connect(t, f, nil, state, log)
	if !s2.Session().Valid(time.Now()) {
		t.Fatal("session lost with the state")
	}
	name, data, err := s2.FetchStatement(ctx, source.Fetch{Progress: log.add})
	if err != nil {
		t.Fatal(err)
	}
	st, err := s2.Import(name, data, nil)
	if err != nil {
		t.Fatal(err)
	}
	bal, err := s2.Balances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// The export and the websocket agree: the history explains every balance.
	if gaps := model.Reconcile(st.Activities, bal, decimal.New(1, -6)); len(gaps) != 0 {
		t.Fatalf("gaps %+v", gaps)
	}
	pea := map[string]string{}
	for _, b := range bal {
		if b.Account == "PEA" {
			pea[b.Asset.Key()] = b.Qty.String()
		}
	}
	if pea["security:IE00B4L5Y983"] != "5" || pea["fiat:EUR"] != "70" || len(pea) != 2 {
		t.Fatalf("PEA %v", pea)
	}
	for _, b := range bal {
		if b.Asset.Symbol == "BTC" && (b.Asset.Kind != model.Crypto || b.Account != "") {
			t.Fatalf("bitcoin %+v", b)
		}
	}

	// Nothing secret reaches the log.
	text := log.String()
	for _, secret := range []string{"1234", "612345678", "ses-", "eyJ", "p-0000"} {
		if strings.Contains(text, secret) {
			t.Errorf("log shows %q:\n%s", secret, text)
		}
	}
	if !strings.Contains(text, "export: ready") || !strings.Contains(text, "positions (PEA): 1") {
		t.Errorf("log too thin:\n%s", text)
	}

	// Trade Republic ends the session: the next sync stops, asks nothing more.
	f.Revoke()
	s3 := connect(t, f, nil, s2.State(), nil)
	if _, _, err := s3.FetchStatement(ctx, source.Fetch{}); !errors.Is(err, source.ErrLogin) {
		t.Fatalf("revoked: %v", err)
	}
	if s3.Session().Valid(time.Now()) {
		t.Fatal("a revoked session still looks valid")
	}
	before := len(f.Requests)
	if _, _, err := s3.FetchStatement(ctx, source.Fetch{}); !errors.Is(err, source.ErrLogin) || len(f.Requests) != before {
		t.Fatalf("second try asked Trade Republic: %v", err)
	}
	if f.Logins != 1 {
		t.Fatalf("%d sign-ins started", f.Logins)
	}
}

func TestConnectTOTP(t *testing.T) {
	ctx := context.Background()
	f := NewFake(t)
	f.TOTP = true
	s := connect(t, f, nil, nil, nil)
	c, err := s.Login(ctx, "app")
	if err != nil || c.Kind != "code" || c.Digits != 6 {
		t.Fatalf("%+v %v", c, err)
	}
	// A wrong code: asked again, the sign-in goes on.
	if c, err = s.Answer(ctx, "000000"); err == nil || c == nil || c.Kind != "code" {
		t.Fatalf("wrong code: %+v %v", c, err)
	}
	if c, err = s.Answer(ctx, "123456"); err != nil {
		t.Fatal(err)
	}
	approve(t, s, c)
	if !s.Session().Valid(time.Now()) {
		t.Fatal("not signed in")
	}
}

func TestConnectQR(t *testing.T) {
	ctx := context.Background()
	f := NewFake(t)
	s := connect(t, f, map[string]string{"phone": "", "pin": ""}, nil, nil)
	if m := s.Session().Methods; strings.Join(m, ",") != "qr" {
		t.Fatalf("methods %v", m)
	}
	if _, err := s.Login(ctx, "app"); err == nil {
		t.Fatal("app sign-in without a PIN")
	}
	c, err := s.Login(ctx, "qr")
	if err != nil || c.Kind != "qr" || c.Data != "trqr://challenge/1" {
		t.Fatalf("%+v %v", c, err)
	}
	// The code changes while it waits.
	if c, err = s.Poll(ctx); err != nil || c.Kind != "qr" || c.Data != "trqr://challenge/2" {
		t.Fatalf("%+v %v", c, err)
	}
	// Scanned: then approved like any sign-in.
	if c, err = s.Poll(ctx); err != nil || c.Kind != "approve" {
		t.Fatalf("%+v %v", c, err)
	}
	approve(t, s, c)
	if !s.Session().Valid(time.Now()) || f.Logins != 0 || f.QRs != 1 {
		t.Fatalf("session %+v, %d logins %d QR", s.Session(), f.Logins, f.QRs)
	}
}

// Seen on a real account: approved in the app, the challenge says CLAIMED
// and gives no process; the session is already there.
func TestConnectQRClaimed(t *testing.T) {
	ctx := context.Background()
	f := NewFake(t)
	f.Claimed = true
	s := connect(t, f, map[string]string{"phone": "", "pin": ""}, nil, nil)
	if _, err := s.Login(ctx, "qr"); err != nil {
		t.Fatal(err)
	}
	c, err := s.Poll(ctx) // still waiting for the scan
	if err != nil || c == nil || c.Kind != "qr" {
		t.Fatalf("%+v %v", c, err)
	}
	if c, err = s.Poll(ctx); err != nil || c != nil {
		t.Fatalf("CLAIMED: %+v %v, want signed in", c, err)
	}
	if !s.Session().Valid(time.Now()) {
		t.Fatal("not signed in")
	}
}

func TestConnectRefusals(t *testing.T) {
	ctx := context.Background()
	f := NewFake(t)
	// A wrong PIN: refused once, never tried again with it.
	s := connect(t, f, map[string]string{"pin": "9999"}, nil, nil)
	if _, err := s.Login(ctx, "app"); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("wrong PIN: %v", err)
	}
	s = connect(t, f, map[string]string{"pin": "9999"}, s.State(), nil)
	if _, err := s.Login(ctx, "app"); err == nil {
		t.Fatal("wrong PIN accepted")
	}
	if f.Logins != 1 {
		t.Fatalf("the refused PIN was sent %d times", f.Logins)
	}
	// Another PIN can be tried: it is the user's new input.
	s = connect(t, f, nil, s.State(), nil)
	if _, err := s.Login(ctx, "app"); err != nil || f.Logins != 2 {
		t.Fatalf("new PIN: %v (%d)", err, f.Logins)
	}

	// Declined in the app.
	f.Decline = true
	if _, err := s.Poll(ctx); err == nil || !strings.Contains(err.Error(), "declined") {
		t.Fatalf("declined: %v", err)
	}
	f.Decline = false

	// Too many attempts: wait as asked, without asking again before.
	f.TooMany = true
	s = connect(t, f, nil, nil, nil)
	if _, err := s.Login(ctx, "app"); err == nil {
		t.Fatal("429 accepted")
	}
	n := f.Logins
	s = connect(t, f, nil, s.State(), nil)
	if _, err := s.Login(ctx, "qr"); err == nil || !strings.Contains(err.Error(), "wait") || f.Logins != n || f.QRs != 0 {
		t.Fatalf("after 429: %v", err)
	}
}

func TestConnectBalancesFallBack(t *testing.T) {
	ctx := context.Background()
	f := NewFake(t)
	s := connect(t, f, nil, nil, nil)
	c, err := s.Login(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	approve(t, s, c)
	f.NoSocket = true
	if _, err := s.Balances(ctx); !errors.Is(err, source.ErrNoBalances) {
		t.Fatalf("no socket: %v", err)
	}
	// In CSV mode the history is the only source.
	csv := connect(t, f, map[string]string{"mode": "csv"}, nil, nil)
	if _, err := csv.Balances(ctx); !errors.Is(err, source.ErrNoBalances) || !csv.Offline() || csv.Session().Needed {
		t.Fatalf("csv: %v", err)
	}
}

func TestDelta(t *testing.T) {
	prev := `{"a":1,"b":"x"}`
	// Keep 5 chars, drop 1, insert "2", keep the rest.
	if got := applyDelta(prev, "=5\t-1\t+2\t=9"); got != `{"a":2,"b":"x"}` {
		t.Fatalf("delta %s", got)
	}
	if got := applyDelta("é", "=1\t+%C3%A9"); got != "éé" {
		t.Fatalf("runes %s", got)
	}
}

func TestSessionEnd(t *testing.T) {
	exp := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	if got := sessionEnd([]kv{{"tr_refresh", jwt(exp, 1)}}, time.Time{}); !got.Equal(exp) {
		t.Fatalf("from the JWT: %s", got)
	}
	in := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	if got := sessionEnd([]kv{{"tr_refresh", "opaque"}}, in); !got.Equal(in.Add(24 * time.Hour)) {
		t.Fatalf("fallback: %s", got)
	}
	for id, want := range map[string]string{"XF000BTC0017": "BTC", "XF000ETH0019": "ETH", "XF000SOL0012": "SOL"} {
		if a := positionAsset(id, "", ""); a.Kind != model.Crypto || a.Symbol != want {
			t.Errorf("%s → %+v", id, a)
		}
	}
	if a := positionAsset("IE00B4L5Y983", "stocksAndETFs", ""); a.Kind != model.Security || a.ISIN != "IE00B4L5Y983" {
		t.Errorf("ETF %+v", a)
	}
}

// Without the export to tell, a cash answer naming accounts is read
// account by account, one naming none is compared over all of them.
func TestCashBalancesWithoutHistory(t *testing.T) {
	accounts := parsePairs([]byte(fakePairs))
	read := func(answer string) map[string]string {
		out := map[string]string{}
		for _, b := range cashBalances(accounts, parseCash([]byte(answer)), nil, func(string, ...any) {}) {
			v := b.Qty.String()
			if b.Unread {
				v = "unread"
			}
			out[b.Account+" "+b.Asset.Symbol] = v
		}
		return out
	}
	cases := map[string]map[string]string{
		CashEach:             {" EUR": "-38.47", "PEA EUR": "75.4"},
		CashMainOnly:         {" EUR": "-38.47", "PEA EUR": "unread"},
		CashPEAUnknownNumber: {" EUR": "-38.47", "PEA EUR": "75.4"},
		CashTotal:            {"* EUR": "36.93"},
		CashMainNoNumber:     {"* EUR": "-38.47"},
	}
	for answer, want := range cases {
		got := read(answer)
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Errorf("%s: %v, want %v", answer, got, want)
		}
	}
}
