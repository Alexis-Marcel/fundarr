package wealthfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/dest"
	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
)

var (
	eur  = model.CryptoOrFiat("EUR")
	usdt = model.CryptoOrFiat("USDT")
	btc  = model.CryptoOrFiat("BTC")
	eth  = model.CryptoOrFiat("ETH")
	bnb  = model.CryptoOrFiat("BNB")
	t0   = time.Date(2024, 1, 2, 10, 0, 0, 0, time.UTC)
)

func amt(a model.Asset, q string) *model.Amount { v := model.Amt(a, q); return &v }

func activities() []model.Activity {
	n := 0
	a := func(x model.Activity) model.Activity {
		n++
		x.NativeID = fmt.Sprint(n)
		x.ID = model.ActivityID("src", x.NativeID)
		x.Time = t0.Add(time.Duration(n) * time.Hour)
		return x
	}
	return []model.Activity{
		a(model.Activity{Type: model.Deposit, Base: model.Amt(eur, "1000")}),
		a(model.Activity{Type: model.Trade, Side: model.Buy, Base: model.Amt(usdt, "1080"), Quote: amt(eur, "1000")}),                          // conversion
		a(model.Activity{Type: model.Trade, Side: model.Buy, Base: model.Amt(btc, "0.02"), Quote: amt(usdt, "800"), Fee: amt(usdt, "0.8")}),    // fee in quote
		a(model.Activity{Type: model.Trade, Side: model.Buy, Base: model.Amt(eth, "0.1"), Quote: amt(usdt, "250"), Fee: amt(eth, "0.0001")}),   // fee in base
		a(model.Activity{Type: model.Trade, Side: model.Sell, Base: model.Amt(btc, "0.005"), Quote: amt(usdt, "210"), Fee: amt(bnb, "0.001")}), // fee in BNB
		a(model.Activity{Type: model.Trade, Side: model.Buy, Base: model.Amt(bnb, "0.5"), Quote: amt(eth, "0.05")}),                            // crypto for crypto
		a(model.Activity{Type: model.Deposit, Base: model.Amt(bnb, "1"), TxID: "0xabc", Network: "BSC"}),
		a(model.Activity{Type: model.Withdrawal, Base: model.Amt(btc, "0.01"), Fee: amt(btc, "0.0002")}),
		a(model.Activity{Type: model.Reward, Base: model.Amt(eth, "0.001"), Note: "staking"}),
		a(model.Activity{Type: model.Reward, Base: model.Amt(usdt, "1.5"), Note: "earn"}),
		a(model.Activity{Type: model.Interest, Base: model.Amt(eur, "0.3")}),
		a(model.Activity{Type: model.Fee, Base: model.Amt(eur, "2")}),
		a(model.Activity{Type: model.Tax, Base: model.Amt(btc, "0.00001")}),
		a(model.Activity{Type: model.Dividend, Base: model.Amt(eur, "4.2"), Quote: &model.Amount{Asset: model.Asset{Kind: model.Security, Symbol: "IWDA", ISIN: "IE00B4L5Y983"}}}),
		a(model.Activity{Type: model.Withdrawal, Base: model.Amt(eur, "100")}),
		a(model.Activity{Type: "weird", Base: model.Amt(eur, "1")}),
	}
}

func TestTranslate(t *testing.T) {
	w := &Wealthfolio{stableCash: true}
	types := []string{}
	for _, a := range activities() {
		rows, err := w.translate(a, "acc", "EUR")
		if a.Type == "weird" {
			if err == nil {
				t.Fatal("unknown type must fail")
			}
			continue
		}
		if err != nil {
			t.Fatalf("%s: %v", a.NativeID, err)
		}
		var ts []string
		for _, r := range rows {
			if !strings.HasPrefix(r.Comment, "fundarr:"+a.ID) || !r.ForceImport || r.AccountID != "acc" {
				t.Fatalf("row meta %+v", r)
			}
			ts = append(ts, r.ActivityType+"/"+r.Currency+"/"+r.Symbol)
		}
		types = append(types, strings.Join(ts, "+"))
	}
	want := []string{
		"DEPOSIT/EUR/",
		"WITHDRAWAL/EUR/+DEPOSIT/USD/",
		"BUY/USD/BTC",
		"BUY/USD/ETH",
		"SELL/USD/BTC+TRANSFER_OUT/EUR/BNB",
		"TRANSFER_OUT/EUR/ETH+TRANSFER_IN/EUR/BNB",
		"TRANSFER_IN/EUR/BNB",
		"TRANSFER_OUT/EUR/BTC+TRANSFER_OUT/EUR/BTC",
		"TRANSFER_IN/EUR/ETH",
		"INTEREST/USD/",
		"INTEREST/EUR/",
		"FEE/EUR/",
		"TRANSFER_OUT/EUR/BTC",
		"DIVIDEND/EUR/IWDA",
		"WITHDRAWAL/EUR/",
	}
	if strings.Join(types, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(types, "\n"), strings.Join(want, "\n"))
	}
	// Fee in quote is a fee; fee in base reduces units and raises the price.
	acts := activities()
	r, _ := w.translate(acts[2], "acc", "EUR")
	if *r[0].Fee != 0.8 || *r[0].UnitPrice != 40000 || *r[0].Quantity != 0.02 {
		t.Fatalf("fee in quote %+v", r[0])
	}
	r, _ = w.translate(acts[3], "acc", "EUR")
	if r[0].Fee != nil || *r[0].Quantity != 0.0999 || *r[0].UnitPrice < 2502.5 || *r[0].UnitPrice > 2502.6 {
		t.Fatalf("fee in base %+v", r[0])
	}
	// Stablecoins as crypto: a USDT purchase is crypto for crypto.
	w.stableCash = false
	r, _ = w.translate(acts[2], "acc", "EUR")
	if r[0].ActivityType != "TRANSFER_OUT" || r[0].Symbol != "USDT" {
		t.Fatalf("stable as crypto %+v", r)
	}
	if _, err := w.translate(model.Activity{Type: model.Trade, Base: model.Amt(btc, "1")}, "a", "EUR"); err == nil {
		t.Fatal("trade without quote")
	}
}

