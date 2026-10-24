// Package traderepublic reads a Trade Republic account (compte-titres,
// PEA, crypto) in one of two ways:
//
//   - CSV (always): the transaction export the app makes (Profile →
//     Statements → Transaction export), imported by hand; see statement.go.
//     Balances are then what the history adds up to.
//   - Connect (beta, opt-in): Fundarr signs in to Trade Republic's web API
//     as its web app does (client.go), the user approving each sign-in in
//     the Trade Republic app. A session lasts 24 hours: syncs in that time
//     download the same export and read the balances over the websocket
//     (ws.go); after it, the source waits for the next approval. This API
//     is not public, and Trade Republic's terms likely do not allow it.
package traderepublic

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

const (
	modeCSV     = "csv"
	modeConnect = "connect"
)

func init() {
	source.Register(source.Definition{
		Kind:        "traderepublic",
		Name:        "Trade Republic",
		Category:    "broker",
		Description: "Stocks, ETFs, crypto, PEA.",
		Modes: []source.Mode{
			{ID: modeCSV, Name: "CSV export", Help: "Import the file from the app."},
			{ID: modeConnect, Name: "Connect", Help: "Sync with one tap, approved in the app. Unofficial: Trade Republic does not offer it.", Beta: true},
		},
		Fields: []source.Field{
			{Name: source.ModeField, Label: "Mode", Optional: true, Hidden: true},
			{Name: "phone", Label: "Phone number", Optional: true, Mode: modeConnect, Help: "As in the app: +33 6 12 34 56 78. Leave empty to sign in with a QR code."},
			{Name: "pin", Label: "PIN", Secret: true, Optional: true, Mode: modeConnect},
			{Name: "app_version", Label: "Web app version", Optional: true, Advanced: true, Mode: modeConnect, Help: "leave empty: read from the web app"},
			{Name: "base_url", Label: "API URL", Optional: true, Advanced: true, Mode: modeConnect, Help: "leave empty"},
		},
		Statement: statementHelp,
		Limits:    "Trade Republic's export dates bookings, not executions, and has no transaction hash for crypto.",
		NoTest:    true,
		SealState: true,
		New:       New,
	})
}

// New builds the connector from its settings.
func New(cfg source.Config) (source.Source, error) {
	s := &Source{mode: cfg.Get(source.ModeField), phone: normPhone(cfg.Get("phone")), pin: strings.TrimSpace(cfg.Get("pin"))}
	if s.mode != modeConnect {
		s.mode = modeCSV
	}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &s.st); err != nil {
			return nil, fmt.Errorf("traderepublic: bad saved state: %w", err)
		}
	}
	if s.st.Device == "" {
		s.st.Device = newDevice()
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.Get("base_url")), "/")
	custom := base != ""
	if !custom {
		base = defaultBase
	}
	s.c = newClient(base, cfg.HTTP, s.st.Device, strings.TrimSpace(cfg.Get("app_version")))
	if custom {
		s.c.webApp = base + "/" // a stand-in serves both
	}
	if cfg.Log != nil {
		s.log = cfg.Log
	} else {
		s.log = func(string, ...any) {}
	}
	s.c.logf = func(f string, a ...any) { s.log(f, a...) }
	s.c.setCookies(s.st.Cookies)
	return s, nil
}

func normPhone(p string) string {
	r := strings.NewReplacer(" ", "", ".", "", "-", "", "(", "", ")", "")
	p = r.Replace(strings.TrimSpace(p))
	if strings.HasPrefix(p, "00") {
		p = "+" + p[2:]
	}
	return p
}

// Source is a configured Trade Republic connector.
type Source struct {
	mode, phone, pin string
	c                *client
	st               state
	log              func(string, ...any)
	login            *process // a sign-in waiting for the user
	// hist: each account's cash in the export this run downloaded.
	hist map[string]map[string]decimal.Decimal
}

// state is kept between runs, sealed by the engine (it holds the session).
type state struct {
	Device   string    `json:"device"`
	Cookies  []kv      `json:"cookies,omitempty"`
	SignedIn time.Time `json:"signedIn,omitzero"`
	Until    time.Time `json:"until,omitzero"`
	// Refused: hash of the phone number and PIN Trade Republic refused.
	// They are not tried again until changed.
	Refused string `json:"refused,omitempty"`
	// Wait: Trade Republic asked to wait until then before a new sign-in.
	Wait time.Time `json:"wait,omitzero"`
}

