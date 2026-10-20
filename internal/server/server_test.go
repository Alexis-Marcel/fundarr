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
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/dest"
	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/price"
	"github.com/Alexis-Marcel/fundarr/internal/secret"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/internal/store"
	"github.com/shopspring/decimal"
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
	return setupWith(t, AuthConfig{Mode: mode, PasswordHash: HashPassword("correct horse")})
}

func setupWith(t *testing.T, ac AuthConfig) (*httptest.Server, *engine.Engine, *http.Client) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "f.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secret.New(bytes.Repeat([]byte{9}, 32))
	eng := engine.New(st, box, slog.New(slog.NewTextHandler(io.Discard, nil)))
	ac.SessionKey = box.Key("s")
	ac.Credentials = st
	bg, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(context.Background(), Config{Engine: eng, Version: "test", Background: bg,
		Static: fstest.MapFS{"index.html": {Data: []byte("<html>ui</html>")}, "assets/a.js": {Data: []byte("js")}},
		Auth:   ac})
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
	if code := do(t, c, "POST", ts.URL+"/api/login", map[string]string{"username": "admin", "password": "nope"}, nil); code != 401 {
		t.Fatalf("bad password %d", code)
	}
	if code := do(t, c, "POST", ts.URL+"/api/login", map[string]string{"username": "someone", "password": "correct horse"}, nil); code != 401 {
		t.Fatalf("bad user %d", code)
	}
	if code := do(t, c, "POST", ts.URL+"/api/login", map[string]string{"username": "Admin", "password": "correct horse"}, nil); code != 200 {
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
	if len(list) != 1 || list[0]["activities"].(float64) != 1 || list[0]["secretsSet"].([]any)[0] != "key" || list[0]["gaps"] != nil || list[0]["corrections"].(float64) != 1 {
		t.Fatalf("list %v", list)
	}
	var acts struct {
		Activities []model.Activity `json:"activities"`
		Total      int              `json:"total"`
	}
	do(t, c, "GET", ts.URL+"/api/sources/"+id+"/activities?limit=5", nil, &acts)
	if acts.Total != 2 || len(acts.Activities) != 2 { // with the correction
		t.Fatalf("activities %+v", acts)
	}
	var hold struct {
		Holdings []map[string]any `json:"holdings"`
	}
	do(t, c, "GET", ts.URL+"/api/sources/"+id+"/holdings", nil, &hold)
	if len(hold.Holdings) != 1 || hold.Holdings[0]["replayed"] != "1" || hold.Holdings[0]["correction"] != "1" || hold.Holdings[0]["reported"] != "2" || hold.Holdings[0]["gap"] != false {
		t.Fatalf("holdings %+v", hold)
	}
	// The correction, then what it really was.
	var adjs []map[string]any
	do(t, c, "GET", ts.URL+"/api/sources/"+id+"/adjustments", nil, &adjs)
	if len(adjs) != 1 || adjs[0]["side"] != "in" || adjs[0]["delivered"] != false || adjs[0]["currency"] != "EUR" {
		t.Fatalf("adjustments %+v", adjs)
	}
	aid := adjs[0]["id"].(string)
	var e2 map[string]string
	if code := do(t, c, "POST", ts.URL+"/api/sources/"+id+"/adjustments/"+aid, map[string]any{"as": "withdrawal"}, &e2); code != 422 {
		t.Fatalf("coins in cannot be a withdrawal: %d %v", code, e2)
	}
	var manual model.Activity
	if code := do(t, c, "POST", ts.URL+"/api/sources/"+id+"/adjustments/"+aid, map[string]any{"as": "reward", "note": "airdrop"}, &manual); code != 200 || manual.Type != model.Reward {
		t.Fatalf("reclassify %d %+v", code, manual)
	}
	do(t, c, "GET", ts.URL+"/api/sources/"+id+"/adjustments", nil, &adjs)
	if len(adjs) != 0 {
		t.Fatalf("after %+v", adjs)
	}
	if code := do(t, c, "DELETE", ts.URL+"/api/sources/"+id+"/activities/"+manual.ID, map[string]any{}, nil); code != 200 {
		t.Fatalf("delete manual %d", code)
	}
	do(t, c, "GET", ts.URL+"/api/sources/"+id+"/adjustments", nil, &adjs)
	if len(adjs) != 1 {
		t.Fatalf("correction back %+v", adjs)
	}
	// Turning corrections off leaves the gap shown.
	no := false
	do(t, c, "PUT", ts.URL+"/api/sources/"+id, map[string]any{"adjust": &no}, &src)
	if src["adjust"] != false || src["corrections"].(float64) != 0 || len(src["gaps"].([]any)) != 1 {
		t.Fatalf("off %v", src)
	}
	yes := true
	do(t, c, "PUT", ts.URL+"/api/sources/"+id, map[string]any{"adjust": &yes}, &src)
	if src["adjust"] != true || src["corrections"].(float64) != 1 {
		t.Fatalf("on %v", src)
	}
	// Settings and transfers.
	var set map[string]any
	if code := do(t, c, "PUT", ts.URL+"/api/settings", map[string]any{"currency": "usd"}, &set); code != 200 || set["currency"] != "USD" {
		t.Fatalf("settings %d %v", code, set)
	}
	if code := do(t, c, "PUT", ts.URL+"/api/settings", map[string]any{"currency": "XYZ"}, nil); code != 422 {
		t.Fatal("bad currency")
	}
	var tr map[string]any
	if code := do(t, c, "GET", ts.URL+"/api/transfers", nil, &tr); code != 200 || len(tr["pairs"].([]any)) != 0 || tr["currency"] != "USD" {
		t.Fatalf("transfers %d %v", code, tr)
	}
	if code := do(t, c, "PUT", ts.URL+"/api/transfers", map[string]any{"outId": "a", "inId": "b", "kind": "pair"}, nil); code != 422 {
		t.Fatal("unknown activities cannot be paired")
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

func TestFirstRunSetup(t *testing.T) {
	ts, _, c := setupWith(t, AuthConfig{Mode: "password"})
	var sess map[string]any
	do(t, c, "GET", ts.URL+"/api/session", nil, &sess)
	if sess["setup"] != true || sess["local"] != true || sess["fixed"] != false {
		t.Fatalf("session %v", sess)
	}
	if code := do(t, c, "POST", ts.URL+"/api/login", map[string]string{"username": "admin", "password": "whatever1"}, nil); code != 401 {
		t.Fatal("no login before setup")
	}
	// From the internet (through a proxy): refused.
	b, _ := json.Marshal(map[string]string{"username": "me", "password": "longenough"})
	req, _ := http.NewRequest("POST", ts.URL+"/api/setup", bytes.NewReader(b))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.2")
	if resp, _ := c.Do(req); resp.StatusCode != 403 {
		t.Fatalf("public setup %d", resp.StatusCode)
	}
	if code := do(t, c, "POST", ts.URL+"/api/setup", map[string]string{"username": "me", "password": "short"}, nil); code != 422 {
		t.Fatal("short password")
	}
	if code := do(t, c, "POST", ts.URL+"/api/setup", map[string]string{"username": " me ", "password": "longenough"}, nil); code != 200 {
		t.Fatalf("setup %d", code)
	}
	do(t, c, "GET", ts.URL+"/api/session", nil, &sess)
	if sess["user"] != "me" || sess["setup"] != false {
		t.Fatalf("after setup %v", sess)
	}
	if code := do(t, c, "POST", ts.URL+"/api/setup", map[string]string{"username": "x", "password": "longenough"}, nil); code != 409 {
		t.Fatal("setup only once")
	}
	if code := do(t, c, "POST", ts.URL+"/api/password", map[string]string{"current": "wrong", "new": "newpassword"}, nil); code != 403 {
		t.Fatal("current password checked")
	}
	if code := do(t, c, "POST", ts.URL+"/api/password", map[string]string{"current": "longenough", "new": "newpassword"}, nil); code != 200 {
		t.Fatal("change password")
	}
	c2 := &http.Client{}
	jar, _ := cookiejar.New(nil)
	c2.Jar = jar
	if code := do(t, c2, "POST", ts.URL+"/api/login", map[string]string{"username": "ME", "password": "newpassword"}, nil); code != 200 {
		t.Fatal("login with new password")
	}
}

func TestProxyMode(t *testing.T) {
	ts, _, c := setupWith(t, AuthConfig{Mode: "proxy"})
	if code := do(t, c, "GET", ts.URL+"/api/sources", nil, nil); code != 401 {
		t.Fatal("no header, no access")
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/session", nil)
	req.Header.Set("Remote-User", "alice")
	resp, _ := c.Do(req)
	var sess map[string]any
	json.NewDecoder(resp.Body).Decode(&sess)
	if sess["user"] != "alice" || sess["mode"] != "proxy" {
		t.Fatalf("proxy session %v", sess)
	}
	if !fromPrivateNetwork(&http.Request{RemoteAddr: "100.92.1.2:5"}) || fromPrivateNetwork(&http.Request{RemoteAddr: "8.8.8.8:5"}) || fromPrivateNetwork(&http.Request{RemoteAddr: "garbage"}) {
		t.Fatal("private network detection")
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

func TestSecretListsEditMasked(t *testing.T) {
	old := "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNf\n0x52908400098527886E0F7030069857D2E4169EE7"
	m := maskLines(old)
	if strings.Contains(m, "Vz398") || strings.Count(m, "…") != 2 {
		t.Fatalf("masked %q", m)
	}
	lines := strings.Split(m, "\n")
	// Keep the second, drop the first, add one.
	got, err := unmaskLines(lines[1]+"\n0xabc", old)
	if err != nil || got != "0x52908400098527886E0F7030069857D2E4169EE7\n0xabc" {
		t.Errorf("unmask %q %v", got, err)
	}
	if _, err := unmaskLines("zpub6rFR…XXXXXX", old); err == nil {
		t.Error("an edited masked line must be refused")
	}
}

func TestSecretListsKeepLabels(t *testing.T) {
	old := "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNf\tSavings"
	m := maskLines(old)
	if !strings.HasSuffix(m, "\tSavings") || strings.Contains(m, "Vz398") {
		t.Fatalf("masked %q", m)
	}
	// Renamed, same key.
	val, _, _ := strings.Cut(m, "\t")
	got, err := unmaskLines(val+"\tCold storage", old)
	if err != nil || got != "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNf\tCold storage" {
		t.Errorf("rename %q %v", got, err)
	}
}

func TestReadAPIWithToken(t *testing.T) {
	ts, eng, c := setup(t, "password")
	if code := do(t, c, "POST", ts.URL+"/api/login", map[string]string{"username": "admin", "password": "correct horse"}, nil); code != 200 {
		t.Fatalf("login %d", code)
	}
	var src map[string]any
	do(t, c, "POST", ts.URL+"/api/sources", map[string]any{"kind": "test", "name": "T", "values": map[string]string{"key": "good"}}, &src)
	waitIdle(t, eng, src["id"].(string))
	var made struct {
		Token string `json:"token"`
		Info  struct {
			ID string `json:"id"`
		} `json:"info"`
	}
	if code := do(t, c, "POST", ts.URL+"/api/tokens", map[string]string{"name": "script"}, &made); code != 200 || !strings.HasPrefix(made.Token, "fdr_") {
		t.Fatalf("token %d %+v", code, made)
	}
	// A program without a session: the token, and only for /api/v1 reads.
	get := func(path, token string) (*http.Response, string) {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp, string(b)
	}
	if r, _ := get("/api/v1/activities", ""); r.StatusCode != 401 {
		t.Fatalf("no token %d", r.StatusCode)
	}
	if r, _ := get("/api/v1/activities", "fdr_nope"); r.StatusCode != 401 {
		t.Fatalf("bad token %d", r.StatusCode)
	}
	if r, _ := get("/api/sources", made.Token); r.StatusCode != 401 {
		t.Fatalf("token outside v1 %d", r.StatusCode)
	}
	r, body := get("/api/v1/activities?limit=1", made.Token)
	if r.StatusCode != 200 || !strings.Contains(body, `"source":{"id"`) || !strings.Contains(body, `"next":"`) {
		t.Fatalf("activities %d %s", r.StatusCode, body)
	}
	r, body = get("/api/v1/activities.csv?format=koinly", made.Token)
	if r.StatusCode != 200 || !strings.HasPrefix(body, "Date,Sent Amount,Sent Currency") {
		t.Fatalf("koinly %d %s", r.StatusCode, body)
	}
	if r, body = get("/api/v1/holdings", made.Token); r.StatusCode != 200 || !strings.Contains(body, `"balances"`) {
		t.Fatalf("holdings %d %s", r.StatusCode, body)
	}
	// Revoked, it stops working.
	if code := do(t, c, "DELETE", ts.URL+"/api/tokens/"+made.Info.ID, map[string]string{}, nil); code != 200 {
		t.Fatalf("revoke %d", code)
	}
	if r, _ := get("/api/v1/sources", made.Token); r.StatusCode != 401 {
		t.Fatalf("revoked %d", r.StatusCode)
	}
}

// fakePrices: 1 BTC = 50000 EUR every day; nothing else is listed.
func fakePrices(t *testing.T, st *store.Store) *price.Service {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/api/v3/klines":
			if q.Get("symbol") != "BTCEUR" {
				w.WriteHeader(400)
				w.Write([]byte(`{"code":-1121,"msg":"Invalid symbol."}`))
				return
			}
			start, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
			end, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
			var rows [][]any
			for ts := start; ts <= end && len(rows) < 1000; ts += 86400000 {
				rows = append(rows, []any{ts, "0", "0", "0", "50000", "0"})
			}
			json.NewEncoder(w).Encode(rows)
		case "/api/v5/market/history-candles":
			w.Write([]byte(`{"code":"51001","msg":"Instrument ID doesn't exist.","data":[]}`))
		default:
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"not found"}`))
		}
	}))
	t.Cleanup(srv.Close)
	p := price.New(st, srv.Client())
	p.BinanceURL, p.OKXURL, p.FXURL = srv.URL, srv.URL, srv.URL+"/fx"
	return p
}

func TestPortfolioAPI(t *testing.T) {
	ts, eng, c := setup(t, "password")
	eng.Prices = fakePrices(t, eng.Store)
	if code := do(t, c, "POST", ts.URL+"/api/login", map[string]string{"username": "admin", "password": "correct horse"}, nil); code != 200 {
		t.Fatalf("login %d", code)
	}
	var src map[string]any
	do(t, c, "POST", ts.URL+"/api/sources", map[string]any{"kind": "test", "name": "T", "values": map[string]string{"key": "good"}}, &src)
	id := src["id"].(string)
	waitIdle(t, eng, id)
	// Besides the source's deposit of 1 BTC (2024-01-01): 0.1 BTC sold for
	// 6000 € in 2025, 1 € fee.
	sale := model.Activity{SourceID: id, NativeID: "sale", Time: time.Date(2025, 3, 1, 10, 0, 0, 0, time.UTC), Type: model.Trade, Side: model.Sell,
		Base: model.Amt(model.CryptoOrFiat("BTC"), "0.1"), Quote: &[]model.Amount{model.Amt(model.CryptoOrFiat("EUR"), "6000")}[0],
		Fee: &[]model.Amount{model.Amt(model.CryptoOrFiat("EUR"), "1")}[0]}
	sale.ID = model.ActivityID(id, sale.NativeID)
	if _, err := eng.Store.AddActivities(context.Background(), []model.Activity{sale}); err != nil {
		t.Fatal(err)
	}
	var made struct {
		Token string `json:"token"`
	}
	do(t, c, "POST", ts.URL+"/api/tokens", map[string]string{"name": "tracker"}, &made)
	get := func(path string, out any) int {
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer "+made.Token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			json.NewDecoder(resp.Body).Decode(out)
		}
		return resp.StatusCode
	}

	var p struct {
		Currency string `json:"currency"`
		Total    struct {
			Value, Invested, Cost, Unrealized, Realized, Rewards string
		} `json:"total"`
		Assets []struct {
			Asset   model.Asset `json:"asset"`
			Qty     string      `json:"qty"`
			Price   string      `json:"price"`
			Value   string      `json:"value"`
			Share   string      `json:"share"`
			Sources []struct {
				ID, Name, Qty string
			} `json:"sources"`
		} `json:"assets"`
		Sources []struct {
			ID, Name, Kind, Value string
		} `json:"sources"`
		Unpriced []model.Asset `json:"unpriced"`
	}
	if code := get("/api/v1/portfolio", &p); code != 200 || p.Currency != "EUR" || len(p.Assets) != 1 || p.Unpriced == nil {
		t.Fatalf("portfolio %d %+v", code, p)
	}
	btc := p.Assets[0]
	// The balance check may add the BTC the source reports beyond its history.
	qty := decimal.RequireFromString(btc.Qty)
	if btc.Asset.Symbol != "BTC" || btc.Price != "50000.00" || btc.Value != qty.Mul(decimal.NewFromInt(50000)).StringFixed(2) ||
		btc.Share != "1.000" || len(btc.Sources) != 1 || btc.Sources[0].Name != "T" || p.Total.Value != btc.Value {
		t.Fatalf("BTC %+v", p)
	}
	if len(p.Sources) != 1 || p.Sources[0].Kind != "test" || p.Sources[0].Value != btc.Value {
		t.Fatalf("sources %+v", p.Sources)
	}
	// Bought at 50000, sold at 60000: 1000 € gained, less the fee.
	if p.Total.Realized != "999.00" || p.Total.Rewards != "0.00" {
		t.Fatalf("totals %+v", p.Total)
	}

	var h struct {
		Points []struct{ Date, Value, Invested string } `json:"points"`
	}
	if code := get("/api/v1/portfolio/history?from=2024-01-01&to=2024-03-31&step=month", &h); code != 200 || len(h.Points) != 3 {
		t.Fatalf("history %d %+v", code, h)
	}
	if pt := h.Points[1]; pt.Date != "2024-02-29" || pt.Value != "50000.00" || pt.Invested != "50000.00" {
		t.Fatalf("February %+v", pt)
	}
	if code := get("/api/v1/portfolio/history", &h); code != 200 || h.Points[0].Date != "2024-01-01" {
		t.Fatalf("default history %d %+v", code, h.Points[:1])
	}
	if code := get("/api/v1/portfolio/history?step=year", nil); code != 400 {
		t.Fatalf("bad step %d", code)
	}
	if code := get("/api/v1/portfolio/history?from=2000-01-01&step=day", nil); code != 422 {
		t.Fatalf("too many points %d", code)
	}

	var tx struct {
		Year      int    `json:"year"`
		Currency  string `json:"currency"`
		Disposals []struct {
			Source                                                       struct{ ID, Name string } `json:"source"`
			Asset                                                        model.Asset               `json:"asset"`
			Qty, Price, Fees, Portfolio, Acquisition, CapitalShare, Gain string
		} `json:"disposals"`
		Total struct {
			Disposals, Gain, Tax, Rate string
			Exempt                     bool
		} `json:"total"`
		Notes []string `json:"notes"`
	}
	if code := get("/api/v1/tax/fr?year=2025", &tx); code != 200 || tx.Year != 2025 || tx.Currency != "EUR" || len(tx.Disposals) != 1 {
		t.Fatalf("tax %d %+v", code, tx)
	}
	// The deposited BTC counts for 0: the whole price is a gain.
	d := tx.Disposals[0]
	if d.Source.Name != "T" || d.Qty != "0.1" || d.Price != "6000.00" || d.Fees != "1.00" || d.Portfolio != "50000.00" ||
		d.Acquisition != "0.00" || d.CapitalShare != "0.00" || d.Gain != "5999.00" {
		t.Fatalf("disposal %+v", d)
	}
	if tx.Total.Disposals != "6000.00" || tx.Total.Gain != "5999.00" || tx.Total.Exempt || tx.Total.Rate != "0.314" || tx.Total.Tax != "1883.69" || len(tx.Notes) == 0 {
		t.Fatalf("total %+v", tx.Total)
	}
	// Taxed as income when received, it is worth 50000: 50000 × 6000 / 50000.
	get("/api/v1/tax/fr?year=2025&rewards=income", &tx)
	if d := tx.Disposals[0]; d.Acquisition != "50000.00" || d.CapitalShare != "6000.00" || d.Gain != "-1.00" || tx.Total.Tax != "0.00" {
		t.Fatalf("income %+v", tx)
	}
	if code := get("/api/v1/tax/fr?year=1990", nil); code != 400 {
		t.Fatalf("bad year %d", code)
	}
	if code := get("/api/v1/tax/fr?rewards=maybe", nil); code != 400 {
		t.Fatalf("bad rewards %d", code)
	}
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/portfolio", nil)
	if r, _ := http.DefaultClient.Do(req); r.StatusCode != 401 {
		t.Fatalf("no token %d", r.StatusCode)
	}
}
