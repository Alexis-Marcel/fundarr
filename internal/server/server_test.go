package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/dest"
	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/secret"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/internal/store"
)

type testSrc struct{ key string }

func (t testSrc) Activities(ctx context.Context, f source.Fetch) error {
	return f.Emit(model.Activity{NativeID: "1", Time: time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), Type: model.Deposit, Base: model.Amt(model.CryptoOrFiat("BTC"), "1")})
}
func (t testSrc) Balances(context.Context) ([]model.Balance, error) {
	return []model.Balance{{Amount: model.Amt(model.CryptoOrFiat("BTC"), "2")}}, nil
}
func (t testSrc) State() []byte { return nil }
func (t testSrc) Test(context.Context) error {
	if t.key != "good" {
		return errBadKey
	}
	return nil
}

type errString string

func (e errString) Error() string { return string(e) }

const errBadKey = errString("bad key")

type testDest struct{}

func (testDest) Accounts(context.Context) ([]dest.Account, error) {
	return []dest.Account{{ID: "acc", Name: "Main", Currency: "EUR"}}, nil
}
func (testDest) Push(_ context.Context, b dest.Batch) ([]dest.Result, error) {
	var out []dest.Result
	for _, a := range b.Activities {
		out = append(out, dest.Result{ActivityID: a.ID, Status: dest.Delivered})
	}
	return out, nil
}

func init() {
	source.Register(source.Definition{Kind: "test", Name: "Test", Category: "exchange",
		Fields: []source.Field{{Name: "key", Label: "Key", Secret: true}, {Name: "region", Label: "Region", Optional: true}},
		New:    func(c source.Config) (source.Source, error) { return testSrc{key: c.Get("key")}, nil }})
	dest.Register(dest.Definition{Kind: "testdest", Name: "TestDest", Fields: []source.Field{{Name: "url", Label: "URL"}},
		New: func(source.Config) (dest.Destination, error) { return testDest{}, nil }})
}

func setup(t *testing.T, mode string) (*httptest.Server, *engine.Engine, *http.Client) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secret.New(bytes.Repeat([]byte{9}, 32))
	eng := engine.New(st, box, slog.New(slog.NewTextHandler(io.Discard, nil)))
	srv, err := New(context.Background(), Config{Engine: eng, Version: "test",
		Static: fstest.MapFS{"index.html": {Data: []byte("<html>ui</html>")}, "assets/a.js": {Data: []byte("js")}},
		Auth:   AuthConfig{Mode: mode, PasswordHash: HashPassword("correct horse"), SessionKey: box.Key("s")}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)
	jar, _ := cookiejar.New(nil)
	return ts, eng, &http.Client{Jar: jar}
}

func do(t *testing.T, c *http.Client, method, url string, body any, out any) int {
	t.Helper()
	var r io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		r = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, url, r)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if out != nil {
		json.NewDecoder(resp.Body).Decode(out)
	}
	return resp.StatusCode
}