func (s *Source) State() []byte {
	b, _ := json.Marshal(s.st)
	return b
}

// Offline implements source.Offline: in CSV mode nothing is read from
// Trade Republic.
func (s *Source) Offline() bool { return s.mode != modeConnect }

// Test implements source.Tester: checking would mean a sign-in to approve,
// so the form has no test (NoTest) and saving checks nothing.
func (s *Source) Test(ctx context.Context) error { return nil }

// Activities implements source.Source: the history comes from the export
// (FetchStatement, Import), never from here.
func (s *Source) Activities(ctx context.Context, f source.Fetch) error { return nil }

// ---- session

func (s *Source) Session() source.Session {
	if s.mode != modeConnect {
		return source.Session{}
	}
	ses := source.Session{Needed: true, Methods: []string{"qr"}}
	if s.phone != "" && s.pin != "" {
		ses.Methods = []string{"app", "qr"}
	}
	if hasSession(s.st.Cookies) {
		ses.Until = s.st.Until
	}
	return ses
}

func (s *Source) creds() string {
	h := sha256.Sum256([]byte(s.phone + "\x00" + s.pin))
	return hex.EncodeToString(h[:8])
}

// Login implements source.Interactive.
func (s *Source) Login(ctx context.Context, method string) (*source.Challenge, error) {
	if s.mode != modeConnect {
		return nil, errors.New("Connect is off for this source")
	}
	if now := s.c.now(); s.st.Wait.After(now) {
		return nil, fmt.Errorf("Trade Republic asked to wait: try again after %s UTC", s.st.Wait.Format("15:04"))
	}
	var p *process
	var err error
	switch method {
	case "qr":
		p, err = s.c.startQR(ctx)
	case "app", "":
		if s.phone == "" || s.pin == "" {
			return nil, errors.New("add the phone number and PIN in Settings, or sign in with a QR code")
		}
		if s.st.Refused == s.creds() {
			return nil, errors.New("Trade Republic refused this phone number and PIN: check them in Settings")
		}
		p, err = s.c.startApp(ctx, s.phone, s.pin)
	default:
		return nil, fmt.Errorf("unknown sign-in method %q", method)
	}
	if err != nil {
		var e *apiError
		if errors.As(err, &e) {
			switch {
			case e.Status == 429 || e.Code == "TOO_MANY_REQUESTS":
				wait := e.Retry
				if wait <= 0 {
					wait = time.Hour
				}
				s.st.Wait = s.c.now().Add(wait)
			case method != "qr" && e.Status >= 400 && e.Status < 500 && e.Status != 426 &&
				e.Code != "MISSING_REQUIRED_HEADER" && e.Code != "BLOCKED_BY_FIREWALL" && e.Code != "CLIENT_VERSION_OUTDATED":
				// Wrong number or PIN: never again with the same.
				s.st.Refused = s.creds()
				return nil, fmt.Errorf("%w (phone number or PIN refused: check them in Settings; nothing is retried)", err)
			}
		}
		return nil, err
	}
	s.st.Refused = ""
	s.login = p
	return s.challenge(), nil
}

func (s *Source) challenge() *source.Challenge {
	p := s.login
	switch {
	case p == nil:
		return nil
	case p.Code:
		return &source.Challenge{Kind: "code", Digits: 6, Expires: p.Expires}
	case p.Method == "qr" && p.ID == "":
		return &source.Challenge{Kind: "qr", Data: p.QR, Expires: p.Expires}
	}
	return &source.Challenge{Kind: "approve", Expires: p.Expires}
}

// Poll implements source.Interactive.
func (s *Source) Poll(ctx context.Context) (*source.Challenge, error) {
	if s.login == nil {
		return nil, errors.New("no sign-in in progress")
	}
	if s.c.now().After(s.login.Expires) {
		s.login = nil
		return nil, errors.New("the sign-in was not approved in time")
	}
	ok, err := s.c.poll(ctx, s.login)
	if err != nil {
		var e *apiError
		if errors.As(err, &e) && (e.Status >= 500 || e.Status == 0) {
			return s.challenge(), nil // a hiccup: the next poll tells
		}
		s.login = nil
		return nil, err
	}
	if !ok {
		return s.challenge(), nil
	}
	return s.finish(ctx)
}

