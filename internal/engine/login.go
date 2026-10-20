package engine

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/internal/store"
)

// Interactive sign-ins (source.Interactive): the user approves on their
// phone, scans a QR code or types a code. The engine keeps the connector
// while the sign-in waits, checks on it in the background (so it goes on
// when the page is closed), keeps the session it ends with in the source's
// state, then syncs. A refused sign-in is never started again by itself.

// LoginPoll is how often a waiting sign-in is checked.
var LoginPoll = 2 * time.Second

// Login is where a sign-in stands, for the UI.
type Login struct {
	Method    string            `json:"method"`
	Challenge *source.Challenge `json:"challenge,omitempty"`
	Started   time.Time         `json:"started"`
	Done      bool              `json:"done,omitempty"`
	Error     string            `json:"error,omitempty"`
}

type login struct {
	mu     sync.Mutex // state
	call   sync.Mutex // one call to the connector at a time
	state  Login
	src    source.Source
	ia     source.Interactive
	kind   string
	cancel context.CancelFunc
	log    *runLog
	runID  int64
}

func (l *login) snapshot() *Login {
	l.mu.Lock()
	defer l.mu.Unlock()
	s := l.state
	if s.Challenge != nil {
		c := *s.Challenge
		s.Challenge = &c
	}
	return &s
}

func (l *login) waiting() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return !l.state.Done && l.state.Error == ""
}

var ErrNotInteractive = errors.New("this source does not sign in")

// Session is what a source says of its sign-in (nil when it has none).
func (e *Engine) Session(ent *store.Entity) *source.Session {
	src, err := e.Build(ent)
	if err != nil {
		return nil
	}
	ia, ok := src.(source.Interactive)
	if !ok {
		return nil
	}
	s := ia.Session()
	if !s.Needed {
		return nil
	}
	return &s
}

// LoginNeeded reports whether syncing the source needs a sign-in first.
func (e *Engine) LoginNeeded(ent *store.Entity) bool {
	s := e.Session(ent)
	return s != nil && (!s.Valid(e.Now()) || NeedsLogin(ent))
}

func (e *Engine) loginOf(id string) *login {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.logins[id]
}

func (e *Engine) loginPending(id string) bool {
	l := e.loginOf(id)
	return l != nil && l.waiting()
}

// LoginState returns the source's sign-in, if one was started.
func (e *Engine) LoginState(id string) *Login {
	if l := e.loginOf(id); l != nil {
		return l.snapshot()
	}
	return nil
}

// StartLogin starts a sign-in (method "" takes the source's first). One
// already waiting is returned as is: a second notification is never sent
// while the first can still be approved.
func (e *Engine) StartLogin(ctx context.Context, id, method string) (*Login, error) {
	if l := e.loginOf(id); l != nil && l.waiting() && (method == "" || method == l.state.Method) {
		return l.snapshot(), nil
	}
	e.CancelLogin(id)
	ent, err := e.Store.Source(ctx, id)
	if err != nil {
		return nil, err
	}
	log := &runLog{start: time.Now()}
	logf := func(f string, a ...any) {
		log.add(f, a...)
		e.Log.Info("sign-in", "source", ent.Name, "step", fmt.Sprintf(f, a...))
	}
	src, err := e.build(ent, logf)
	if err != nil {
		return nil, err
	}
	ia, ok := src.(source.Interactive)
	if !ok || !ia.Session().Needed {
		return nil, ErrNotInteractive
	}
	if method == "" {
		method = ia.Session().Methods[0]
	}
	l := &login{src: src, ia: ia, kind: ent.Kind, log: log, state: Login{Method: method, Started: e.Now()}}
	l.runID, _ = e.Store.StartRun(ctx, "login", id)
	e.mu.Lock()
	if e.logins == nil {
		e.logins = map[string]*login{}
	}
	e.logins[id] = l
	e.mu.Unlock()

	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	c, err := ia.Login(sctx, method)
	cancel()
	// What the attempt taught (a refusal, a wait) is kept either way.
	_ = e.Store.PutSourceState(ctx, id, e.sealState(ent.Kind, src.State()))
	if err != nil {
		e.endLogin(ctx, id, l, err)
		return l.snapshot(), err
	}
	l.mu.Lock()
	l.state.Challenge = c
	l.mu.Unlock()
	if c == nil { // signed in at once
		e.loggedIn(ctx, id, l)
		return l.snapshot(), nil
	}
	bg, stop := context.WithDeadline(context.WithoutCancel(ctx), c.Expires.Add(30*time.Second))
	l.cancel = stop
	go e.watchLogin(bg, id, l)
	return l.snapshot(), nil
}

