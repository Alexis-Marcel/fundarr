// Package server is Fundarr's HTTP API and the web UI it serves.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/dest"
	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/internal/store"
)

type Config struct {
	Engine  *engine.Engine
	Auth    AuthConfig
	Static  fs.FS // the built UI; nil in tests
	Version string
	Log     *slog.Logger
	// Background is the context syncs started from the UI run in.
	Background context.Context
}

type Server struct {
	cfg  Config
	auth *auth
	eng  *engine.Engine
	st   *store.Store
	mux  *http.ServeMux
}

func New(ctx context.Context, cfg Config) (*Server, error) {
	a, err := newAuth(ctx, cfg.Auth)
	if err != nil {
		return nil, err
	}
	if cfg.Background == nil {
		cfg.Background = context.Background()
	}
	s := &Server{cfg: cfg, auth: a, eng: cfg.Engine, st: cfg.Engine.Store, mux: http.NewServeMux()}
	s.routes()
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "same-origin")
	w.Header().Set("X-Frame-Options", "DENY")
	s.mux.ServeHTTP(w, r)
}

func (s *Server) routes() {
	m := s.mux
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.st.Ping(r.Context()); err != nil {
			http.Error(w, "db", http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte("ok"))
	})
	m.HandleFunc("GET /api/session", s.session)
	m.HandleFunc("POST /api/login", s.auth.handleLogin)
	m.HandleFunc("POST /api/setup", s.auth.handleSetup)
	m.HandleFunc("POST /api/logout", s.auth.handleLogout)
	m.HandleFunc("GET /auth/login", s.auth.handleOIDCStart)
	m.HandleFunc("GET /auth/callback", s.auth.handleOIDCCallback)

	api := func(pattern string, h http.HandlerFunc) { m.HandleFunc(pattern, s.protect(h)) }
	api("POST /api/password", s.auth.handleChangePassword)
	api("GET /api/definitions", s.definitions)
	api("GET /api/sources", s.listSources)
	api("POST /api/sources", s.createSource)
	api("POST /api/sources/test", s.testSource)
	api("PUT /api/sources/{id}", s.updateSource)
	api("DELETE /api/sources/{id}", s.deleteSource)
	api("POST /api/sources/{id}/sync", s.syncSource)
	api("GET /api/sources/{id}/activities", s.sourceActivities)
	api("GET /api/sources/{id}/holdings", s.sourceHoldings)
	api("GET /api/destinations", s.listDestinations)
	api("POST /api/destinations", s.createDestination)
	api("PUT /api/destinations/{id}", s.updateDestination)
	api("DELETE /api/destinations/{id}", s.deleteDestination)
	api("GET /api/destinations/{id}/accounts", s.destinationAccounts)
	api("POST /api/destinations/{id}/push", s.pushDestination)
	api("PUT /api/routes", s.putRoute)
	api("DELETE /api/routes", s.deleteRoute)
	api("POST /api/routes/reset", s.resetRoute)
	api("GET /api/runs", s.runs)
	m.HandleFunc("/", s.static)
}

// protect requires a session and, for writes, a JSON body (a cross-site form
// cannot send one without a CORS preflight, which Fundarr never allows).
func (s *Server) protect(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.auth.user(r) == "" {
			writeErr(w, http.StatusUnauthorized, "sign in first")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeErr(w, http.StatusUnsupportedMediaType, "JSON body required")
				return
			}
			if o := r.Header.Get("Origin"); o != "" {
				if u, err := url.Parse(o); err != nil || u.Host != r.Host {
					writeErr(w, http.StatusForbidden, "cross-origin request refused")
					return
				}
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
		h(w, r)
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func newID() string {
	b := make([]byte, 6)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Server) session(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"user":    s.auth.user(r),
		"mode":    s.auth.cfg.Mode,
		"setup":   s.auth.needsSetup(r.Context()),
		"local":   fromPrivateNetwork(r),
		"fixed":   s.auth.cfg.PasswordHash != "",
		"auto":    s.auth.cfg.Mode == "oidc" && s.auth.cfg.OIDCAutoRedirect,
		"version": s.cfg.Version,
	})
}

func (s *Server) definitions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"sources": source.List(), "destinations": dest.List()})
}

// ---- sources

type sourceView struct {
	*store.Entity
	Category   string      `json:"category"`
	Interval   int         `json:"intervalMinutes"`
	Running    bool        `json:"running"`
	Activities int         `json:"activities"`
	BalancesAt time.Time   `json:"balancesAt"`
	Gaps       []model.Gap `json:"gaps"`
	SecretSet  []string    `json:"secretsSet"` // which secret fields have a value
}

