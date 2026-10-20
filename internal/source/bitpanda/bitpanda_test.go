package bitpanda

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

const testKey = "bp-test-key-0123456789"

// Catalog UUIDs (made up, in Bitpanda's format).
const (
	eurID    = "b88b8466-efe3-11eb-b56f-0691764446a7"
	btcID    = "b86c034b-efe3-11eb-b56f-0691764446a7"
	ethID    = "b86c113a-efe3-11eb-b56f-0691764446a7"
	bestID   = "b86c97b7-efe3-11eb-b56f-0691764446a7"
	goldID   = "b86c88d4-efe3-11eb-b56f-0691764446a7"
	aaplOld  = "00aa0001-b0f4-11ec-a6ac-0a686dc2c129" // legacy "security"
	aaplEq   = "1f0f0002-0000-6000-8000-000000000002" // tradeable "equity_security", same ISIN
	eurotech = "1f0f13b6-8630-6a24-b2e4-859c8eda3240" // stock whose symbol is ETH
	aaplISIN = "US0378331005"
)

var catalog = []map[string]any{
	{"id": btcID, "name": "Bitcoin", "symbol": "BTC", "group": "coin", "type": "cryptocoin"},
	{"id": ethID, "name": "Ethereum", "symbol": "ETH", "group": "coin", "type": "cryptocoin"},
	{"id": bestID, "name": "Bitpanda Ecosystem Token", "symbol": "BEST", "group": "coin", "type": "cryptocoin"},
	{"id": goldID, "name": "Gold", "symbol": "XAU", "group": "metal", "type": "commodity"},
	{"id": aaplOld, "name": "Apple", "symbol": "AAPL", "isin": aaplISIN, "group": "stock", "type": "security"},
	{"id": aaplEq, "name": "Apple Inc.", "symbol": "AAPL", "isin": aaplISIN, "group": "equity_stock", "type": "equity_security"},
	{"id": eurotech, "name": "Eurotech SpA", "symbol": "ETH", "isin": "IT0003895668", "group": "equity_stock", "type": "equity_security"},
}

// fake is a Bitpanda Public API serving the documented response shapes.
type fake struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	ops       []map[string]any // newest first, as the API lists them
	portfolio []map[string]any
	maxPage   int                 // the fake's own page size cap, to force pagination
	fail      map[string][]int    // statuses to answer first, per path
	calls     map[string][]string // raw queries, per path
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, maxPage: 4, fail: map[string][]int{}, calls: map[string][]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[r.URL.Path] = append(f.calls[r.URL.Path], r.URL.RawQuery)
	w.Header().Set("Content-Type", "application/json")
	if fl := f.fail[r.URL.Path]; len(fl) > 0 {
		f.fail[r.URL.Path] = fl[1:]
		if fl[0] == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "2")
		}
		w.WriteHeader(fl[0])
		w.Write([]byte(`{"error":{"code":"too_many_requests"}}`))
		return
	}
	public := r.URL.Path == "/v1/assets" || r.URL.Path == "/v1/currencies"
	if !public && r.Header.Get("x-api-key") != testKey {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"errors":[{"code":"unauthorized","status":401,"title":"Credentials / Access token wrong"}]}`))
		return
	}
	q := r.URL.Query()
	switch r.URL.Path {
	case "/v1/currencies":
		json.NewEncoder(w).Encode(map[string]any{"data": []any{
			map[string]any{"id": eurID, "symbol": "EUR", "name": "Euro"},
			map[string]any{"id": "b88b8879-efe3-11eb-b56f-0691764446a7", "symbol": "USD", "name": "US Dollar"},
		}})
	case "/v1/assets":
		ids := map[string]bool{}
		for _, id := range strings.Split(q.Get("id"), ",") {
			ids[id] = true
		}
		var out []any
		for _, a := range catalog {
			if ids[a["id"].(string)] {
				out = append(out, a)
			}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": out, "has_next_page": false})
	case "/v1/portfolio":
		json.NewEncoder(w).Encode(map[string]any{"data": f.portfolio})
	case "/v1/operations":
		size, err := strconv.Atoi(q.Get("page_size"))
		if err != nil || size < 1 || size > 100 {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"code":"page_size.invalid"}}`))
			return
		}
		size = min(size, f.maxPage)
		var list []map[string]any
		from, _ := time.Parse(time.RFC3339Nano, q.Get("from"))
		for _, op := range f.ops {
			ts, _ := time.Parse(time.RFC3339, op["transactions"].([]any)[0].(map[string]any)["credited_at"].(string))
			if q.Get("from") == "" || ts.IsZero() || !ts.Before(from) {
				list = append(list, op)
			}
		}
		start := 0
		if c := q.Get("cursor"); c != "" {
			start, _ = strconv.Atoi(strings.TrimPrefix(c, "c"))
		}
		end := min(start+size, len(list))
		page := map[string]any{"data": list[start:end], "self_cursor": fmt.Sprintf("c%d", start), "has_next_page": end < len(list)}
		if end < len(list) {
			page["next_cursor"] = fmt.Sprintf("c%d", end)
		}
		json.NewEncoder(w).Encode(page)
	default:
		w.WriteHeader(http.StatusNotFound)
		w.Write([]byte(`{"error":{"code":"not_found"}}`))
	}
}

