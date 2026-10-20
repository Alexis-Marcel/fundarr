package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/store"
)

// The changes feed lets an app keep its own copy of the history: the first
// call returns every activity, the next ones what was added, rewritten or
// removed since (docs/api.md). It is brought up to date from the history
// itself, so whatever rewrote it (a sync, a statement, a correction, a
// transfer paired by hand, a source removed) shows.

// feed is the server's side of the changes feed and of notifications.
type feed struct {
	mu    sync.Mutex // one refresh at a time
	fresh time.Time  // last refresh
	dirty atomic.Bool
	kick  chan struct{}

	// Notifications: what is being posted to each app, and what is next.
	nmu   sync.Mutex
	busy  map[string]bool
	next  map[string]string
	head  int64 // last head the apps were told about
	retry []time.Duration
	http  *http.Client
}

// feedMaxAge: a feed nobody marked changed is still rebuilt this often,
// in case something rewrote the history without saying so.
const feedMaxAge = 30 * time.Second

func newFeed() *feed {
	f := &feed{kick: make(chan struct{}, 1), busy: map[string]bool{}, next: map[string]string{},
		retry: []time.Duration{10 * time.Second, time.Minute, 5 * time.Minute}, http: &http.Client{Timeout: 15 * time.Second}}
	f.dirty.Store(true)
	return f
}

// changed marks the feed out of date and wakes the notifier. Never blocks.
func (s *Server) changed() {
	s.feed.dirty.Store(true)
	select {
	case s.feed.kick <- struct{}{}:
	default:
	}
}

// refreshFeed brings the feed in line with the history, unless it already is.
func (s *Server) refreshFeed(ctx context.Context) error {
	f := s.feed
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.dirty.Load() && time.Since(f.fresh) < feedMaxAge {
		return nil
	}
	f.dirty.Store(false)
	err := s.buildFeed(ctx)
	if err != nil {
		f.dirty.Store(true)
		return err
	}
	f.fresh = time.Now()
	return nil
}

func (s *Server) buildFeed(ctx context.Context) error {
	l, err := s.eng.Ledger(ctx)
	if err != nil {
		return err
	}
	// Costs are left out: they move with prices and the currency setting,
	// not with the history.
	views := s.views(ctx, l, "")
	items := make([]store.FeedItem, 0, len(views))
	for _, v := range views {
		data, err := json.Marshal(v)
		if err != nil {
			return err
		}
		items = append(items, store.FeedItem{ID: v.ID, Data: data})
	}
	_, err = s.st.ApplyFeed(ctx, items)
	return err
}

func feedCursor(st store.FeedState, seq int64) string {
	return st.Epoch + "." + strconv.FormatInt(seq, 10)
}

// GET /api/v1/changes?after=&limit=
func (s *Server) v1Changes(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.refreshFeed(ctx); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	st, err := s.st.FeedState(ctx)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	q := r.URL.Query()
	limit := min(max(atoi(q.Get("limit"), 1000), 1), 5000)
	var after int64
	if c := q.Get("after"); c != "" {
		epoch, seq, ok := strings.Cut(c, ".")
		n, err := strconv.ParseInt(seq, 10, 64)
		if !ok || err != nil || n < 0 {
			writeErr(w, 400, "after: the cursor of a previous answer")
			return
		}
		if epoch != st.Epoch || n > st.Head || n != 0 && n < st.Floor {
			writeErr(w, http.StatusGone, "cursor too old: drop your copy and start again without after")
			return
		}
		after = n
	}
	rows, more, err := s.st.Changes(ctx, after, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	type change struct {
		Op       string          `json:"op"` // "upsert" or "remove"
		ID       string          `json:"id"`
		Activity json.RawMessage `json:"activity,omitempty"`
	}
	out := make([]change, 0, len(rows))
	last := after
	for _, c := range rows {
		if len(c.Data) == 0 {
			out = append(out, change{Op: "remove", ID: c.ID})
		} else {
			out = append(out, change{Op: "upsert", ID: c.ID, Activity: c.Data})
		}
		last = c.Seq
	}
	writeJSON(w, map[string]any{"changes": out, "cursor": feedCursor(st, last), "more": more})
}

// ---- notifications

// notifyLoop refreshes the feed after changes and tells the apps that
// asked, until ctx is done. Bursts of changes are told once.
func (s *Server) notifyLoop(ctx context.Context) {
	f := s.feed
	if st, err := s.st.FeedState(ctx); err == nil {
		f.head = st.Head
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-f.kick:
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(time.Second):
		}
		if err := s.refreshFeed(ctx); err != nil {
			s.cfg.Log.Warn("changes feed", "err", err)
			continue
		}
		st, err := s.st.FeedState(ctx)
		if err != nil || st.Head <= f.head {
			continue
		}
		f.head = st.Head
		targets, err := s.st.NotifyTargets(ctx)
		if err != nil {
			continue
		}
		for _, t := range targets {
			s.notify(ctx, t, feedCursor(st, st.Head))
		}
	}
}