func (s *Server) viewSource(ctx context.Context, e *store.Entity) sourceView {
	v := sourceView{Entity: e, Interval: int(e.Interval / time.Minute), Running: s.eng.Running(e.ID)}
	if d, err := source.Get(e.Kind); err == nil {
		v.Category = d.Category
	}
	v.Activities, _ = s.st.CountActivities(ctx, e.ID)
	_, v.BalancesAt, _ = s.st.Balances(ctx, e.ID)
	if !v.BalancesAt.IsZero() {
		v.Gaps, _ = s.eng.Reconcile(ctx, e.ID)
	}
	if sec, err := s.eng.Secrets(e); err == nil {
		for k, val := range sec {
			if val != "" {
				v.SecretSet = append(v.SecretSet, k)
			}
		}
	}
	return v
}

func (s *Server) listSources(w http.ResponseWriter, r *http.Request) {
	ents, err := s.st.Sources(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []sourceView{}
	for _, e := range ents {
		out = append(out, s.viewSource(r.Context(), e))
	}
	writeJSON(w, out)
}

type entityInput struct {
	Kind     string            `json:"kind"`
	Name     string            `json:"name"`
	Values   map[string]string `json:"values"`
	Enabled  *bool             `json:"enabled"`
	Interval int               `json:"intervalMinutes"`
	SkipTest bool              `json:"skipTest"`
}

// split separates a form's values into settings and secrets according to the
// definition's fields; an empty secret keeps the stored one.
func split(fields []source.Field, values map[string]string, old map[string]string) (map[string]string, map[string]string, error) {
	settings, secrets := map[string]string{}, map[string]string{}
	for _, f := range fields {
		v := strings.TrimSpace(values[f.Name])
		if f.Secret {
			if v == "" {
				v = old[f.Name]
			}
			if v != "" {
				secrets[f.Name] = v
			}
		} else if v != "" {
			settings[f.Name] = v
		}
		if v == "" && !f.Optional {
			return nil, nil, errors.New(f.Label + " is required")
		}
	}
	return settings, secrets, nil
}

func (s *Server) buildSource(ctx context.Context, in entityInput, existing *store.Entity) (*store.Entity, error) {
	kind := in.Kind
	if existing != nil {
		kind = existing.Kind
	}
	def, err := source.Get(kind)
	if err != nil {
		return nil, err
	}
	var old map[string]string
	if existing != nil {
		if old, err = s.eng.Secrets(existing); err != nil {
			return nil, err
		}
	}
	settings, secrets, err := split(def.Fields, in.Values, old)
	if err != nil {
		return nil, err
	}
	sealed, err := s.eng.Seal(secrets)
	if err != nil {
		return nil, err
	}
	e := &store.Entity{ID: newID(), Kind: kind, Name: strings.TrimSpace(in.Name), Settings: settings, Secrets: sealed, Enabled: true, Interval: 6 * time.Hour}
	if existing != nil {
		e.ID, e.Created, e.State, e.Enabled, e.Interval = existing.ID, existing.Created, existing.State, existing.Enabled, existing.Interval
	}
	if e.Name == "" {
		e.Name = def.Name
	}
	if in.Enabled != nil {
		e.Enabled = *in.Enabled
	}
	if in.Interval > 0 {
		e.Interval = time.Duration(max(in.Interval, 15)) * time.Minute
	}
	return e, nil
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}

func (s *Server) testSource(w http.ResponseWriter, r *http.Request) {
	var in struct {
		entityInput
		ID string `json:"id"`
	}
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var existing *store.Entity
	if in.ID != "" {
		existing, _ = s.st.Source(r.Context(), in.ID)
	}
	e, err := s.buildSource(r.Context(), in.entityInput, existing)
	if err == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		err = s.eng.Test(ctx, e)
	}
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) createSource(w http.ResponseWriter, r *http.Request) {
	var in entityInput
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	e, err := s.buildSource(r.Context(), in, nil)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	if !in.SkipTest {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		if err := s.eng.Test(ctx, e); err != nil {
			writeErr(w, 422, err.Error())
			return
		}
	}
	if err := s.st.PutSource(r.Context(), e); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// First sync right away.
	go s.eng.Sync(s.cfg.Background, e.ID)
	writeJSON(w, s.viewSource(r.Context(), e))
}

