package ghostfolio

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/dest"
	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

var (
	eur  = model.CryptoOrFiat("EUR")
	usdt = model.CryptoOrFiat("USDT")
	btc  = model.CryptoOrFiat("BTC")
	eth  = model.CryptoOrFiat("ETH")
	bnb  = model.CryptoOrFiat("BNB")
	iwda = model.Asset{Kind: model.Security, Symbol: "IWDA", ISIN: "IE00B4L5Y983"}
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
		a(model.Activity{Type: model.Deposit, Base: model.Amt(eur, "1000")}),                                                                   // 0 cash: skipped
		a(model.Activity{Type: model.Trade, Side: model.Buy, Base: model.Amt(usdt, "1080"), Quote: amt(eur, "1000")}),                          // 1 conversion: skipped
		a(model.Activity{Type: model.Trade, Side: model.Buy, Base: model.Amt(btc, "0.02"), Quote: amt(usdt, "800"), Fee: amt(usdt, "0.8")}),    // 2 fee in quote
		a(model.Activity{Type: model.Trade, Side: model.Buy, Base: model.Amt(eth, "0.1"), Quote: amt(usdt, "250"), Fee: amt(eth, "0.0001")}),   // 3 fee in base
		a(model.Activity{Type: model.Trade, Side: model.Sell, Base: model.Amt(btc, "0.005"), Quote: amt(usdt, "210"), Fee: amt(bnb, "0.001")}), // 4 fee in BNB
		a(model.Activity{Type: model.Trade, Side: model.Buy, Base: model.Amt(bnb, "0.5"), Quote: amt(eth, "0.05")}),                            // 5 swap
		a(model.Activity{Type: model.Deposit, Base: model.Amt(bnb, "1"), TxID: "0xabc", Network: "BSC"}),                                       // 6
		a(model.Activity{Type: model.Withdrawal, Base: model.Amt(btc, "0.01"), Fee: amt(btc, "0.0002")}),                                       // 7
		a(model.Activity{Type: model.Reward, Base: model.Amt(eth, "0.001"), Note: "staking"}),                                                  // 8
		a(model.Activity{Type: model.Reward, Base: model.Amt(usdt, "1.5"), Note: "earn"}),                                                      // 9
		a(model.Activity{Type: model.Interest, Base: model.Amt(eur, "0.3")}),                                                                   // 10
		a(model.Activity{Type: model.Fee, Base: model.Amt(eur, "2")}),                                                                          // 11
		a(model.Activity{Type: model.Tax, Base: model.Amt(btc, "0.00001")}),                                                                    // 12
		a(model.Activity{Type: model.Dividend, Base: model.Amt(eur, "4.2"), Quote: &model.Amount{Asset: iwda}}),                                // 13
		a(model.Activity{Type: model.Trade, Side: model.Buy, Base: model.Amt(iwda, "3"), Quote: amt(eur, "270"), Fee: amt(eur, "1")}),          // 14
		a(model.Activity{Type: model.Trade, Side: model.Buy, Base: model.Amt(usdt, "100"), Quote: amt(eth, "0.04")}),                           // 15 cash bought with an asset
		a(model.Activity{Type: "weird", Base: model.Amt(eur, "1")}),                                                                            // 16
	}
}

func TestInstrument(t *testing.T) {
	for _, c := range []struct {
		a      model.Asset
		ds, sy string
	}{
		{btc, "COINGECKO", "bitcoin"},
		{eth, "COINGECKO", "ethereum"},
		{bnb, "COINGECKO", "binancecoin"},
		{model.CryptoOrFiat("AVAX"), "COINGECKO", "avalanche-2"},
		{model.CryptoOrFiat("POL"), "COINGECKO", "polygon-ecosystem-token"},
		{usdt, "COINGECKO", "tether"},
		{model.CryptoOrFiat("RENDER"), "YAHOO", "RENDERUSD"},
		{iwda, "YAHOO", "IE00B4L5Y983"},
		{model.Asset{Kind: model.Security, Symbol: "AAPL"}, "YAHOO", "AAPL"},
	} {
		ds, sy := instrument(c.a)
		if ds != c.ds || sy != c.sy {
			t.Errorf("%v: %s %s, want %s %s", c.a, ds, sy, c.ds, c.sy)
		}
	}
}