// notify posts to one app in the background; while a post is under way,
// only the latest cursor waits for its turn.
func (s *Server) notify(ctx context.Context, t store.NotifyTarget, cursor string) {
	f := s.feed
	f.nmu.Lock()
	defer f.nmu.Unlock()
	if f.busy[t.ID] {
		f.next[t.ID] = cursor
		return
	}
	f.busy[t.ID] = true
	go func() {
		for {
			if err := s.post(ctx, t, cursor); err != nil {
				s.cfg.Log.Warn("notify", "app", t.ID, "err", err)
			}
			f.nmu.Lock()
			c, ok := f.next[t.ID]
			delete(f.next, t.ID)
			if !ok {
				delete(f.busy, t.ID)
				f.nmu.Unlock()
				return
			}
			f.nmu.Unlock()
			cursor = c
		}
	}()
}

// sign returns the Fundarr-Signature header of body sent at t.
func sign(secret []byte, t time.Time, body []byte) string {
	ts := strconv.FormatInt(t.Unix(), 10)
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(ts + "."))
	m.Write(body)
	return "t=" + ts + ",v1=" + hex.EncodeToString(m.Sum(nil))
}

// post tells one app, retrying a few times.
func (s *Server) post(ctx context.Context, t store.NotifyTarget, cursor string) error {
	if s.eng.Box == nil {
		return errors.New("no key to open the secret")
	}
	secret, err := s.eng.Box.Open(t.Secret)
	if err != nil {
		return err
	}
	body, _ := json.Marshal(map[string]string{"event": "changes", "cursor": cursor})
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.URL, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", "Fundarr")
		req.Header.Set("Fundarr-Signature", sign(secret, time.Now(), body))
		resp, err := s.feed.http.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode/100 == 2 {
				return nil
			}
			err = fmt.Errorf("HTTP %d", resp.StatusCode)
		}
		if attempt >= len(s.feed.retry) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(s.feed.retry[attempt]):
		}
	}
}

// PUT /api/tokens/{id}/notify {url}: returns the secret that signs the
// calls, shown only now; an empty url stops them.
func (s *Server) putNotify(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	raw := strings.TrimSpace(in.URL)
	var sealed []byte
	secret := ""
	if raw != "" {
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || len(raw) > 500 {
			writeErr(w, 422, "an http:// or https:// address")
			return
		}
		if s.eng.Box == nil {
			writeErr(w, 500, "no key to seal the secret")
			return
		}
		b := make([]byte, 24)
		rand.Read(b)
		secret = "fns_" + hex.EncodeToString(b)
		sealed = s.eng.Box.Seal([]byte(secret))
	}
	err := s.st.SetNotify(r.Context(), r.PathValue("id"), raw, sealed)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "no such app")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]string{"secret": secret})
}