// watchLogin checks on a waiting sign-in until it ends.
func (e *Engine) watchLogin(ctx context.Context, id string, l *login) {
	defer l.cancel()
	t := time.NewTicker(LoginPoll)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			if l.waiting() {
				e.endLogin(context.WithoutCancel(ctx), id, l, errors.New("the sign-in was not approved in time"))
			}
			return
		case <-t.C:
		}
		if !l.waiting() {
			return
		}
		l.call.Lock()
		if !l.waiting() { // answered meanwhile
			l.call.Unlock()
			return
		}
		pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		c, err := l.ia.Poll(pctx)
		cancel()
		l.call.Unlock()
		if err == nil && c != nil {
			l.mu.Lock()
			l.state.Challenge = c
			l.mu.Unlock()
		}
		switch {
		case err != nil:
			e.endLogin(context.WithoutCancel(ctx), id, l, err)
			return
		case c == nil:
			e.loggedIn(context.WithoutCancel(ctx), id, l)
			return
		}
	}
}

// AnswerLogin sends the code the sign-in asks for.
func (e *Engine) AnswerLogin(ctx context.Context, id, code string) (*Login, error) {
	l := e.loginOf(id)
	if l == nil || !l.waiting() {
		return nil, errors.New("no sign-in waits for a code")
	}
	l.call.Lock()
	defer l.call.Unlock()
	actx, cancel := context.WithTimeout(ctx, 20*time.Second)
	c, err := l.ia.Answer(actx, code)
	cancel()
	if c != nil {
		l.mu.Lock()
		l.state.Challenge = c
		l.mu.Unlock()
	}
	if err != nil {
		// A wrong code can be typed again while the sign-in waits.
		l.log.add("code: %v", err)
		return l.snapshot(), err
	}
	if c == nil {
		e.loggedIn(ctx, id, l)
	}
	return l.snapshot(), nil
}

// CancelLogin forgets a sign-in.
func (e *Engine) CancelLogin(id string) {
	e.mu.Lock()
	l := e.logins[id]
	delete(e.logins, id)
	e.mu.Unlock()
	if l != nil && l.cancel != nil {
		l.cancel()
	}
	if l != nil && l.waiting() {
		l.mu.Lock()
		l.state.Error = "cancelled"
		l.mu.Unlock()
		_ = e.Store.EndRun(context.Background(), l.runID, false, "sign-in cancelled", l.log.String())
	}
}

func (e *Engine) endLogin(ctx context.Context, id string, l *login, err error) {
	l.mu.Lock()
	l.state.Error, l.state.Challenge = err.Error(), nil
	l.mu.Unlock()
	l.log.add("sign-in failed: %v", err)
	_ = e.Store.PutSourceState(ctx, id, e.sealState(l.kind, l.src.State()))
	_ = e.Store.EndRun(ctx, l.runID, false, "sign-in: "+err.Error(), l.log.String())
	e.changed()
}

// loggedIn keeps the session, then syncs.
func (e *Engine) loggedIn(ctx context.Context, id string, l *login) {
	// A sync running with the old state must not write it over the new.
	for i := 0; !e.lock(id); i++ {
		if i > 600 {
			e.endLogin(ctx, id, l, errors.New("a sync never ended; try again"))
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	err := e.Store.PutSourceState(ctx, id, e.sealState(l.kind, l.src.State()))
	e.unlock(id)
	if err != nil {
		e.endLogin(ctx, id, l, err)
		return
	}
	l.mu.Lock()
	l.state.Done, l.state.Challenge = true, nil
	l.mu.Unlock()
	l.log.add("signed in")
	_ = e.Store.EndRun(ctx, l.runID, true, "signed in", l.log.String())
	go func() {
		if _, err := e.Sync(context.WithoutCancel(ctx), id); err != nil && !errors.Is(err, ErrBusy) {
			e.Log.Warn("sync after sign-in", "source", id, "err", err)
		}
		// Done: the UI follows the sync from here.
		e.mu.Lock()
		if e.logins[id] == l {
			delete(e.logins, id)
		}
		e.mu.Unlock()
	}()
}