func waitIdle(t *testing.T, eng *engine.Engine, id string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for eng.Running(id) || eng.Running("push:"+id) {
		if time.Now().After(deadline) {
			t.Fatal("still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestPasswordAuthAndGuards(t *testing.T) {
	ts, _, c := setup(t, "password")
	var sess map[string]any
	do(t, c, "GET", ts.URL+"/api/session", nil, &sess)
	if sess["user"] != "" || sess["mode"] != "password" {
		t.Fatalf("session %v", sess)
	}
	if code := do(t, c, "GET", ts.URL+"/api/sources", nil, nil); code != 401 {
		t.Fatalf("unauthenticated %d", code)
	}
	if code := do(t, c, "POST", ts.URL+"/api/login", map[string]string{"password": "nope"}, nil); code != 401 {
		t.Fatalf("bad password %d", code)
	}
	if code := do(t, c, "POST", ts.URL+"/api/login", map[string]string{"password": "correct horse"}, nil); code != 200 {
		t.Fatalf("login %d", code)
	}
	do(t, c, "GET", ts.URL+"/api/session", nil, &sess)
	if sess["user"] != "admin" {
		t.Fatalf("session %v", sess)
	}
	// Writes need a JSON body and a same-origin Origin.
	req, _ := http.NewRequest("POST", ts.URL+"/api/sources", strings.NewReader("kind=test"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if resp, _ := c.Do(req); resp.StatusCode != 415 {
		t.Fatalf("form post %d", resp.StatusCode)
	}
	req, _ = http.NewRequest("POST", ts.URL+"/api/sources", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "https://evil.example")
	if resp, _ := c.Do(req); resp.StatusCode != 403 {
		t.Fatalf("cross origin %d", resp.StatusCode)
	}
	// A tampered cookie is refused.
	u := ts.URL + "/"
	for _, ck := range c.Jar.Cookies(mustURL(u)) {
		ck.Value = strings.Replace(ck.Value, "|", "x|", 1)
		c.Jar.SetCookies(mustURL(u), []*http.Cookie{ck})
	}
	if code := do(t, c, "GET", ts.URL+"/api/sources", nil, nil); code != 401 {
		t.Fatalf("tampered %d", code)
	}
	var out map[string]string
	do(t, c, "POST", ts.URL+"/api/logout", map[string]string{}, &out)
	if out["redirect"] != "/" {
		t.Fatalf("logout %v", out)
	}
	// Static UI with SPA fallback; health.
	resp, _ := http.Get(ts.URL + "/sources/abc")
	b, _ := io.ReadAll(resp.Body)
	if string(b) != "<html>ui</html>" || resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Fatalf("spa %q", b)
	}
	resp, _ = http.Get(ts.URL + "/assets/a.js")
	if resp.Header.Get("Cache-Control") == "no-cache" {
		t.Fatal("assets are immutable")
	}
	resp, _ = http.Get(ts.URL + "/healthz")
	if resp.StatusCode != 200 {
		t.Fatal("health")
	}
	if code := do(t, c, "GET", ts.URL+"/api/nope", nil, nil); code != 404 {
		t.Fatalf("api 404 %d", code)
	}
}

func mustURL(s string) *url.URL { u, _ := url.Parse(s); return u }

func TestSourcesDestinationsRoutes(t *testing.T) {
	ts, eng, c := setup(t, "none")
	var defs struct {
		Sources      []source.Definition `json:"sources"`
		Destinations []dest.Definition   `json:"destinations"`
	}
	do(t, c, "GET", ts.URL+"/api/definitions", nil, &defs)
	if len(defs.Sources) == 0 || len(defs.Destinations) == 0 {
		t.Fatal("definitions")
	}
	// Missing required field, then bad key, then good.
	var e map[string]string
	if code := do(t, c, "POST", ts.URL+"/api/sources", map[string]any{"kind": "test", "values": map[string]string{}}, &e); code != 422 || e["error"] != "Key is required" {
		t.Fatalf("required %d %v", code, e)
	}
	if code := do(t, c, "POST", ts.URL+"/api/sources/test", map[string]any{"kind": "test", "values": map[string]string{"key": "bad"}}, &e); code != 422 || e["error"] != "bad key" {
		t.Fatalf("test %d %v", code, e)
	}
	var src map[string]any
	if code := do(t, c, "POST", ts.URL+"/api/sources", map[string]any{"kind": "test", "name": " My test ", "values": map[string]string{"key": "good", "region": "eu"}}, &src); code != 200 {
		t.Fatalf("create %d %v", code, src)
	}
	id := src["id"].(string)
	if src["name"] != "My test" || src["settings"].(map[string]any)["region"] != "eu" {
		t.Fatalf("created %v", src)
	}
	if _, leaked := src["settings"].(map[string]any)["key"]; leaked {
		t.Fatal("secret in settings")
	}
	waitIdle(t, eng, id)

	var list []map[string]any
	do(t, c, "GET", ts.URL+"/api/sources", nil, &list)
	if len(list) != 1 || list[0]["activities"].(float64) != 1 || list[0]["secretsSet"].([]any)[0] != "key" || len(list[0]["gaps"].([]any)) != 1 {
		t.Fatalf("list %v", list)
	}
	var acts struct {
		Activities []model.Activity `json:"activities"`
		Total      int              `json:"total"`
	}
	do(t, c, "GET", ts.URL+"/api/sources/"+id+"/activities?limit=5", nil, &acts)
	if acts.Total != 1 || len(acts.Activities) != 1 {
		t.Fatalf("activities %+v", acts)
	}
	var hold struct {
		Holdings []map[string]any `json:"holdings"`
	}
	do(t, c, "GET", ts.URL+"/api/sources/"+id+"/holdings", nil, &hold)
	if len(hold.Holdings) != 1 || hold.Holdings[0]["replayed"] != "1" || hold.Holdings[0]["reported"] != "2" || hold.Holdings[0]["gap"] != true {
		t.Fatalf("holdings %+v", hold)
	}
	// Update keeps the secret when left empty; disabling without values.
	if code := do(t, c, "PUT", ts.URL+"/api/sources/"+id, map[string]any{"name": "Renamed", "values": map[string]string{"key": ""}, "intervalMinutes": 5}, &src); code != 200 {
		t.Fatalf("update %d %v", code, src)
	}
	if src["intervalMinutes"].(float64) != 15 || src["name"] != "Renamed" {
		t.Fatalf("updated %v", src)
	}
	off := false
	do(t, c, "PUT", ts.URL+"/api/sources/"+id, map[string]any{"enabled": &off}, &src)
	if src["enabled"] != false {
		t.Fatal("disable")
	}
	ent, _ := eng.Store.Source(context.Background(), id)
	if sec, _ := eng.Secrets(ent); sec["key"] != "good" {
		t.Fatal("secret kept")
	}
	if code := do(t, c, "POST", ts.URL+"/api/sources/"+id+"/sync", map[string]any{}, nil); code != 202 {
		t.Fatalf("sync %d", code)
	}
	waitIdle(t, eng, id)
	if code := do(t, c, "POST", ts.URL+"/api/sources/nope/sync", map[string]any{}, nil); code != 404 {
		t.Fatal("sync 404")
	}

	var d map[string]any
	if code := do(t, c, "POST", ts.URL+"/api/destinations", map[string]any{"kind": "testdest", "values": map[string]string{"url": "http://x"}}, &d); code != 200 {
		t.Fatalf("dest %d %v", code, d)
	}
	did := d["id"].(string)
	var accs []dest.Account
	do(t, c, "GET", ts.URL+"/api/destinations/"+did+"/accounts", nil, &accs)
	if len(accs) != 1 {
		t.Fatal("accounts")
	}
	if code := do(t, c, "PUT", ts.URL+"/api/routes", map[string]any{"destinationId": did, "sourceId": id}, nil); code != 422 {
		t.Fatal("route needs account")
	}
	do(t, c, "PUT", ts.URL+"/api/routes", map[string]any{"destinationId": did, "sourceId": id, "account": "acc"}, nil)
	time.Sleep(50 * time.Millisecond)
	waitIdle(t, eng, did)
	var dl struct {
		Destinations []map[string]any `json:"destinations"`
		Routes       []store.Route    `json:"routes"`
	}
	do(t, c, "GET", ts.URL+"/api/destinations", nil, &dl)
	if len(dl.Routes) != 1 || dl.Destinations[0]["counts"].(map[string]any)["delivered"].(float64) != 1 {
		t.Fatalf("destinations %+v", dl)
	}
	do(t, c, "POST", ts.URL+"/api/routes/reset", map[string]any{"destinationId": did, "sourceId": id}, nil)
	if code := do(t, c, "POST", ts.URL+"/api/destinations/"+did+"/push", map[string]any{}, nil); code != 202 {
		t.Fatal("push")
	}
	waitIdle(t, eng, did)
	do(t, c, "PUT", ts.URL+"/api/destinations/"+did, map[string]any{"name": "Renamed"}, &d)
	if d["name"] != "Renamed" {
		t.Fatal("dest rename")
	}
	var runs []store.Run
	do(t, c, "GET", ts.URL+"/api/runs?limit=10&log=1", nil, &runs)
	if len(runs) < 3 || runs[len(runs)-1].Log == "" {
		t.Fatalf("runs %+v", runs)
	}
	do(t, c, "DELETE", ts.URL+"/api/routes", map[string]any{"destinationId": did, "sourceId": id}, nil)
	do(t, c, "DELETE", ts.URL+"/api/destinations/"+did, map[string]any{}, nil)
	do(t, c, "DELETE", ts.URL+"/api/sources/"+id, map[string]any{}, nil)
	do(t, c, "GET", ts.URL+"/api/sources", nil, &list)
	if len(list) != 0 {
		t.Fatal("deleted")
	}
}

func TestAuthConfigErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := newAuth(ctx, AuthConfig{Mode: "password", PasswordHash: "plain"}); err == nil {
		t.Fatal("bad hash")
	}
	if _, err := newAuth(ctx, AuthConfig{Mode: "oidc", OIDCIssuer: "https://x", OIDCClientID: "c", PublicURL: "https://f"}); err == nil || !strings.Contains(err.Error(), "ALLOWED") {
		t.Fatalf("allowlist required: %v", err)
	}
	if _, err := newAuth(ctx, AuthConfig{Mode: "oidc"}); err == nil {
		t.Fatal("oidc fields")
	}
	if _, err := newAuth(ctx, AuthConfig{Mode: "magic"}); err == nil {
		t.Fatal("mode")
	}
	a := &auth{cfg: AuthConfig{OIDCAllowedSubs: []string{"u1"}, OIDCAllowedMails: []string{"me@x.io"}}}
	if !a.allowed("u1", "", false) || !a.allowed("u2", "ME@x.io", true) || a.allowed("u2", "me@x.io", false) || a.allowed("u3", "", true) {
		t.Fatal("allowlist")
	}
}