func (s *Server) updateSource(w http.ResponseWriter, r *http.Request) {
	existing, err := s.st.Source(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "no such source")
		return
	}
	var in entityInput
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Values == nil {
		// Only toggling or renaming: keep the stored values.
		in.Values = existing.Settings
	}
	e, err := s.buildSource(r.Context(), in, existing)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	if err := s.st.PutSource(r.Context(), e); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, s.viewSource(r.Context(), e))
}

func (s *Server) deleteSource(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteSource(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) syncSource(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.st.Source(r.Context(), id); err != nil {
		writeErr(w, 404, "no such source")
		return
	}
	if s.eng.Running(id) {
		writeErr(w, 409, "a sync is already running")
		return
	}
	var body struct {
		Full bool `json:"full"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Full {
		go s.eng.Resync(s.cfg.Background, id)
	} else {
		go s.eng.Sync(s.cfg.Background, id)
	}
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"started": true})
}

func atoi(s string, def int) int {
	if n, err := strconv.Atoi(s); err == nil && n >= 0 {
		return n
	}
	return def
}

func (s *Server) sourceActivities(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	limit := min(atoi(q.Get("limit"), 100), 1000)
	acts, err := s.st.Activities(r.Context(), store.ActivityFilter{SourceID: r.PathValue("id"), Limit: limit, Offset: atoi(q.Get("offset"), 0), Newest: true})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if acts == nil {
		acts = []model.Activity{}
	}
	total, _ := s.st.CountActivities(r.Context(), r.PathValue("id"))
	writeJSON(w, map[string]any{"activities": acts, "total": total})
}

// sourceHoldings compares what history adds up to with what the platform says.
func (s *Server) sourceHoldings(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	acts, err := s.st.Activities(r.Context(), store.ActivityFilter{SourceID: id})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	bal, at, err := s.st.Balances(r.Context(), id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	type line struct {
		Asset    model.Asset `json:"asset"`
		Replayed string      `json:"replayed"`
		Reported string      `json:"reported"`
		Gap      bool        `json:"gap"`
	}
	replayed := model.Holdings(acts)
	reported := map[string]model.Amount{}
	for _, b := range bal {
		cur, ok := reported[b.Asset.Key()]
		if !ok {
			cur = model.Amount{Asset: b.Asset}
		}
		cur.Qty = cur.Qty.Add(b.Qty)
		reported[b.Asset.Key()] = cur
	}
	gaps := map[string]bool{}
	for _, g := range model.Reconcile(acts, bal, s.eng.Tolerance()) {
		gaps[g.Asset.Key()] = true
	}
	seen := map[string]bool{}
	out := []line{}
	add := func(k string, a model.Asset) {
		if seen[k] {
			return
		}
		seen[k] = true
		rp, rr := replayed[k].Qty, reported[k].Qty
		if rp.IsZero() && rr.IsZero() {
			return
		}
		out = append(out, line{Asset: a, Replayed: rp.String(), Reported: rr.String(), Gap: gaps[k]})
	}
	for k, a := range reported {
		add(k, a.Asset)
	}
	for k, a := range replayed {
		add(k, a.Asset)
	}
	writeJSON(w, map[string]any{"holdings": out, "balancesAt": at})
}

// ---- destinations

type destView struct {
	*store.Entity
	Running   bool           `json:"running"`
	Counts    map[string]int `json:"counts"`
	SecretSet []string       `json:"secretsSet"`
}

func (s *Server) viewDest(ctx context.Context, e *store.Entity) destView {
	v := destView{Entity: e, Running: s.eng.Running("push:" + e.ID)}
	v.Counts, _ = s.st.DeliveryCounts(ctx, e.ID)
	if sec, err := s.eng.Secrets(e); err == nil {
		for k, val := range sec {
			if val != "" {
				v.SecretSet = append(v.SecretSet, k)
			}
		}
	}
	return v
}

func (s *Server) listDestinations(w http.ResponseWriter, r *http.Request) {
	ents, err := s.st.Destinations(r.Context())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	routes, _ := s.st.Routes(r.Context())
	views := []destView{}
	for _, e := range ents {
		views = append(views, s.viewDest(r.Context(), e))
	}
	if routes == nil {
		routes = []store.Route{}
	}
	writeJSON(w, map[string]any{"destinations": views, "routes": routes})
}

func (s *Server) buildDest(in entityInput, existing *store.Entity) (*store.Entity, error) {
	kind := in.Kind
	if existing != nil {
		kind = existing.Kind
	}
	def, err := dest.Get(kind)
	if err != nil {
		return nil, err
	}
	var old map[string]string
	if existing != nil {
		if old, err = s.eng.Secrets(existing); err != nil {
			return nil, err
		}
	}
	settings, secrets, err := split(def.Fields, in.Values, old)
	if err != nil {
		return nil, err
	}
	sealed, err := s.eng.Seal(secrets)
	if err != nil {
		return nil, err
	}
	e := &store.Entity{ID: newID(), Kind: kind, Name: strings.TrimSpace(in.Name), Settings: settings, Secrets: sealed, Enabled: true}
	if existing != nil {
		e.ID, e.Created, e.Enabled = existing.ID, existing.Created, existing.Enabled
	}
	if e.Name == "" {
		e.Name = def.Name
	}
	if in.Enabled != nil {
		e.Enabled = *in.Enabled
	}
	return e, nil
}

func (s *Server) createDestination(w http.ResponseWriter, r *http.Request) {
	var in entityInput
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	e, err := s.buildDest(in, nil)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	if !in.SkipTest {
		d, err := s.eng.BuildDest(e)
		if err == nil {
			ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
			defer cancel()
			_, err = d.Accounts(ctx)
		}
		if err != nil {
			writeErr(w, 422, err.Error())
			return
		}
	}
	if err := s.st.PutDestination(r.Context(), e); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, s.viewDest(r.Context(), e))
}

func (s *Server) updateDestination(w http.ResponseWriter, r *http.Request) {
	existing, err := s.st.Destination(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "no such destination")
		return
	}
	var in entityInput
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Values == nil {
		in.Values = existing.Settings
	}
	e, err := s.buildDest(in, existing)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	if err := s.st.PutDestination(r.Context(), e); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, s.viewDest(r.Context(), e))
}

func (s *Server) deleteDestination(w http.ResponseWriter, r *http.Request) {
	if err := s.st.DeleteDestination(r.Context(), r.PathValue("id")); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) destinationAccounts(w http.ResponseWriter, r *http.Request) {
	e, err := s.st.Destination(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "no such destination")
		return
	}
	d, err := s.eng.BuildDest(e)
	if err != nil {
		writeErr(w, 422, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	accs, err := d.Accounts(ctx)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	if accs == nil {
		accs = []dest.Account{}
	}
	writeJSON(w, accs)
}

func (s *Server) pushDestination(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.st.Destination(r.Context(), id); err != nil {
		writeErr(w, 404, "no such destination")
		return
	}
	if s.eng.Running("push:" + id) {
		writeErr(w, 409, "a push is already running")
		return
	}
	go s.eng.Push(s.cfg.Background, id)
	w.WriteHeader(http.StatusAccepted)
	writeJSON(w, map[string]bool{"started": true})
}

func (s *Server) putRoute(w http.ResponseWriter, r *http.Request) {
	var in store.Route
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.DestinationID == "" || in.SourceID == "" || in.Account == "" {
		writeErr(w, 422, "destination, source and account are required")
		return
	}
	if err := s.st.PutRoute(r.Context(), in); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	go s.eng.Push(s.cfg.Background, in.DestinationID)
	writeJSON(w, in)
}

func (s *Server) deleteRoute(w http.ResponseWriter, r *http.Request) {
	var in store.Route
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.st.DeleteRoute(r.Context(), in.DestinationID, in.SourceID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// resetRoute forgets deliveries so the next push sends everything again.
func (s *Server) resetRoute(w http.ResponseWriter, r *http.Request) {
	var in store.Route
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := s.st.ResetDeliveries(r.Context(), in.DestinationID, in.SourceID); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

func (s *Server) runs(w http.ResponseWriter, r *http.Request) {
	runs, err := s.st.Runs(r.Context(), r.URL.Query().Get("subject"), min(atoi(r.URL.Query().Get("limit"), 50), 200))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if r.URL.Query().Get("log") != "1" {
		for i := range runs {
			runs[i].Log = ""
		}
	}
	if runs == nil {
		runs = []store.Run{}
	}
	writeJSON(w, runs)
}

// static serves the UI, falling back to index.html for client routes.
func (s *Server) static(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Static == nil {
		http.NotFound(w, r)
		return
	}
	p := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if p == "" {
		p = "index.html"
	}
	if strings.HasPrefix(p, "api/") {
		writeErr(w, 404, "not found")
		return
	}
	if _, err := fs.Stat(s.cfg.Static, p); err != nil {
		p = "index.html"
	}
	if strings.HasPrefix(p, "assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	http.ServeFileFS(w, r, s.cfg.Static, p)
}
