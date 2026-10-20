package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/docs"
	"github.com/Alexis-Marcel/fundarr/internal/demo"
	"github.com/Alexis-Marcel/fundarr/internal/engine"
	"github.com/Alexis-Marcel/fundarr/internal/price"
	"github.com/Alexis-Marcel/fundarr/internal/secret"
)

// TestOpenAPI: every read route is described, nothing else is, and the
// description is served without a token.
func TestOpenAPI(t *testing.T) {
	ts, _, _ := setup(t, "password")
	spec := string(docs.OpenAPI)
	// Paths are the keys indented by two spaces under "paths:"; each has a get.
	described := map[string]bool{}
	for _, m := range regexp.MustCompile(`(?m)^  (/api/v1/\S+):\n    get:`).FindAllStringSubmatch(spec, -1) {
		described[m[1]] = true
	}
	srv, err := New(context.Background(), Config{Engine: engine.New(nil, nil, nil), Auth: AuthConfig{Mode: "none"}})
	if err != nil {
		t.Fatal(err)
	}
	routes := append(srv.v1, "GET /api/v1/openapi.yaml")
	for _, r := range routes {
		p, ok := strings.CutPrefix(r, "GET ")
		if !ok || !described[p] {
			t.Errorf("%s is not in docs/openapi.yaml", r)
		}
		delete(described, p)
	}
	for p := range described {
		t.Errorf("docs/openapi.yaml describes %s, which Fundarr does not serve", p)
	}

	resp, err := http.Get(ts.URL + "/api/v1/openapi.yaml")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/yaml" || !bytes.HasPrefix(b, []byte("openapi: 3.1")) {
		t.Fatalf("served %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
}

// TestDemo: the made-up data through the same API, readable with any
// token; nothing that would reach a platform runs.
func TestDemo(t *testing.T) {
	now := time.Now().UTC()
	st, err := demo.Open(context.Background(), now)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	box, _ := secret.New(bytes.Repeat([]byte{7}, 32))
	eng := engine.New(st, box, slog.New(slog.NewTextHandler(io.Discard, nil)))
	eng.Prices = price.New(st, nil)
	eng.Prices.Market = demo.Market
	bg, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	srv, err := New(context.Background(), Config{Engine: eng, Demo: true, Auth: AuthConfig{Mode: "none"}, Background: bg})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv)
	t.Cleanup(ts.Close)

	get := func(path string, out any) int {
		t.Helper()
		req, _ := http.NewRequest("GET", ts.URL+path, nil)
		req.Header.Set("Authorization", "Bearer fdr_demo")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if out != nil {
			if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
				t.Fatalf("%s: %v", path, err)
			}
		}
		return resp.StatusCode
	}
	var session struct{ Demo bool }
	if get("/api/session", &session); !session.Demo {
		t.Fatal("session does not say demo")
	}
	var sources []map[string]string
	if code := get("/api/v1/sources", &sources); code != 200 || len(sources) != 3 {
		t.Fatalf("sources %d %v", code, sources)
	}
	var acts struct {
		Activities []map[string]any
		Next       string
	}
	if code := get("/api/v1/activities?limit=100", &acts); code != 200 || len(acts.Activities) != 100 || acts.Next == "" {
		t.Fatalf("activities %d %d", code, len(acts.Activities))
	}
	var p struct {
		Total  map[string]string
		Assets []struct {
			Asset struct{ Symbol string }
			Value string
		}
		Unpriced []any
	}
	if code := get("/api/v1/portfolio", &p); code != 200 || len(p.Assets) != 4 || len(p.Unpriced) != 0 || p.Total["value"] == "0.00" {
		t.Fatalf("portfolio %d %+v", code, p)
	}
	var h struct{ Points []map[string]string }
	if code := get("/api/v1/portfolio/history?step=month", &h); code != 200 || len(h.Points) < 40 {
		t.Fatalf("history %d %d", code, len(h.Points))
	}
	var tax struct {
		Disposals []any
		Total     struct{ Exempt bool }
	}
	if code := get("/api/v1/tax/fr?year=2024", &tax); code != 200 || len(tax.Disposals) == 0 || tax.Total.Exempt {
		t.Fatalf("tax %d %+v", code, tax)
	}
	var holdings []struct{ Balances []any }
	if code := get("/api/v1/holdings", &holdings); code != 200 || len(holdings) != 3 || len(holdings[0].Balances) == 0 {
		t.Fatalf("holdings %d %+v", code, holdings)
	}
	if code := get("/api/v1/activities.csv?format=koinly", nil); code != 200 {
		t.Fatalf("csv %d", code)
	}

	// Syncs, pushes and new sources are refused; tokens still work.
	c := http.DefaultClient
	if code := do(t, c, "POST", ts.URL+"/api/sources/"+demo.Binance.ID+"/sync", map[string]any{}, nil); code != 403 {
		t.Fatalf("sync %d", code)
	}
	if code := do(t, c, "POST", ts.URL+"/api/sources", map[string]any{"kind": "test", "values": map[string]string{"key": "good"}}, nil); code != 403 {
		t.Fatalf("new source %d", code)
	}
	var made struct{ Token string }
	if code := do(t, c, "POST", ts.URL+"/api/tokens", map[string]string{"name": "Cairn"}, &made); code != 200 {
		t.Fatalf("token %d", code)
	}
	// A real token is marked used, even without sign-in.
	req, _ := http.NewRequest("GET", ts.URL+"/api/v1/sources", nil)
	req.Header.Set("Authorization", "Bearer "+made.Token)
	if resp, err := c.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("with the token %v", err)
	}
	var tokens []struct{ LastUsed time.Time }
	if do(t, c, "GET", ts.URL+"/api/tokens", nil, &tokens); len(tokens) != 1 || tokens[0].LastUsed.IsZero() {
		t.Fatalf("tokens %+v", tokens)
	}
	if eng.Running(demo.Binance.ID) {
		t.Fatal("a sync started")
	}
}