func TestTranslate(t *testing.T) {
	g := &Ghostfolio{stableCash: true}
	var got []string
	for _, a := range activities() {
		rows, _, err := g.translate(a, "acc", "EUR", extra{})
		if err != nil {
			got = append(got, "skip")
			continue
		}
		var ts []string
		for j, r := range rows {
			want := "fundarr:" + a.ID
			if j > 0 {
				want += fmt.Sprintf("#%d", j+1)
			}
			if rowKey(r.Comment) != want || r.AccountID != "acc" || r.Date != a.Time.Format(time.RFC3339) {
				t.Fatalf("row meta %+v", r)
			}
			if (r.Type == "FEE" || r.Type == "INTEREST") != (r.DataSource == "") {
				t.Fatalf("data source of %+v", r)
			}
			ts = append(ts, r.Type+"/"+r.Currency+"/"+r.Symbol)
		}
		got = append(got, strings.Join(ts, "+"))
	}
	want := []string{
		"skip",
		"skip",
		"BUY/USD/bitcoin",
		"BUY/USD/ethereum",
		"SELL/USD/bitcoin+SELL/EUR/binancecoin",
		"SELL/EUR/ethereum+BUY/EUR/binancecoin",
		"BUY/EUR/binancecoin",
		"SELL/EUR/bitcoin+SELL/EUR/bitcoin",
		"BUY/EUR/ethereum",
		"INTEREST/USD/Interest",
		"INTEREST/EUR/Interest",
		"FEE/EUR/Fee",
		"SELL/EUR/bitcoin",
		"DIVIDEND/EUR/IE00B4L5Y983",
		"BUY/EUR/IE00B4L5Y983",
		"SELL/USD/ethereum",
		"skip",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("got\n%s\nwant\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	acts := activities()
	one := func(i int, x extra) []order {
		t.Helper()
		r, _, err := g.translate(acts[i], "acc", "EUR", x)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	// Fee in quote is the fee; fee in base lowers the units and raises the price.
	if r := one(2, extra{}); r[0].Fee != 0.8 || r[0].UnitPrice != 40000 || r[0].Quantity != 0.02 {
		t.Fatalf("fee in quote %+v", r[0])
	}
	if r := one(3, extra{}); r[0].Fee != 0 || r[0].Quantity != 0.0999 || r[0].UnitPrice < 2502.5 || r[0].UnitPrice > 2502.6 {
		t.Fatalf("fee in base %+v", r[0])
	}
	// Buying USDT with ETH sells ETH for USD.
	if r := one(15, extra{}); r[0].Quantity != 0.04 || r[0].UnitPrice != 2500 {
		t.Fatalf("cash side %+v", r[0])
	}
	// FEE and INTEREST carry their amount where Ghostfolio's own form does.
	if r := one(11, extra{}); r[0].Fee != 2 || r[0].Quantity != 0 || r[0].UnitPrice != 0 {
		t.Fatalf("fee %+v", r[0])
	}
	if r := one(10, extra{}); r[0].Fee != 0 || r[0].Quantity != 1 || r[0].UnitPrice != 0.3 {
		t.Fatalf("interest %+v", r[0])
	}
	// Rewards in units are free.
	if r := one(8, extra{cost: decimal.RequireFromString("2"), hasCost: true}); r[0].UnitPrice != 0 || !strings.Contains(r[0].Comment, "reward staking") {
		t.Fatalf("reward %+v", r[0])
	}
	// A deposit from another of your accounts comes in at the cost carried
	// over; without one, at 0, and the result says so.
	r, note, _ := g.translate(acts[6], "acc", "EUR", extra{cost: decimal.RequireFromString("550"), hasCost: true, peer: "Ledger"})
	if r[0].UnitPrice != 550 || !strings.Contains(r[0].Comment, "transfer from Ledger") || note != "" {
		t.Fatalf("paired deposit %+v %q", r[0], note)
	}
	if _, note, _ = g.translate(acts[6], "acc", "EUR", extra{}); !strings.Contains(note, "no price known") {
		t.Fatalf("note %q", note)
	}
	// Outgoing: at the cost when known, at 0 otherwise.
	if r := one(7, extra{peer: "Ledger"}); r[0].UnitPrice != 0 || r[0].Quantity != 0.01 || r[1].Quantity != 0.0002 || !strings.Contains(r[0].Comment, "transfer to Ledger") {
		t.Fatalf("withdrawal %+v", r)
	}
	if r := one(7, extra{cost: decimal.RequireFromString("400"), hasCost: true}); r[0].UnitPrice != 40000 || r[1].UnitPrice != 0 {
		t.Fatalf("withdrawal at cost %+v", r)
	}
	// A swap moves the carried cost from one asset to the other.
	if r := one(5, extra{cost: decimal.RequireFromString("100"), hasCost: true}); r[0].UnitPrice != 2000 || r[1].UnitPrice != 200 {
		t.Fatalf("swap %+v", r)
	}
	// Stablecoins as crypto: a USDT purchase is a swap; a USDT reward is units.
	g.stableCash = false
	if r := one(2, extra{}); r[0].Type != "SELL" || r[0].Symbol != "tether" || r[1].Symbol != "bitcoin" || r[2].Symbol != "tether" {
		t.Fatalf("stable as crypto %+v", r)
	}
	if r := one(9, extra{}); r[0].Type != "BUY" || r[0].Symbol != "tether" {
		t.Fatalf("stable reward %+v", r)
	}
	g.stableCash = true
	if _, _, err := g.translate(model.Activity{Type: model.Trade, Base: model.Amt(btc, "1")}, "a", "EUR", extra{}); err == nil {
		t.Fatal("trade without quote")
	}
	adj := model.Activity{ID: "x", Type: model.Adjustment, Side: model.Out, Base: model.Amt(btc, "0.1"), Note: "gone"}
	if r, _, _ := g.translate(adj, "acc", "EUR", extra{}); len(r) != 1 || r[0].Type != "SELL" || !strings.Contains(r[0].Comment, "balance correction") {
		t.Fatalf("correction %+v", r)
	}
}

// fakeGF answers like Ghostfolio: security token sign-in, JWT checks,
// accounts, activities, and an import refused whole when a symbol cannot be
// resolved ("BAD...") and that drops exact duplicates without a word.
type fakeGF struct {
	mu       sync.Mutex
	logins   int
	valid    map[string]bool
	stored   []map[string]any
	imports  int
	oldPaths bool // before /api/v1/activities existed
}

func (f *fakeGF) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(code int, v any) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(code)
		json.NewEncoder(w).Encode(v)
	}
	if r.Method == http.MethodPost && r.URL.Path == "/api/v1/auth/anonymous" {
		var body struct {
			AccessToken string `json:"accessToken"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		if body.AccessToken != "sec" {
			reply(403, map[string]any{"message": "Forbidden", "statusCode": 403})
			return
		}
		f.logins++
		tok := fmt.Sprintf("jwt%d", f.logins)
		f.valid[tok] = true
		reply(201, map[string]any{"authToken": tok})
		return
	}
	if !f.valid[strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")] {
		reply(401, map[string]any{"message": "Unauthorized", "statusCode": 401})
		return
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/api/v1/account":
		reply(200, map[string]any{"accounts": []any{
			map[string]any{"id": "acc", "name": "Binance", "currency": "EUR", "balance": 0},
		}})
	case r.Method == http.MethodGet && (r.URL.Path == "/api/v1/activities" && !f.oldPaths || r.URL.Path == "/api/v1/order" && f.oldPaths):
		var list []any
		for _, a := range f.stored {
			if a["accountId"] == r.URL.Query().Get("accounts") {
				list = append(list, a)
			}
		}
		reply(200, map[string]any{"activities": list, "count": len(list)})
	case r.Method == http.MethodPost && r.URL.Path == "/api/v1/import":
		f.imports++
		var body struct {
			Activities []map[string]any `json:"activities"`
		}
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&body); err != nil {
			reply(400, map[string]any{"message": []string{err.Error()}, "error": "Bad Request"})
			return
		}
		for i, a := range body.Activities {
			if s, _ := a["symbol"].(string); strings.HasPrefix(s, "BAD") {
				reply(400, map[string]any{"message": []string{fmt.Sprintf("activities.%d.symbol (%q) cannot be resolved by the data source (\"YAHOO\")", i, s)}, "error": "Bad Request"})
				return
			}
			switch a["type"] {
			case "BUY", "SELL", "DIVIDEND", "FEE", "INTEREST", "LIABILITY":
			default:
				reply(400, map[string]any{"message": []string{"type must be a valid enum value"}})
				return
			}
			if a["accountId"] != "acc" {
				reply(400, map[string]any{"message": []string{fmt.Sprintf("activities.%d.accountId is not valid", i)}})
				return
			}
		}
		var created []any
		for _, a := range body.Activities {
			dup := false
			for _, s := range f.stored {
				if s["comment"] == a["comment"] && s["date"] == a["date"] && s["symbol"] == a["symbol"] && s["type"] == a["type"] && s["quantity"] == a["quantity"] && s["unitPrice"] == a["unitPrice"] && s["fee"] == a["fee"] {
					dup = true
				}
			}
			if dup {
				continue
			}
			a["id"] = fmt.Sprintf("gf%d", len(f.stored)+1)
			f.stored = append(f.stored, a)
			created = append(created, a)
		}
		reply(201, map[string]any{"activities": created})
	default:
		reply(404, map[string]any{"message": "Cannot " + r.Method + " " + r.URL.Path, "statusCode": 404})
	}
}

func setup(t *testing.T) (*fakeGF, dest.Destination) {
	t.Helper()
	fake := &fakeGF{valid: map[string]bool{}}
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	def, err := dest.Get("ghostfolio")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := def.New(source.Config{Values: map[string]string{"url": srv.URL}}); err == nil {
		t.Fatal("token required")
	}
	d, err := def.New(source.Config{Values: map[string]string{"url": srv.URL + "/", "token": "sec"}, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return fake, d
}

func TestAuthAndAccounts(t *testing.T) {
	fake, d := setup(t)
	ctx := context.Background()
	accs, err := d.Accounts(ctx)
	if err != nil || len(accs) != 1 || accs[0] != (dest.Account{ID: "acc", Name: "Binance", Currency: "EUR"}) || fake.logins != 1 {
		t.Fatalf("accounts %+v %v (logins %d)", accs, err, fake.logins)
	}
	if _, err := d.Accounts(ctx); err != nil || fake.logins != 1 {
		t.Fatalf("token reused: %v, logins %d", err, fake.logins)
	}
	// The JWT expires: Fundarr signs in again, once.
	fake.valid = map[string]bool{}
	if _, err := d.Accounts(ctx); err != nil || fake.logins != 2 {
		t.Fatalf("refresh: %v, logins %d", err, fake.logins)
	}
	// A wrong security token says so.
	d.(*Ghostfolio).token, d.(*Ghostfolio).jwt = "nope", ""
	if _, err := d.Accounts(ctx); err == nil || !strings.Contains(err.Error(), "security token") {
		t.Fatalf("bad token: %v", err)
	}
}

func TestPush(t *testing.T) {
	fake, d := setup(t)
	ctx := context.Background()
	acts := activities()
	// One activity Ghostfolio cannot resolve: it fails alone.
	acts[14].Base.Asset = model.Asset{Kind: model.Security, Symbol: "BADX"}
	b := dest.Batch{Account: "acc", Activities: acts, Costs: map[string]decimal.Decimal{acts[6].ID: decimal.RequireFromString("550")}, Transfers: map[string]string{acts[6].ID: "Ledger"}}
	res, err := d.Push(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[dest.Status]int{}
	for _, r := range res {
		counts[r.Status]++
	}
	if counts[dest.Failed] != 1 || counts[dest.Skipped] != 3 || counts[dest.Delivered] != len(acts)-4 {
		t.Fatalf("counts %v %+v", counts, res)
	}
	if res[14].Status != dest.Failed || !strings.Contains(res[14].Message, "cannot be resolved") {
		t.Fatalf("failed %+v", res[14])
	}
	if res[0].Status != dest.Skipped || res[0].Message != errCash.Error() || res[16].Status != dest.Skipped {
		t.Fatalf("skipped %+v %+v", res[0], res[16])
	}
	if res[2].ExternalID == "" || res[7].Status != dest.Delivered {
		t.Fatalf("delivered %+v", res)
	}
	if !strings.Contains(res[5].Message, "no price known") {
		t.Fatalf("note %+v", res[5])
	}
	stored := len(fake.stored)
	var deposit map[string]any
	for _, s := range fake.stored {
		if rowKey(s["comment"].(string)) == "fundarr:"+acts[6].ID {
			deposit = s
		}
		if s["symbol"] == "BADX" {
			t.Fatal("refused activity stored")
		}
	}
	if deposit == nil || deposit["unitPrice"] != 550.0 || deposit["type"] != "BUY" || deposit["dataSource"] != "COINGECKO" {
		t.Fatalf("deposit %+v", deposit)
	}

	// Pushing again writes nothing: everything is already there.
	imports := fake.imports
	res, err = d.Push(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res {
		if want := map[int]dest.Status{0: dest.Skipped, 1: dest.Skipped, 16: dest.Skipped}[i]; want != "" {
			if r.Status != want {
				t.Fatalf("%d: %+v", i, r)
			}
		} else if i != 14 && r.Status != dest.Duplicate {
			t.Fatalf("%d not a duplicate: %+v", i, r)
		}
	}
	if len(fake.stored) != stored || fake.imports != imports+1 {
		t.Fatalf("re-push stored %d (was %d), imports %d", len(fake.stored), stored, fake.imports-imports)
	}

	// An activity half there (its fee row lost) gets only the missing row.
	for j, s := range fake.stored {
		if s["comment"] == "fundarr:"+acts[7].ID+"#2" {
			fake.stored = append(fake.stored[:j], fake.stored[j+1:]...)
		}
	}
	res, err = d.Push(ctx, dest.Batch{Account: "acc", Activities: acts[7:8]})
	if err != nil || res[0].Status != dest.Delivered || len(fake.stored) != stored {
		t.Fatalf("half there %+v %v %d", res, err, len(fake.stored))
	}

	// Ghostfolio's own duplicate check (rows it already has, the list not
	// showing them) counts as already there.
	g := d.(*Ghostfolio)
	fake.oldPaths = true // also: an older server lists activities at /api/v1/order
	g.listPath = ""
	saved := fake.stored
	fake.stored = nil
	res, err = d.Push(ctx, dest.Batch{Account: "acc", Activities: acts[2:3]})
	if err != nil || res[0].Status != dest.Delivered || g.listPath != "/api/v1/order" {
		t.Fatalf("old path %+v %v %q", res, err, g.listPath)
	}
	fake.stored = saved
	if _, err := d.Push(ctx, dest.Batch{Account: "nope", Activities: acts[2:3]}); err == nil {
		t.Fatal("unknown account")
	}
}

func TestPushSplitsRefusedImports(t *testing.T) {
	fake, d := setup(t)
	acts := activities()[2:9]
	acts[1].Base.Asset = model.CryptoOrFiat("BADA")
	acts[5].Base.Asset = model.CryptoOrFiat("BADB")
	res, err := d.Push(context.Background(), dest.Batch{Account: "acc", Activities: acts})
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res {
		want := dest.Delivered
		if i == 1 || i == 5 {
			want = dest.Failed
		}
		if r.Status != want {
			t.Fatalf("%d: %+v", i, r)
		}
	}
	if fake.imports > 13 { // about 2 per faulty activity and halving
		t.Fatalf("%d imports", fake.imports)
	}
	// A server error is not one activity's fault: the push fails.
	fake.valid = map[string]bool{}
	d.(*Ghostfolio).token = "nope"
	if _, err := d.Push(context.Background(), dest.Batch{Account: "acc", Activities: activities()[2:3]}); err == nil {
		t.Fatal("sign-in failure must fail the push")
	}
}