// Answer implements source.Interactive.
func (s *Source) Answer(ctx context.Context, code string) (*source.Challenge, error) {
	if s.login == nil || !s.login.Code {
		return nil, errors.New("no sign-in waits for a code")
	}
	code = strings.TrimSpace(code)
	if len(code) < 4 || len(code) > 10 {
		return nil, errors.New("type the code from your authenticator app")
	}
	if err := s.c.answer(ctx, s.login, code); err != nil {
		var e *apiError
		if errors.As(err, &e) && e.Code == "VALIDATION_CODE_INVALID" {
			s.login.Code = true
		}
		return s.challenge(), err
	}
	return s.Poll(ctx)
}

// finish keeps the session once approved.
func (s *Source) finish(ctx context.Context) (*source.Challenge, error) {
	s.login = nil
	// The web app turns the approved sign-in into a session first.
	if err := s.c.refresh(ctx); err != nil {
		return nil, fmt.Errorf("signed in, but the session was not opened: %w", err)
	}
	if _, err := s.c.account(ctx); err != nil {
		return nil, fmt.Errorf("signed in, but the session does not work: %w", err)
	}
	now := s.c.now()
	s.st.Cookies, s.st.SignedIn, s.st.Wait = s.c.cookies(), now, time.Time{}
	s.st.Until = sessionEnd(s.st.Cookies, now)
	if !hasSession(s.st.Cookies) {
		return nil, fmt.Errorf("signed in, but Trade Republic gave no session cookie (got: %s)", cookieNames(s.st.Cookies))
	}
	s.log("session cookies: %s", cookieNames(s.st.Cookies))
	s.log("signed in, session until %s UTC", s.st.Until.Format("2006-01-02 15:04"))
	return nil, nil
}

// session makes sure the session works before a sync, and keeps it fresh.
func (s *Source) session(ctx context.Context) error {
	if s.mode != modeConnect {
		return errors.New("Connect is off for this source")
	}
	if !hasSession(s.st.Cookies) || !s.st.Until.After(s.c.now()) {
		s.forget()
		return source.ErrLogin
	}
	if err := s.c.refresh(ctx); err != nil {
		if errors.Is(err, source.ErrLogin) {
			s.forget()
		}
		return err
	}
	s.st.Cookies = s.c.cookies()
	if u := sessionEnd(s.st.Cookies, s.st.SignedIn); !u.IsZero() {
		s.st.Until = u
	}
	return nil
}

func (s *Source) forget() {
	s.st.Cookies, s.st.Until = nil, time.Time{}
	s.c.setCookies(nil)
}

// FetchStatement implements source.StatementFetcher: the whole export,
// every sync, so the engine sees each booking as Trade Republic does now
// (a dividend booked again replaces the first one).
func (s *Source) FetchStatement(ctx context.Context, f source.Fetch) (string, []byte, error) {
	if f.Progress != nil {
		s.log = f.Progress
	}
	if err := s.session(ctx); err != nil {
		return "", nil, err
	}
	data, err := s.c.export(ctx, exportFrom, s.c.now().Format(time.DateOnly))
	if errors.Is(err, source.ErrLogin) {
		s.forget()
	}
	if err != nil {
		return "", nil, err
	}
	s.st.Cookies = s.c.cookies()
	s.hist = exportCash(data)
	return "Trade Republic (synced)", data, nil
}

// Balances implements source.Source: over the websocket with the session;
// from the history (source.ErrNoBalances) in CSV mode or when it fails.
func (s *Source) Balances(ctx context.Context) ([]model.Balance, error) {
	if s.mode != modeConnect {
		return nil, source.ErrNoBalances
	}
	if !hasSession(s.st.Cookies) {
		return nil, source.ErrLogin
	}
	b, err := s.c.balances(ctx, s.hist)
	if errors.Is(err, source.ErrLogin) {
		s.forget()
		return nil, err
	}
	if err != nil {
		return nil, fmt.Errorf("%w: %v", source.ErrNoBalances, err)
	}
	return b, nil
}
