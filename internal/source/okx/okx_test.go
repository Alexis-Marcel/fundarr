package okx

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

const (
	testKey    = "key-1"
	testSecret = "secret-1"
	testPass   = "pass-1"
)

// fake is an OKX server serving documented response shapes.
type fake struct {
	t      *testing.T
	srv    *httptest.Server
	mu     sync.Mutex
	routes map[string]func(r *http.Request) any // returns the "data" value, or *apiErr
	calls  map[string][]*http.Request
	files  map[string][]byte
}

type apiErr struct{ code, msg string }

func newFake(t *testing.T) *fake {
	f := &fake{t: t, routes: map[string]func(*http.Request) any{}, calls: map[string][]*http.Request{}, files: map[string][]byte{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/files/") {
		b, ok := f.files[strings.TrimPrefix(r.URL.Path, "/files/")]
		if !ok || r.Header.Get("OK-ACCESS-KEY") != "" { // pre-signed link: no OKX headers
			http.NotFound(w, r)
			return
		}
		w.Write(b)
		return
	}
	body, _ := io.ReadAll(r.Body)
	ts := r.Header.Get("OK-ACCESS-TIMESTAMP")
	want := sign(testSecret, ts, r.Method, r.URL.RequestURI(), string(body))
	if r.Header.Get("OK-ACCESS-SIGN") != want || r.Header.Get("OK-ACCESS-KEY") != testKey || r.Header.Get("OK-ACCESS-PASSPHRASE") != testPass {
		json.NewEncoder(w).Encode(map[string]any{"code": "50113", "msg": "Invalid Sign", "data": []any{}})
		return
	}
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", ts); err != nil {
		f.t.Errorf("bad timestamp %q", ts)
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	f.mu.Lock()
	f.calls[r.Method+" "+r.URL.Path] = append(f.calls[r.Method+" "+r.URL.Path], r)
	h := f.routes[r.Method+" "+r.URL.Path]
	f.mu.Unlock()
	if h == nil {
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "msg": "", "data": []any{}})
		return
	}
	switch v := h(r).(type) {
	case *apiErr:
		json.NewEncoder(w).Encode(map[string]any{"code": v.code, "msg": v.msg, "data": []any{}})
	case int: // raw HTTP status
		w.WriteHeader(v)
		w.Write([]byte("<html>busy</html>"))
	default:
		json.NewEncoder(w).Encode(map[string]any{"code": "0", "msg": "", "data": v})
	}
}

func (f *fake) on(method, path string, h func(r *http.Request) any) { f.routes[method+" "+path] = h }

func (f *fake) source(t *testing.T, state []byte, now time.Time) *Source {
	s, err := New(source.Config{Values: map[string]string{
		"api_key": testKey, "api_secret": testSecret, "passphrase": testPass, "base_url": f.srv.URL,
	}, HTTP: f.srv.Client(), State: state})
	if err != nil {
		t.Fatal(err)
	}
	src := s.(*Source)
	src.c.now = func() time.Time { return now }
	src.c.sleep = func(context.Context, time.Duration) error { return nil }
	return src
}

func ms(t time.Time) string { return fmt.Sprint(t.UnixMilli()) }

var t0 = time.Date(2024, 4, 10, 12, 0, 0, 0, time.UTC)

func at(h int) string { return ms(t0.Add(time.Duration(h) * time.Hour)) }