func (f *fake) source(t *testing.T, key string, state []byte) (*Source, *[]time.Duration) {
	s, err := New(source.Config{Values: map[string]string{"api_key": key, "base_url": f.srv.URL}, HTTP: f.srv.Client(), State: state})
	if err != nil {
		t.Fatal(err)
	}
	src := s.(*Source)
	var waits []time.Duration
	src.c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	return src, &waits
}

var t0 = time.Date(2025, 3, 1, 9, 0, 0, 0, time.UTC)

func at(h int) string { return t0.Add(time.Duration(h) * time.Hour).Format(time.RFC3339) }

// leg is one transaction: unit "c:" or "a:" + UUID, signed amount.
func leg(id, u, qty, when string, extra map[string]any) map[string]any {
	flow := "INCOMING"
	if strings.HasPrefix(qty, "-") {
		flow, qty = "OUTGOING", qty[1:]
	}
	m := map[string]any{"transaction_id": id, "wallet_id": "w-" + u, "wallet_owner": "shared-default", "flow": flow,
		"credited_at": when, "fee_amount": nil}
	amt := map[string]any{"value": qty}
	if strings.HasPrefix(u, "c:") {
		m["currency_id"], amt["currency_id"] = u[2:], u[2:]
	} else {
		m["asset_id"], amt["asset_id"] = u[2:], u[2:]
	}
	m["asset_amount"] = amt
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func op(id, typ string, legs ...map[string]any) map[string]any {
	ts := make([]any, len(legs))
	for i, l := range legs {
		ts[i] = l
	}
	return map[string]any{"operation_id": id, "operation_type": typ, "transactions": ts}
}

func tradeRef(id, fee string) map[string]any {
	return map[string]any{"trade": map[string]any{"trade_id": id, "fee": map[string]any{"value": fee, "currency_id": eurID},
		"fee_percentage": "1.49", "rate": "50000", "rate_with_fee": "50745", "to_eur_rate": "1"}}
}

const (
	eur  = "c:" + eurID
	btc  = "a:" + btcID
	eth  = "a:" + ethID
	best = "a:" + bestID
	gold = "a:" + goldID
	aapl = "a:" + aaplEq
)

// scenario wires a consistent account: replaying everything gives the
// portfolio's balances.
func scenario(f *fake) {
	ops := []map[string]any{
		op("op-dep", "deposit", leg("t1", eur, "1000", at(0), nil)),
		// Crypto buy: 100 EUR paid, fee 1.49 EUR inside it.
		op("op-buy", "buy", leg("t2", eur, "-100", at(1), tradeRef("tr-buy", "1.49")), leg("t3", btc, "0.002", at(1), tradeRef("tr-buy", "1.49"))),
		// Crypto sell: 30 EUR received after a 0.45 EUR fee.
		op("op-sell", "sell", leg("t4", btc, "-0.0005", at(2), tradeRef("tr-sell", "0.45")), leg("t5", eur, "30", at(2), tradeRef("tr-sell", "0.45"))),
		// Stock buy: the fee is a leg of its own.
		op("op-stock", "stock_exchange_buy", leg("t6", eur, "-200", at(3), tradeRef("tr-stock", "1")), leg("t7", aapl, "1.5", at(3), tradeRef("tr-stock", "1")),
			leg("t8", eur, "-1", at(3), map[string]any{"transaction_type": "fee"})),
		// Gold, through a savings plan.
		op("op-gold", "savings_plan", leg("t9", eur, "-50", at(4), tradeRef("tr-gold", "0.5")), leg("t10", gold, "0.6", at(4), tradeRef("tr-gold", "0.5"))),
		op("op-ethdep", "deposit", leg("t11", eth, "1", at(5), nil)),
		op("op-ethwd", "withdrawal", leg("t12", eth, "-0.4", at(6), map[string]any{"fee_amount": map[string]any{"value": "0.001", "asset_id": ethID}})),
		// Staking: from the main wallet to the staking one, not an activity.
		op("op-stake", "stake", leg("t13", eth, "-0.5", at(7), nil), leg("t14", eth, "0.5", at(7), map[string]any{"wallet_owner": "staking-service"})),
		op("op-reward", "reward", leg("t15", eth, "0.01", at(8), map[string]any{"wallet_owner": "staking-service"})),
		op("op-best", "best_reward", leg("t16", best, "5", at(9), nil)),
		op("op-card", "card_reserve", leg("t17", eur, "-20", at(10), nil), leg("t18", eur, "20", at(10), map[string]any{"wallet_owner": "card"})),
		op("op-cash", "earn_on_fiat_reward", leg("t19", eur, "1.41", at(11), map[string]any{"fee_amount": map[string]any{"value": "0.28", "currency_id": eurID}})),
		op("op-div", "ca_dividend", leg("t20", eur, "3", at(12), nil), leg("t21", eur, "-0.45", at(12), map[string]any{"transaction_type": "levy"})),
		// A margin loan leg: not the user's money.
		op("op-loan", "margin_trading_open_long", leg("t22", eur, "500", at(13), map[string]any{"wallet_owner": "margin-trading-credit"})),
	}
	// Newest first, and one operation repeated across pages.
	for i := len(ops) - 1; i >= 0; i-- {
		f.ops = append(f.ops, ops[i])
	}
	f.ops = append(f.ops, ops[0])
	f.portfolio = []map[string]any{
		pf(eur, "682.68"), pf(btc, "0.0015"), pf(eth, "0.609"), pf(best, "5"), pf(gold, "0.6"),
		pf("a:"+aaplOld, "1.5"), // the legacy listing of the same ISIN
		pf("a:"+eurotech, "0"),  // nothing held: left out
	}
}

func pf(u, qty string) map[string]any {
	m := map[string]any{"balance": map[string]any{"value": qty}, "available_balance": map[string]any{"value": qty}}
	if strings.HasPrefix(u, "c:") {
		m["currency_id"] = u[2:]
	} else {
		m["asset_id"] = u[2:]
	}
	return m
}

func collect(t *testing.T, s *Source, since time.Time) []model.Activity {
	t.Helper()
	var acts []model.Activity
	err := s.Activities(context.Background(), source.Fetch{Since: since, Emit: func(a model.Activity) error {
		acts = append(acts, a)
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	return acts
}

func byID(acts []model.Activity) map[string]model.Activity {
	m := map[string]model.Activity{}
	for _, a := range acts {
		m[a.NativeID] = a
	}
	return m
}

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestHistoryAndBalances(t *testing.T) {
	f := newFake(t)
	scenario(f)
	s, _ := f.source(t, testKey, nil)
	acts := collect(t, s, time.Time{})
	by := byID(acts)
	if len(by) != len(acts) {
		t.Fatalf("duplicate native ids in %d activities", len(acts))
	}
	if n := len(f.calls["/v1/operations"]); n < 4 {
		t.Fatalf("pagination: %d pages read", n)
	}
	if q := f.calls["/v1/operations"][0]; strings.Contains(q, "from=") {
		t.Fatalf("first sync reads everything: %s", q)
	}

	buy := by["trade:tr-buy"]
	if buy.Type != model.Trade || buy.Side != model.Buy || buy.Base.Asset != (model.Asset{Kind: model.Crypto, Symbol: "BTC", Name: "Bitcoin"}) ||
		!buy.Base.Qty.Equal(d("0.002")) || !buy.Quote.Qty.Equal(d("98.51")) || buy.Quote.Asset.Kind != model.Fiat ||
		buy.Fee == nil || !buy.Fee.Qty.Equal(d("1.49")) || buy.Fee.Asset.Symbol != "EUR" || !buy.Time.Equal(t0.Add(time.Hour)) {
		t.Errorf("crypto buy %+v", buy)
	}
	sell := by["trade:tr-sell"]
	if sell.Side != model.Sell || !sell.Base.Qty.Equal(d("0.0005")) || !sell.Quote.Qty.Equal(d("30.45")) || !sell.Fee.Qty.Equal(d("0.45")) {
		t.Errorf("crypto sell %+v", sell)
	}
	stock := by["trade:tr-stock"]
	if stock.Side != model.Buy || stock.Base.Asset.Kind != model.Security || stock.Base.Asset.ISIN != aaplISIN || stock.Base.Asset.Symbol != "AAPL" ||
		!stock.Quote.Qty.Equal(d("200")) || stock.Fee == nil || !stock.Fee.Qty.Equal(d("1")) {
		t.Errorf("stock buy %+v", stock)
	}
	g := by["trade:tr-gold"]
	if g.Base.Asset.Kind != model.Crypto || g.Base.Asset.Symbol != "XAU" || g.Base.Asset.ISIN != "" || !g.Quote.Qty.Equal(d("49.5")) {
		t.Errorf("gold %+v", g)
	}
	if a := by["op:op-dep:"+eur]; a.Type != model.Deposit || a.Base.Asset.Kind != model.Fiat || !a.Base.Qty.Equal(d("1000")) {
		t.Errorf("fiat deposit %+v", a)
	}
	if a := by["op:op-ethdep:"+eth]; a.Type != model.Deposit || !a.Base.Qty.Equal(d("1")) {
		t.Errorf("crypto deposit %+v", a)
	}
	if a := by["op:op-ethwd:"+eth]; a.Type != model.Withdrawal || !a.Base.Qty.Equal(d("0.4")) || a.Fee == nil || !a.Fee.Qty.Equal(d("0.001")) || a.Fee.Asset.Symbol != "ETH" {
		t.Errorf("crypto withdrawal %+v", a)
	}
	if a := by["op:op-reward:"+eth]; a.Type != model.Reward || !a.Base.Qty.Equal(d("0.01")) {
		t.Errorf("staking reward %+v", a)
	}
	if a := by["op:op-best:"+best]; a.Type != model.Reward || a.Base.Asset.Symbol != "BEST" {
		t.Errorf("BEST reward %+v", a)
	}
	if a := by["op:op-cash:"+eur]; a.Type != model.Reward || !a.Base.Qty.Equal(d("1.41")) || !a.Fee.Qty.Equal(d("0.28")) {
		t.Errorf("Cash Plus reward %+v", a)
	}
	if a := by["op:op-div:"+eur]; a.Type != model.Dividend || !a.Base.Qty.Equal(d("3")) {
		t.Errorf("dividend %+v", a)
	}
	if a := by["tax:t21"]; a.Type != model.Tax || !a.Base.Qty.Equal(d("0.45")) {
		t.Errorf("withholding tax %+v", a)
	}
	for id := range by {
		if strings.Contains(id, "op-stake") || strings.Contains(id, "op-card") || strings.Contains(id, "op-loan") {
			t.Errorf("internal move or loan became an activity: %s", id)
		}
	}
	if len(by) != 12 {
		t.Errorf("%d activities: %v", len(by), by)
	}

	bal, err := s.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(bal) != 6 {
		t.Fatalf("balances %+v", bal)
	}
	if gaps := model.Reconcile(acts, bal, decimal.Zero); len(gaps) != 0 {
		t.Fatalf("history does not add up to the portfolio: %+v", gaps)
	}
	if strings.Contains(string(s.State()), testKey) {
		t.Fatal("the key ended up in the state")
	}
}

func TestIncrementalAndPending(t *testing.T) {
	f := newFake(t)
	scenario(f)
	// A deposit not credited yet.
	pending := op("op-pend", "deposit", leg("t30", eur, "10", "", nil))
	f.ops = append([]map[string]any{pending}, f.ops...)
	s, _ := f.source(t, testKey, nil)
	acts := collect(t, s, time.Time{})
	if _, ok := byID(acts)["op:op-pend:"+eur]; ok {
		t.Fatal("pending operation emitted")
	}
	st := s.State()
	assetCalls := len(f.calls["/v1/assets"])

	// Next run, a month later: the pending one keeps the read at its floor.
	since := t0.Add(30 * 24 * time.Hour)
	f.calls["/v1/operations"] = nil
	pending["transactions"].([]any)[0].(map[string]any)["credited_at"] = at(20)
	s, _ = f.source(t, testKey, st)
	acts = collect(t, s, since)
	if q := f.calls["/v1/operations"][0]; !strings.Contains(q, "from=2014-01-01") {
		t.Fatalf("pending floor not kept: %s", q)
	}
	if a, ok := byID(acts)["op:op-pend:"+eur]; !ok || a.Type != model.Deposit {
		t.Fatalf("booked operation missing: %+v", acts)
	}
	if len(f.calls["/v1/assets"]) != assetCalls {
		t.Error("asset names are cached in the state")
	}

	// Nothing pending any more: back to since minus the overlap.
	f.calls["/v1/operations"] = nil
	s, _ = f.source(t, testKey, s.State())
	acts = collect(t, s, since)
	want := "from=" + strings.ReplaceAll(since.Add(-overlap).Format("2006-01-02T15:04:05.000Z"), ":", "%3A")
	if q := f.calls["/v1/operations"][0]; !strings.Contains(q, want) {
		t.Fatalf("incremental read %s, want %s", q, want)
	}
	if len(acts) != 0 {
		t.Fatalf("nothing new expected, got %+v", acts)
	}
}

func TestRetries(t *testing.T) {
	f := newFake(t)
	scenario(f)
	f.fail["/v1/portfolio"] = []int{http.StatusTooManyRequests, http.StatusServiceUnavailable}
	s, waits := f.source(t, testKey, nil)
	if _, err := s.Balances(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(*waits) != 2 || (*waits)[0] != 2*time.Second {
		t.Fatalf("waits %v", *waits)
	}

	f.fail["/v1/portfolio"] = []int{429, 429, 429, 429, 429, 429, 429}
	if _, err := s.Balances(context.Background()); err == nil || !strings.Contains(err.Error(), "429") {
		t.Fatalf("gives up after retries: %v", err)
	}
}

func TestTester(t *testing.T) {
	f := newFake(t)
	scenario(f)
	s, _ := f.source(t, testKey, nil)
	if err := s.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := len(f.calls["/v1/operations"]); n != 1 || !strings.Contains(f.calls["/v1/operations"][0], "page_size=1") {
		t.Fatalf("test reads one operation: %v", f.calls["/v1/operations"])
	}
	bad, _ := f.source(t, "wrong-key-secret", nil)
	err := bad.Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused") || strings.Contains(err.Error(), "wrong-key-secret") {
		t.Fatalf("bad key: %v", err)
	}
	if !isAuthError(err) {
		t.Error("401 is an auth error")
	}
	if _, err := New(source.Config{Values: map[string]string{}}); err == nil {
		t.Error("empty key accepted")
	}
}

func TestNoRedirect(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		leaked = r.Header.Get("x-api-key") != ""
	}))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v1/portfolio", http.StatusFound)
	}))
	defer srv.Close()
	s, err := New(source.Config{Values: map[string]string{"api_key": testKey, "base_url": srv.URL}, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Balances(context.Background()); err == nil || leaked {
		t.Fatalf("redirect followed (leaked %v): %v", leaked, err)
	}
}

func TestMapEdgeCases(t *testing.T) {
	names := map[unit]model.Asset{
		eur: {Kind: model.Fiat, Symbol: "EUR"}, btc: {Kind: model.Crypto, Symbol: "BTC"},
		best: {Kind: model.Crypto, Symbol: "BEST"}, eth: {Kind: model.Crypto, Symbol: "ETH"},
	}
	asset := func(u unit) model.Asset { return names[u] }
	decode := func(m map[string]any) operation {
		b, _ := json.Marshal(m)
		var o operation
		if err := json.Unmarshal(b, &o); err != nil {
			t.Fatal(err)
		}
		return o
	}

	// Fee paid in BEST, in a leg of its own: the trade keeps its full amount.
	o := decode(op("o1", "buy", leg("a", eur, "-50", at(0), tradeRef("x", "0.37")), leg("b", btc, "0.001", at(0), tradeRef("x", "0.37")),
		leg("c", best, "-1.2", at(0), map[string]any{"transaction_type": "fee"})))
	acts, ok := mapOperation(o, asset)
	if !ok || len(acts) != 1 || !acts[0].Quote.Qty.Equal(d("50")) || acts[0].Fee.Asset.Symbol != "BEST" || !acts[0].Fee.Qty.Equal(d("1.2")) {
		t.Fatalf("BEST fee %+v", acts)
	}

	// A crypto-to-crypto swap without a trade object: one in, one out.
	o = decode(op("o2", "merger_crypto", leg("a", btc, "-1", at(0), nil), leg("b", eth, "20", at(0), nil)))
	acts, _ = mapOperation(o, asset)
	if len(acts) != 1 || acts[0].Type != model.Trade || acts[0].Base.Asset.Symbol != "ETH" || acts[0].Quote.Asset.Symbol != "BTC" {
		t.Fatalf("merger %+v", acts)
	}

	// A swap chained through EUR: two trades, the EUR cancels out.
	o = decode(op("o3", "swap", leg("a", btc, "-0.01", at(0), tradeRef("s1", "0")), leg("b", eur, "600", at(0), tradeRef("s1", "0")),
		leg("c", eur, "-600", at(0), tradeRef("s2", "0")), leg("d", eth, "0.3", at(0), tradeRef("s2", "0"))))
	acts, _ = mapOperation(o, asset)
	if len(acts) != 2 || acts[0].Side != model.Sell || acts[1].Side != model.Buy || acts[0].Fee != nil {
		t.Fatalf("swap %+v", acts)
	}

	// An unknown type is imported by direction.
	o = decode(op("o4", "something_new", leg("a", eth, "2", at(0), nil)))
	acts, _ = mapOperation(o, asset)
	if len(acts) != 1 || acts[0].Type != model.Deposit || knownType("something_new") {
		t.Fatalf("unknown %+v", acts)
	}

	// An unknown UUID still gets a stable asset of its own.
	s := &Source{st: state{Assets: map[string]assetMeta{}}}
	if a := s.asset("a:dead-beef"); a.Kind != model.Crypto || a.Symbol != "BITPANDA:DEAD-BEEF" {
		t.Fatalf("unknown asset %+v", a)
	}
}