// fakeWF answers the MCP calls like Wealthfolio, rejecting rows whose symbol is "BAD".
type fakeWF struct {
	committed [][]map[string]any
	auth      string
}

func (f *fakeWF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.auth = r.Header.Get("Authorization")
	var req struct {
		ID     int            `json:"id"`
		Method string         `json:"method"`
		Params map[string]any `json:"params"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	w.Header().Set("Mcp-Session-Id", "sess")
	if req.Method == "notifications/initialized" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	result := map[string]any{}
	if req.Method == "tools/call" {
		name := req.Params["name"].(string)
		args, _ := req.Params["arguments"].(map[string]any)
		var structured any
		switch name {
		case "get_accounts":
			structured = map[string]any{"accounts": []any{
				map[string]any{"id": "acc", "name": "Binance", "currency": "EUR", "isActive": true},
				map[string]any{"id": "old", "name": "Old", "currency": "EUR", "isActive": false},
			}}
		case "prepare_activity_import", "commit_activity_import":
			rows := []any{}
			acts, _ := args["activities"].([]any)
			var failed []any
			for _, x := range acts {
				m := x.(map[string]any)
				ok := m["symbol"] != "BAD"
				res := map[string]any{"lineNumber": m["lineNumber"], "isValid": ok, "isDuplicate": false}
				if !ok {
					res["errors"] = []string{"symbol: unknown"}
					failed = append(failed, res)
				}
				rows = append(rows, res)
			}
			if name == "prepare_activity_import" {
				structured = map[string]any{"rows": rows}
			} else {
				var list []map[string]any
				for _, x := range acts {
					list = append(list, x.(map[string]any))
				}
				f.committed = append(f.committed, list)
				structured = map[string]any{"importRunId": "run1", "summary": map[string]any{"imported": len(acts), "success": true}, "failed": failed}
			}
		}
		result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "ok"}}, "structuredContent": structured, "isError": false}
	}
	// Answer as an event stream, like rmcp does.
	w.Header().Set("Content-Type", "text/event-stream")
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
	fmt.Fprintf(w, "data: \n\ndata: %s\n\n", b)
}

func TestPushWithFakeServer(t *testing.T) {
	fake := &fakeWF{}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	def, _ := dest.Get("wealthfolio")
	if _, err := def.New(source.Config{Values: map[string]string{"url": srv.URL}}); err == nil {
		t.Fatal("token required")
	}
	d, err := def.New(source.Config{Values: map[string]string{"url": srv.URL + "/", "token": "wfp_x"}, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	accs, err := d.Accounts(ctx)
	if err != nil || len(accs) != 1 || accs[0].ID != "acc" || fake.auth != "Bearer wfp_x" {
		t.Fatalf("accounts %+v %v", accs, err)
	}
	acts := activities()
	// One activity Wealthfolio rejects: it fails alone, the others go in.
	acts[6].Base.Asset.Symbol = "BAD"
	res, err := d.Push(ctx, dest.Batch{Account: "acc", Activities: acts})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[dest.Status]int{}
	for _, r := range res {
		counts[r.Status]++
	}
	if counts[dest.Failed] != 1 || counts[dest.Skipped] != 1 || counts[dest.Delivered] != len(acts)-2 {
		t.Fatalf("counts %v %+v", counts, res)
	}
	if res[6].Status != dest.Failed || res[6].Message != "symbol: unknown" || res[0].ExternalID != "run1" {
		t.Fatalf("results %+v", res)
	}
	if len(fake.committed) != 1 {
		t.Fatal("one commit")
	}
	for _, r := range fake.committed[0] {
		if r["symbol"] == "BAD" {
			t.Fatal("rejected row committed")
		}
	}
	if _, err := d.Push(ctx, dest.Batch{Account: "nope", Activities: acts[:1]}); err == nil {
		t.Fatal("unknown account")
	}
}

// Against a real Wealthfolio: WF_TEST_URL, WF_TEST_TOKEN, WF_TEST_ACCOUNT.
func TestPushLive(t *testing.T) {
	u, tok, acc := os.Getenv("WF_TEST_URL"), os.Getenv("WF_TEST_TOKEN"), os.Getenv("WF_TEST_ACCOUNT")
	if u == "" {
		t.Skip("WF_TEST_URL not set")
	}
	def, _ := dest.Get("wealthfolio")
	d, err := def.New(source.Config{Values: map[string]string{"url": u, "token": tok}})
	if err != nil {
		t.Fatal(err)
	}
	acts := activities()[:15]
	res, err := d.Push(context.Background(), dest.Batch{Account: acc, Activities: acts})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res {
		t.Logf("%2d %-10s %-9s %s", i+1, acts[i].Type, r.Status, r.Message)
		if r.Status != dest.Delivered {
			t.Errorf("activity %d not delivered: %s", i+1, r.Message)
		}
	}
}