// scenario wires a consistent account: replaying everything must give the balances.
func scenario(f *fake) {
	f.on("GET", "/api/v5/account/subtypes", func(*http.Request) any {
		return []any{
			map[string]any{"type": "1", "typeDesc": "Transfer", "subTypeDetails": []any{
				map[string]any{"subType": "11", "subTypeDesc": "Transfer in"}, map[string]any{"subType": "12", "subTypeDesc": "Transfer out"}}},
			map[string]any{"type": "2", "typeDesc": "Trade", "subTypeDetails": []any{
				map[string]any{"subType": "1", "subTypeDesc": "Buy"}, map[string]any{"subType": "2", "subTypeDesc": "Sell"}}},
			map[string]any{"type": "7", "typeDesc": "Interest deduction", "subTypeDetails": []any{
				map[string]any{"subType": "9", "subTypeDesc": "Interest deduction for Market loans"}}},
			map[string]any{"type": "30", "typeDesc": "Convert", "subTypeDetails": []any{
				map[string]any{"subType": "320", "subTypeDesc": "Simple buy"}, map[string]any{"subType": "321", "subTypeDesc": "Simple sell"}}},
		}
	})

	// Deposits: page 1 has 99 pending records and one credited; page 2 one more.
	f.on("GET", "/api/v5/asset/deposit-history", func(r *http.Request) any {
		if r.URL.Query().Get("after") == "" {
			page := []any{map[string]any{"ccy": "USDT", "chain": "USDT-TRC20", "amt": "1500", "depId": "d1", "state": "2",
				"ts": at(0), "txId": "0xaaa", "to": "TN4h", "from": ""}}
			for i := 0; i < 99; i++ {
				page = append(page, map[string]any{"ccy": "USDT", "amt": "1", "depId": fmt.Sprintf("p%d", i), "state": "0", "ts": at(0)})
			}
			return page
		}
		if got := r.URL.Query().Get("after"); got != fmt.Sprint(t0.UnixMilli()+1) {
			f.t.Errorf("deposit page 2 after=%s", got)
		}
		return []any{map[string]any{"ccy": "USDT", "amt": "500", "depId": "d2", "state": "1", "ts": at(-1), "from": "156****359"}}
	})
	f.on("GET", "/api/v5/asset/withdrawal-history", func(*http.Request) any {
		return []any{
			map[string]any{"ccy": "BTC", "chain": "BTC-Bitcoin", "amt": "0.01", "fee": "0.0002", "feeCcy": "BTC", "to": "bc1q",
				"txId": "0xbbb", "state": "2", "ts": at(30), "wdId": "w1"},
			map[string]any{"ccy": "BTC", "amt": "1", "fee": "0.0002", "state": "-2", "ts": at(29), "wdId": "w0"},
		}
	})

	// Funding ledger: page 1 = 100 bills (skipped transfers as filler + real ones), page 2 = the rest.
	f.on("GET", "/api/v5/asset/bills-history", func(r *http.Request) any {
		if r.URL.Query().Get("pagingType") != "2" {
			f.t.Errorf("funding bills must page by bill id")
		}
		bill := func(id, ccy, chg, typ, ts string) any {
			return map[string]any{"billId": id, "ccy": ccy, "balChg": chg, "bal": "0", "type": typ, "ts": ts, "notes": "", "clientId": ""}
		}
		if r.URL.Query().Get("after") == "" {
			page := []any{
				bill("f90", "ETH", "0.001", "83", at(40)),  // staking yield
				bill("f89", "OKB", "5", "229", at(39)),     // airdrop
				bill("f88", "USDT", "-10", "273", at(38)),  // convert: sell USDT...
				bill("f87", "ADA", "20", "272", at(38)),    // ...buy ADA
				bill("f86", "XRP", "1", "999", at(37)),     // unknown type: plain deposit
				bill("f85", "USDT", "-100", "75", at(36)),  // Simple Earn subscription: internal
				bill("f84", "USDT", "1500", "1", at(0)),    // deposit: from deposit-history
				bill("f83", "BTC", "-0.0102", "2", at(30)), // withdrawal: from withdrawal-history
				bill("f82", "EUR", "2000", "1", at(2)),     // bank deposit: from fiat history
				bill("f81", "EUR", "-300", "116", at(3)),   // buy/sell order: from buy/sell history
				bill("f80", "BTC", "0.005", "117", at(3)),  //
			}
			for i := len(page); i < 100; i++ {
				page = append(page, bill(fmt.Sprintf("x%d", i), "USDT", "-1", "131", at(1)))
			}
			return page
		}
		if r.URL.Query().Get("after") != "x99" {
			f.t.Errorf("funding page 2 after=%s", r.URL.Query().Get("after"))
		}
		return []any{bill("f01", "USDT", "-1900", "131", at(1))}
	})

	// Trading ledger (last 3 months).
	f.on("GET", "/api/v5/account/bills-archive", func(r *http.Request) any {
		tb := func(id, sub, ccy, chg, fee, inst, trade, ts string) any {
			return map[string]any{"billId": id, "type": map[bool]string{true: "2", false: ""}[sub == "1" || sub == "2"],
				"subType": sub, "ccy": ccy, "balChg": chg, "fee": fee, "instId": inst, "instType": "SPOT", "tradeId": trade,
				"ordId": "o" + trade, "ts": ts, "bal": "0", "pnl": "0", "sz": "0", "px": "0"}
		}
		return []any{
			tb("t9", "9", "USDT", "-0.5", "0", "", "", at(25)),                 // interest deduction → fee
			tb("t8", "321", "USDT", "-50", "0", "", "", at(24)),                // convert sell USDT
			tb("t7", "320", "SOL", "0.5", "0", "", "", at(24)),                 // convert buy SOL
			tb("t6", "1", "USDT", "-299.97", "0.03", "ETH-USDT", "3", at(22)),  // maker rebate
			tb("t5", "1", "ETH", "0.1", "0", "ETH-USDT", "3", at(22)),          //
			tb("t4", "2", "USDT", "159.84", "-0.16", "BTC-USDT", "2", at(21)),  // sell, fee in USDT
			tb("t3", "2", "BTC", "-0.005", "0", "BTC-USDT", "2", at(21)),       //
			tb("t2", "1", "BTC", "0.0199", "-0.0001", "BTC-USDT", "1", at(20)), // buy, fee in BTC
			tb("t1", "1", "USDT", "-600", "0", "BTC-USDT", "1", at(20)),        //
			tb("t0", "11", "USDT", "1900", "0", "", "", at(1)),                 // transfer in: internal
		}
	})

	// Bank movements and buy/sell orders.
	f.on("GET", "/api/v5/fiat/deposit-order-history", func(r *http.Request) any {
		if r.URL.Query().Get("before") == "" {
			page := []any{map[string]any{"ordId": "fd1", "ccy": "EUR", "amt": "2000", "fee": "0", "paymentMethod": "SEPA",
				"paymentAcctId": "1", "state": "completed", "cTime": at(2), "uTime": at(2), "clientId": ""}}
			for i := 0; i < 99; i++ {
				page = append(page, map[string]any{"ordId": fmt.Sprintf("fp%d", i), "ccy": "EUR", "amt": "5", "state": "pending", "cTime": at(2)})
			}
			return page
		}
		if got := r.URL.Query().Get("before"); got != fmt.Sprint(t0.Add(2*time.Hour).UnixMilli()-1) {
			f.t.Errorf("fiat deposit page 2 before=%s", got)
		}
		return []any{}
	})
	f.on("GET", "/api/v5/fiat/withdrawal-order-history", func(*http.Request) any {
		return []any{
			map[string]any{"ordId": "fw1", "ccy": "EUR", "amt": "100", "fee": "1", "paymentMethod": "SEPA", "state": "completed", "cTime": at(50), "uTime": "0"},
			map[string]any{"ordId": "fw2", "ccy": "EUR", "amt": "100", "fee": "1", "paymentMethod": "SEPA", "state": "failed", "cTime": at(51)},
		}
	})
	f.on("GET", "/api/v5/fiat/buy-sell/history", func(*http.Request) any {
		return []any{
			map[string]any{"ordId": "bs1", "state": "completed", "side": "buy", "fromCcy": "EUR", "toCcy": "BTC", "rfqAmt": "300",
				"rfqCcy": "EUR", "fillPx": "60000", "fillQuoteCcy": "EUR", "fillFromAmt": "300", "fillToAmt": "0.005", "cTime": at(3), "uTime": at(3)},
			map[string]any{"ordId": "bs2", "state": "failed", "fromCcy": "EUR", "toCcy": "BTC", "fillFromAmt": "0", "fillToAmt": "0", "cTime": at(4)},
		}
	})

	f.on("GET", "/api/v5/finance/savings/lending-history", func(*http.Request) any {
		return []any{map[string]any{"ccy": "USDT", "amt": "100", "earnings": "0.05", "rate": "0.01", "ts": at(41)}}
	})

	// Archives: 2024Q1 is ready right away, the others are being prepared.
	csvBody := "billId,subType,ccy,balChg,fee,instId,instType,tradeId,ordId,ts\n" +
		"a2,1,DOT,10,0,DOT-USDT,SPOT,9,o9," + ms(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)) + "\n" +
		"a1,1,USDT,-70,0,DOT-USDT,SPOT,9,o9," + ms(time.Date(2024, 2, 1, 0, 0, 0, 0, time.UTC)) + "\n"
	var zbuf bytes.Buffer
	zw := zip.NewWriter(&zbuf)
	w, _ := zw.Create("bills.csv")
	w.Write([]byte("\ufeff" + csvBody))
	zw.Close()
	f.files["2024Q1.zip"] = zbuf.Bytes()
	f.on("POST", "/api/v5/account/bills-history-archive", func(r *http.Request) any {
		var body map[string]string
		json.NewDecoder(r.Body).Decode(&body)
		ready := body["year"] == "2024" && body["quarter"] == "Q1"
		return []any{map[string]any{"result": fmt.Sprint(ready), "ts": at(0)}}
	})
	f.on("GET", "/api/v5/account/bills-history-archive", func(r *http.Request) any {
		q := r.URL.Query()
		if q.Get("year") == "2024" && q.Get("quarter") == "Q1" {
			return []any{map[string]any{"fileHref": f.srv.URL + "/files/2024Q1.zip", "state": "finished", "ts": at(0)}}
		}
		if q.Get("year") == "2021" && q.Get("quarter") == "Q1" {
			return []any{map[string]any{"fileHref": "", "state": "failed", "ts": at(0)}}
		}
		return []any{map[string]any{"fileHref": "", "state": "ongoing", "ts": at(0)}}
	})

	// Balances across the three accounts.
	f.on("GET", "/api/v5/account/balance", func(*http.Request) any {
		return []any{map[string]any{"totalEq": "1", "details": []any{
			map[string]any{"ccy": "USDT", "cashBal": "1019.37", "eq": "1019.37"},
			map[string]any{"ccy": "BTC", "cashBal": "0.0047", "eq": "0.0047"},
			map[string]any{"ccy": "ETH", "cashBal": "0.1", "eq": "0.1"},
			map[string]any{"ccy": "SOL", "cashBal": "0.5", "eq": "0.5"},
			map[string]any{"ccy": "DOT", "cashBal": "10", "eq": "10"},
			map[string]any{"ccy": "LTC", "cashBal": "0", "eq": "0"},
		}}}
	})
	f.on("GET", "/api/v5/asset/balances", func(*http.Request) any {
		return []any{
			map[string]any{"ccy": "USDT", "bal": "10", "availBal": "10", "frozenBal": "0"},
			map[string]any{"ccy": "ETH", "bal": "0.001", "availBal": "0.001", "frozenBal": "0"},
			map[string]any{"ccy": "ADA", "bal": "20", "availBal": "20", "frozenBal": "0"},
			map[string]any{"ccy": "OKB", "bal": "5", "availBal": "5", "frozenBal": "0"},
			map[string]any{"ccy": "XRP", "bal": "1", "availBal": "1", "frozenBal": "0"},
			map[string]any{"ccy": "EUR", "bal": "1599", "availBal": "1599", "frozenBal": "0"},
			map[string]any{"ccy": "BTC", "bal": "0.005", "availBal": "0.005", "frozenBal": "0"},
		}
	})
	f.on("GET", "/api/v5/finance/savings/balance", func(*http.Request) any {
		return []any{map[string]any{"ccy": "USDT", "amt": "100.05", "earnings": "0.05", "loanAmt": "100", "pendingAmt": "0.05", "rate": "0.01", "redemptAmt": ""}}
	})
}

func collect(t *testing.T, s *Source, since time.Time) (map[string]model.Activity, []string) {
	t.Helper()
	got := map[string]model.Activity{}
	var logs []string
	err := s.Activities(context.Background(), source.Fetch{
		Since: since,
		Emit: func(a model.Activity) error {
			if _, dup := got[a.NativeID]; dup {
				t.Errorf("duplicate native id %s", a.NativeID)
			}
			if a.Time.IsZero() || a.Time.Location() != time.UTC {
				t.Errorf("%s: time %v", a.NativeID, a.Time)
			}
			got[a.NativeID] = a
			return nil
		},
		Progress: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return got, logs
}

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestFullHistoryReconciles(t *testing.T) {
	f := newFake(t)
	scenario(f)
	now := time.Date(2024, 5, 15, 0, 0, 0, 0, time.UTC)
	s := f.source(t, nil, now)

	acts, logs := collect(t, s, time.Time{})

	check := func(id string, typ model.Type, side model.Side, base, qty string) model.Activity {
		t.Helper()
		a, ok := acts[id]
		if !ok {
			t.Fatalf("missing %s (have %v)", id, keys(acts))
		}
		if a.Type != typ || a.Side != side || a.Base.Asset.Symbol != base || !a.Base.Qty.Equal(d(qty)) {
			t.Errorf("%s = %s %s %s %s, want %s %s %s %s", id, a.Type, a.Side, a.Base.Qty, a.Base.Asset.Symbol, typ, side, qty, base)
		}
		return a
	}
	dep := check("dep:d1", model.Deposit, "", "USDT", "1500")
	if dep.Network != "USDT-TRC20" || dep.TxID != "0xaaa" || dep.Address != "TN4h" {
		t.Errorf("deposit details %+v", dep)
	}
	if a := check("dep:d2", model.Deposit, "", "USDT", "500"); !strings.Contains(a.Note, "internal") {
		t.Errorf("internal transfer note %q", a.Note)
	}
	wd := check("wd:w1", model.Withdrawal, "", "BTC", "0.01")
	if wd.Fee == nil || !wd.Fee.Qty.Equal(d("0.0002")) || wd.Fee.Asset.Symbol != "BTC" || wd.TxID != "0xbbb" {
		t.Errorf("withdrawal fee %+v", wd.Fee)
	}
	if _, ok := acts["wd:w0"]; ok {
		t.Error("canceled withdrawal must be ignored")
	}

	buy := check("trade:BTC-USDT:1", model.Trade, model.Buy, "BTC", "0.02")
	if !buy.Quote.Qty.Equal(d("600")) || buy.Quote.Asset.Symbol != "USDT" || buy.Fee == nil || !buy.Fee.Qty.Equal(d("0.0001")) || buy.Fee.Asset.Symbol != "BTC" {
		t.Errorf("buy %+v fee %+v", buy.Quote, buy.Fee)
	}
	if !buy.Price().Equal(d("30000")) {
		t.Errorf("price %s", buy.Price())
	}
	sell := check("trade:BTC-USDT:2", model.Trade, model.Sell, "BTC", "0.005")
	if !sell.Quote.Qty.Equal(d("160")) || !sell.Fee.Qty.Equal(d("0.16")) || sell.Fee.Asset.Symbol != "USDT" {
		t.Errorf("sell %+v fee %+v", sell.Quote, sell.Fee)
	}
	eth := check("trade:ETH-USDT:3", model.Trade, model.Buy, "ETH", "0.1")
	if !eth.Quote.Qty.Equal(d("300")) || eth.Fee != nil {
		t.Errorf("eth %+v", eth)
	}
	check("rebate:t6", model.Reward, "", "USDT", "0.03")

	conv := check("pair:tbill:t7+tbill:t8", model.Trade, model.Buy, "SOL", "0.5")
	if !conv.Quote.Qty.Equal(d("50")) || conv.Quote.Asset.Symbol != "USDT" {
		t.Errorf("convert %+v", conv.Quote)
	}
	if a := check("tbill:t9", model.Fee, "", "USDT", "0.5"); !strings.Contains(a.Note, "Interest deduction") {
		t.Errorf("note %q", a.Note)
	}
	check("fbill:f90", model.Reward, "", "ETH", "0.001")
	check("fbill:f89", model.Reward, "", "OKB", "5")
	check("pair:fbill:f87+fbill:f88", model.Trade, model.Buy, "ADA", "20")
	check("fbill:f86", model.Deposit, "", "XRP", "1")
	check("earn:USDT:"+at(41), model.Reward, "", "USDT", "0.05")
	dot := check("trade:DOT-USDT:9", model.Trade, model.Buy, "DOT", "10")
	if !dot.Quote.Qty.Equal(d("70")) {
		t.Errorf("archived trade %+v", dot.Quote)
	}
	for id := range acts {
		if strings.HasPrefix(id, "fbill:x") || id == "fbill:f85" || id == "fbill:f84" || id == "fbill:f83" || id == "tbill:t0" || id == "fbill:f01" {
			t.Errorf("internal or duplicated movement emitted: %s", id)
		}
	}
	sepa := check("fiatdep:fd1", model.Deposit, "", "EUR", "2000")
	if sepa.Base.Asset.Kind != model.Fiat || sepa.Fee != nil || !strings.Contains(sepa.Note, "SEPA") {
		t.Errorf("bank deposit %+v", sepa)
	}
	if w := check("fiatwd:fw1", model.Withdrawal, "", "EUR", "100"); w.Fee == nil || !w.Fee.Qty.Equal(d("1")) || !w.Time.Equal(t0.Add(50*time.Hour)) {
		t.Errorf("bank withdrawal %+v", w)
	}
	bs := check("buysell:bs1", model.Trade, model.Buy, "BTC", "0.005")
	if !bs.Quote.Qty.Equal(d("300")) || bs.Quote.Asset.Symbol != "EUR" {
		t.Errorf("buy/sell %+v", bs.Quote)
	}
	if len(acts) != 18 {
		t.Errorf("got %d activities: %v", len(acts), keys(acts))
	}

	bal, err := s.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bal {
		if b.Asset.Symbol == "LTC" {
			t.Error("zero balances must be dropped")
		}
	}
	list := make([]model.Activity, 0, len(acts))
	for _, a := range acts {
		list = append(list, a)
	}
	if gaps := model.Reconcile(list, bal, decimal.Zero); len(gaps) != 0 {
		t.Fatalf("history does not add up to balances: %+v", gaps)
	}

	// Archive bookkeeping: 2024Q1 imported, 2021Q1 failed (asked again later),
	// the rest requested; applies were paced.
	var st state
	json.Unmarshal(s.State(), &st)
	if st.Archive["2024Q1"] != "done" || st.Archive["2023Q4"] != "applied" || len(st.Archive) != 13 {
		t.Errorf("archive state %v", st.Archive)
	}
	if n := len(f.calls["POST /api/v5/account/bills-history-archive"]); n != 13 {
		t.Errorf("%d archive requests", n)
	}
	if !contains(logs, "2024Q1: 2 ledger lines") || !contains(logs, "requested") {
		t.Errorf("logs %v", logs)
	}
	if q := f.calls["GET /api/v5/account/bills-archive"][0].URL.Query(); q.Get("begin") != "" {
		t.Error("first run reads the whole 3 months")
	}

	// Next run: only applied quarters are polled; 2021Q1 failed and is requested again.
	f.calls = map[string][]*http.Request{}
	s2 := f.source(t, s.State(), now)
	acts2, _ := collect(t, s2, now.Add(-time.Hour))
	if n := len(f.calls["POST /api/v5/account/bills-history-archive"]); n != 0 {
		t.Errorf("no new applies expected on the polling run, got %d", n)
	}
	st = state{}
	json.Unmarshal(s2.State(), &st)
	if _, ok := st.Archive["2021Q1"]; ok || st.Archive["2024Q1"] != "done" {
		t.Errorf("state after poll %v", st.Archive)
	}
	if q := f.calls["GET /api/v5/account/bills-archive"][0].URL.Query(); q.Get("begin") != ms(now.Add(-time.Hour-overlap)) {
		t.Errorf("incremental begin = %s", q.Get("begin"))
	}
	// Records older than the cutoff are left out.
	if _, ok := acts2["dep:d1"]; ok {
		t.Error("old deposit re-emitted on an incremental run")
	}
}

func TestFiatUnavailable(t *testing.T) {
	f := newFake(t)
	scenario(f)
	for _, p := range []string{"/api/v5/fiat/deposit-order-history", "/api/v5/fiat/withdrawal-order-history", "/api/v5/fiat/buy-sell/history"} {
		f.on("GET", p, func(*http.Request) any { return &apiErr{"50030", "No permission to use this API"} })
	}
	s := f.source(t, nil, time.Date(2024, 5, 15, 0, 0, 0, 0, time.UTC))
	acts, logs := collect(t, s, time.Time{})
	if !contains(logs, "fiat deposit history not available") || !contains(logs, "buy/sell history not available") {
		t.Errorf("logs %v", logs)
	}
	// Without the buy/sell history, its funding bills are kept (as an exchange).
	if a, ok := acts["pair:fbill:f80+fbill:f81"]; !ok || a.Type != model.Trade || a.Base.Asset.Symbol != "BTC" {
		t.Errorf("buy/sell bills not kept: %v", keys(acts))
	}
	if _, ok := acts["fiatdep:fd1"]; ok {
		t.Error("unavailable history emitted")
	}
	// Network errors are not mistaken for "unavailable".
	g := newFake(t)
	scenario(g)
	g.on("GET", "/api/v5/fiat/deposit-order-history", func(*http.Request) any { return http.StatusBadGateway })
	src := g.source(t, nil, time.Date(2021, 2, 1, 0, 0, 0, 0, time.UTC))
	if err := src.Activities(context.Background(), source.Fetch{Emit: func(model.Activity) error { return nil }}); err == nil {
		t.Error("HTTP error swallowed")
	}
}

func TestTestRejectsTradingKeys(t *testing.T) {
	f := newFake(t)
	perm := "read_only"
	calls := 0
	f.on("GET", "/api/v5/account/config", func(*http.Request) any {
		calls++
		if calls == 1 {
			return &apiErr{"50011", "Rate limit reached"}
		}
		if calls == 2 {
			return http.StatusTooManyRequests
		}
		return []any{map[string]any{"uid": "1", "perm": perm}}
	})
	s := f.source(t, nil, time.Now())
	if err := s.Test(context.Background()); err != nil {
		t.Fatalf("read-only key after rate limit: %v", err)
	}
	perm = "read_only,trade"
	if err := s.Test(context.Background()); err == nil || !strings.Contains(err.Error(), "only the Read permission") {
		t.Fatalf("trade key accepted: %v", err)
	}
	perm = "read_only,withdraw"
	if err := s.Test(context.Background()); err == nil {
		t.Fatal("withdraw key accepted")
	}
}

func TestErrors(t *testing.T) {
	f := newFake(t)
	f.on("GET", "/api/v5/account/config", func(*http.Request) any { return &apiErr{"50111", "Invalid OK-ACCESS-KEY"} })
	s := f.source(t, nil, time.Now())
	err := s.Test(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "50111" || !strings.Contains(err.Error(), "Invalid OK-ACCESS-KEY") {
		t.Fatalf("api error %v", err)
	}

	// Wrong secret: the server rejects the signature.
	bad, _ := New(source.Config{Values: map[string]string{"api_key": testKey, "api_secret": "nope", "passphrase": testPass, "base_url": f.srv.URL}})
	if err := bad.(*Source).Test(context.Background()); err == nil || !strings.Contains(err.Error(), "50113") {
		t.Fatalf("bad signature accepted: %v", err)
	}

	// Rate limit that never ends gives up after a few attempts.
	f.on("GET", "/api/v5/asset/balances", func(*http.Request) any { return &apiErr{"50011", "Rate limit reached"} })
	if _, err := s.Balances(context.Background()); err == nil {
		t.Fatal("endless rate limit must fail")
	}
	f.on("GET", "/api/v5/asset/balances", func(*http.Request) any { return http.StatusBadGateway })
	if _, err := s.Balances(context.Background()); err == nil || !strings.Contains(err.Error(), "502") {
		t.Fatalf("html error page: %v", err)
	}

	// Errors in any history step stop the run.
	for _, p := range []string{"/api/v5/asset/deposit-history", "/api/v5/asset/withdrawal-history", "/api/v5/asset/bills-history",
		"/api/v5/account/bills-archive", "/api/v5/finance/savings/lending-history"} {
		g := newFake(t)
		scenario(g)
		g.on("GET", p, func(*http.Request) any { return &apiErr{"50001", "Service temporarily unavailable"} })
		src := g.source(t, []byte(`{"archive":{}}`), time.Date(2021, 2, 1, 0, 0, 0, 0, time.UTC))
		if err := src.Activities(context.Background(), source.Fetch{Emit: func(model.Activity) error { return nil }}); err == nil {
			t.Errorf("%s error swallowed", p)
		}
	}
	// Emit errors propagate.
	g := newFake(t)
	scenario(g)
	src := g.source(t, nil, time.Date(2021, 2, 1, 0, 0, 0, 0, time.UTC))
	boom := errors.New("boom")
	if err := src.Activities(context.Background(), source.Fetch{Emit: func(model.Activity) error { return boom }}); !errors.Is(err, boom) {
		t.Fatalf("emit error %v", err)
	}
	// Bill types failing is not fatal.
	g.on("GET", "/api/v5/account/subtypes", func(*http.Request) any { return &apiErr{"50001", "down"} })
	var logs []string
	if err := src.Activities(context.Background(), source.Fetch{Emit: func(model.Activity) error { return nil },
		Progress: func(format string, a ...any) { logs = append(logs, fmt.Sprintf(format, a...)) }}); err != nil || !contains(logs, "built-in rules") {
		t.Fatalf("subtypes failure: %v %v", err, logs)
	}

	if _, err := New(source.Config{Values: map[string]string{"api_key": "k"}}); err == nil {
		t.Fatal("missing secret accepted")
	}
	if _, err := New(source.Config{Values: map[string]string{"api_key": "k", "api_secret": "s", "passphrase": "p"}, State: []byte("{")}); err == nil {
		t.Fatal("bad state accepted")
	}
	s3, _ := New(source.Config{Values: map[string]string{"api_key": "k", "api_secret": "s", "passphrase": "p", "base_url": "my.okx.com/"}, State: []byte(`{}`)})
	if s3.(*Source).c.base != "https://my.okx.com" || s3.(*Source).st.Archive == nil {
		t.Fatalf("base %q", s3.(*Source).c.base)
	}
	s4, _ := New(source.Config{Values: map[string]string{"api_key": "k", "api_secret": "s", "passphrase": "p"}})
	if s4.(*Source).c.base != defaultBase {
		t.Fatal("default base")
	}
}

func TestArchiveFormatsAndQuarters(t *testing.T) {
	body := "billId,subType,ccy,balChg,ts\nb1,1,BTC,1," + at(0) + "\n"
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte(body))
	zw.Close()
	for name, raw := range map[string][]byte{"plain": []byte(body), "gzip": gz.Bytes()} {
		b, err := parseArchive(raw)
		if err != nil || len(b) != 1 || b[0].BillID != "b1" || b[0].Ccy != "BTC" {
			t.Errorf("%s: %+v %v", name, b, err)
		}
	}
	if _, err := parseArchive([]byte("billId,ccy\nx,BTC\n")); err == nil {
		t.Error("missing columns must fail")
	}
	if b, err := parseArchive(nil); err != nil || b != nil {
		t.Error("empty file")
	}
	if _, err := parseArchive([]byte{0x1f, 0x8b, 0}); err == nil {
		t.Error("broken gzip")
	}
	if _, err := parseArchive([]byte("PK\x03\x04broken")); err == nil {
		t.Error("broken zip")
	}

	q := quarters(time.Date(2021, 8, 1, 0, 0, 0, 0, time.UTC))
	if strings.Join(q, ",") != "2021Q1,2021Q2" {
		t.Errorf("quarters %v", q)
	}
	if len(quarters(time.Date(2021, 1, 5, 0, 0, 0, 0, time.UTC))) != 0 {
		t.Error("no archive before 2021Q2")
	}

	// A download error is reported.
	f := newFake(t)
	f.on("POST", "/api/v5/account/bills-history-archive", func(*http.Request) any { return []any{map[string]any{"result": "true"}} })
	f.on("GET", "/api/v5/account/bills-history-archive", func(*http.Request) any {
		return []any{map[string]any{"fileHref": f.srv.URL + "/files/missing", "state": "finished"}}
	})
	s := f.source(t, nil, time.Date(2021, 4, 2, 0, 0, 0, 0, time.UTC))
	if err := s.archives(context.Background(), source.Fetch{}, nil, func(model.Activity) error { return nil }); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("download error %v", err)
	}
}

func TestTradeEdgeCases(t *testing.T) {
	var got []model.Activity
	emit := func(a model.Activity) error { got = append(got, a); return nil }
	ts := at(0)
	bills := []tradingBill{
		// Only one side seen: kept as a plain ledger line, not lost.
		{BillID: "s1", SubType: "1", Ccy: "BTC", BalChg: "0.1", InstID: "BTC-USDT", InstType: "SPOT", TradeID: "77", Ts: ts},
		// Fees charged in two currencies.
		{BillID: "m1", SubType: "1", Ccy: "ETH", BalChg: "0.99", Fee: "-0.01", InstID: "ETH-BTC", InstType: "SPOT", TradeID: "78", Ts: ts},
		{BillID: "m2", SubType: "1", Ccy: "BTC", BalChg: "-0.051", Fee: "-0.001", InstID: "ETH-BTC", InstType: "SPOT", TradeID: "78", Ts: ts},
		// Weird instrument id.
		{BillID: "w1", SubType: "1", Ccy: "BTC", BalChg: "-1", InstID: "BTCUSDT", InstType: "SPOT", TradeID: "79", Ts: at(1)},
		// Derivatives are not spot trades.
		{BillID: "p1", SubType: "1", Ccy: "USDT", BalChg: "-3", InstID: "BTC-USDT-SWAP", InstType: "SWAP", TradeID: "80", Ts: at(2)},
	}
	if err := mapTradingBills(bills, map[string]billType{}, emit); err != nil {
		t.Fatal(err)
	}
	byID := map[string]model.Activity{}
	for _, a := range got {
		byID[a.NativeID] = a
	}
	if a := byID["tbill:s1"]; a.Type != model.Deposit || !a.Base.Qty.Equal(d("0.1")) {
		t.Errorf("one-sided fill %+v", a)
	}
	m := byID["trade:ETH-BTC:78"]
	if m.Type != model.Trade || !m.Base.Qty.Equal(d("1")) || !m.Quote.Qty.Equal(d("0.05")) || m.Fee == nil {
		t.Errorf("two-fee trade %+v", m)
	}
	if len(byID) != 5 || byID["tradefee:m2"].Type != model.Fee || byID["tbill:w1"].Type != model.Withdrawal || byID["tbill:p1"].Type != model.Withdrawal {
		t.Errorf("activities %v", keys(byID))
	}
	// Holdings still add up.
	h := model.Holdings(got)
	if !h["crypto:ETH"].Qty.Equal(d("0.99")) || !h["crypto:BTC"].Qty.Equal(d("-0.951")) {
		t.Errorf("holdings %v", h)
	}
}

func TestRegistered(t *testing.T) {
	def, err := source.Get("okx")
	if err != nil || def.Name != "OKX" || def.Category != "exchange" || def.New == nil || !strings.Contains(def.Setup, "Read") {
		t.Fatalf("definition %+v %v", def, err)
	}
	secrets := 0
	for _, fl := range def.Fields {
		if fl.Secret {
			secrets++
		}
	}
	if secrets != 2 || len(def.Fields) != 4 {
		t.Errorf("fields %+v", def.Fields)
	}
}

func TestSleepCtx(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sleepCtx(ctx, time.Hour); err == nil {
		t.Fatal("canceled sleep")
	}
	if err := sleepCtx(context.Background(), time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func keys[V any](m map[string]V) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func contains(logs []string, s string) bool {
	for _, l := range logs {
		if strings.Contains(l, s) {
			return true
		}
	}
	return false
}

// A European key does not exist on www.okx.com (50119): the client moves to
// the regional domain by itself, unless the user set one.
func TestRegionalDomain(t *testing.T) {
	notHere := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"50119","msg":"API key doesn't exist","data":[]}`)
	}))
	defer notHere.Close()
	here := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"code":"0","msg":"","data":[{"uid":"1","perm":"read_only"}]}`)
	}))
	defer here.Close()
	old := regionBases
	defer func() { regionBases = old }()

	regionBases = []string{notHere.URL, here.URL}
	c := &client{base: notHere.URL, auto: true, key: "k", secret: "s", pass: "p", http: here.Client(), now: time.Now, sleep: sleepCtx}
	var out []map[string]string
	if err := c.do(context.Background(), "GET", "/api/v5/account/config", nil, nil, &out); err != nil || c.base != here.URL || out[0]["perm"] != "read_only" {
		t.Fatalf("switch: %v %s %v", err, c.base, out)
	}

	regionBases = []string{notHere.URL}
	c = &client{base: notHere.URL, auto: true, key: "k", secret: "s", pass: "p", http: here.Client(), now: time.Now, sleep: sleepCtx}
	if err := c.do(context.Background(), "GET", "/api/v5/account/config", nil, nil, &out); err == nil || !strings.Contains(err.Error(), "none of OKX's domains") {
		t.Fatalf("nowhere: %v", err)
	}

	// A domain chosen by the user is kept.
	c = &client{base: notHere.URL, key: "k", secret: "s", pass: "p", http: here.Client(), now: time.Now, sleep: sleepCtx}
	if err := c.do(context.Background(), "GET", "/api/v5/account/config", nil, nil, &out); err == nil || !strings.Contains(err.Error(), "50119") || c.base != notHere.URL {
		t.Fatalf("fixed domain: %v", err)
	}
}
