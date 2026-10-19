package kraken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

const (
	testKey = "key-1"
	// Kraken's published example private key.
	testSecret = "kQH5HW/8p1uGOVjbgWA7FunAmGO8lsSUXNsu3eow76sz84Q18fWxnyRzBHCd3pd5nE9qa99HAZtuZuj6F1huXg=="
)

func TestSignKrakenVector(t *testing.T) {
	secret, _ := decodeSecret(testSecret)
	got := sign(secret, "/0/private/AddOrder", "1616492376594",
		"nonce=1616492376594&ordertype=limit&pair=XBTUSD&price=37500&type=buy&volume=1.25")
	want := "4/dpxb3iT4tp/ZCVEwSnEsLxx0bqyhLpdfOpc6fn7OR8+UClSV5n9E6aSS8MPtnRfp32bAb0nmbRn6H8ndwLUQ=="
	if got != want {
		t.Fatalf("sign = %s, want %s", got, want)
	}
}

func decodeSecret(s string) ([]byte, error) {
	src, err := New(source.Config{Values: map[string]string{"api_key": "k", "api_secret": s}})
	if err != nil {
		return nil, err
	}
	return src.(*Source).c.secret, nil
}

// ---- fake Kraken

type ledgerEntry struct {
	id, refid, typ, sub, asset, amount, fee string
	time                                    float64
}

type fake struct {
	t       *testing.T
	srv     *httptest.Server
	mu      sync.Mutex
	entries []ledgerEntry
	balance map[string]string
	calls   map[string][]url.Values
	// fail, when set, answers instead of the route (then is cleared if once).
	fail      func(path string, n int) (status int, body any, ok bool)
	lastNonce int64
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, calls: map[string][]url.Values{}, balance: map[string]string{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	reply := func(errs []string, result any) {
		if errs == nil {
			errs = []string{}
		}
		json.NewEncoder(w).Encode(map[string]any{"error": errs, "result": result})
	}
	if r.Method == http.MethodGet && r.URL.Path == "/0/public/Assets" {
		f.calls[r.URL.Path] = append(f.calls[r.URL.Path], nil)
		reply(nil, map[string]any{
			"XXBT":  map[string]any{"aclass": "currency", "altname": "XBT"},
			"XETH":  map[string]any{"aclass": "currency", "altname": "ETH"},
			"XXDG":  map[string]any{"aclass": "currency", "altname": "XDG"},
			"ZEUR":  map[string]any{"aclass": "currency", "altname": "EUR"},
			"ZETA":  map[string]any{"aclass": "currency", "altname": "ZETA"},
			"DOT":   map[string]any{"aclass": "currency", "altname": "DOT"},
			"DOT.S": map[string]any{"aclass": "currency", "altname": "DOT.S"},
			"KFEE":  map[string]any{"aclass": "currency", "altname": "FEE"},
		})
		return
	}
	body, _ := io.ReadAll(r.Body)
	form, err := url.ParseQuery(string(body))
	if err != nil || r.Method != http.MethodPost {
		f.t.Errorf("bad request %s %s", r.Method, r.URL.Path)
		return
	}
	nonce := form.Get("nonce")
	secret, _ := decodeSecret(testSecret)
	if r.Header.Get("API-Key") != testKey || r.Header.Get("API-Sign") != sign(secret, r.URL.Path, nonce, string(body)) {
		reply([]string{"EAPI:Invalid signature"}, nil)
		return
	}
	n, _ := strconv.ParseInt(nonce, 10, 64)
	if n <= f.lastNonce {
		reply([]string{"EAPI:Invalid nonce"}, nil)
		return
	}
	f.lastNonce = n
	f.calls[r.URL.Path] = append(f.calls[r.URL.Path], form)
	if f.fail != nil {
		if status, b, ok := f.fail(r.URL.Path, len(f.calls[r.URL.Path])); ok {
			w.WriteHeader(status)
			json.NewEncoder(w).Encode(b)
			return
		}
	}
	switch r.URL.Path {
	case "/0/private/BalanceEx":
		res := map[string]any{}
		for k, v := range f.balance {
			res[k] = map[string]any{"balance": v, "hold_trade": "0.0000"}
		}
		reply(nil, res)
	case "/0/private/Ledgers":
		var list []ledgerEntry
		start, _ := strconv.ParseInt(form.Get("start"), 10, 64)
		for _, e := range f.entries {
			if start == 0 || e.time > float64(start) {
				list = append(list, e)
			}
		}
		sort.SliceStable(list, func(i, j int) bool { return list[i].time > list[j].time })
		ofs, _ := strconv.Atoi(form.Get("ofs"))
		page := map[string]any{}
		for i := ofs; i < len(list) && i < ofs+50; i++ {
			e := list[i]
			page[e.id] = map[string]any{"refid": e.refid, "time": e.time, "type": e.typ, "subtype": e.sub,
				"aclass": "currency", "asset": e.asset, "amount": e.amount, "fee": e.fee, "balance": "0"}
		}
		var count any = len(list)
		if form.Get("without_count") == "true" {
			count = nil
		}
		reply(nil, map[string]any{"ledger": page, "count": count})
	default:
		reply([]string{"EGeneral:Unknown method"}, nil)
	}
}

func (f *fake) add(id, refid, typ, sub, asset, amount, fee string, at time.Time) {
	sec := float64(at.UnixMilli()) / 1000
	f.entries = append(f.entries, ledgerEntry{id: id, refid: refid, typ: typ, sub: sub, asset: asset, amount: amount, fee: fee, time: sec})
}

func (f *fake) source(t *testing.T, state []byte, now time.Time) (*Source, *[]time.Duration) {
	s, err := New(source.Config{Values: map[string]string{"api_key": testKey, "api_secret": testSecret},
		HTTP: f.srv.Client(), State: state})
	if err != nil {
		t.Fatal(err)
	}
	src := s.(*Source)
	src.c.base = f.srv.URL
	src.c.now = func() time.Time { return now }
	var sleeps []time.Duration
	src.c.sleep = func(_ context.Context, d time.Duration) error { sleeps = append(sleeps, d); return nil }
	return src, &sleeps
}

var t0 = time.Date(2024, 4, 10, 12, 0, 0, 0, time.UTC)

func at(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }

// scenario wires a consistent account: replaying everything must give the balances.
func scenario(f *fake) {
	f.add("LDEP1-AAAAA-EUR001", "QDEP1", "deposit", "", "ZEUR", "1000.0000", "0.0000", at(0))
	f.add("LDEP2-AAAAA-BTC001", "QDEP2", "deposit", "", "XXBT", "0.1000000000", "0.0000000000", at(1))
	f.add("LDEP3-AAAAA-DOT001", "QDEP3", "deposit", "", "DOT", "50.0000000000", "0.0000000000", at(1))
	// Spot buy: 0.01 BTC for 500 EUR, fee in EUR.
	f.add("LTRD1-AAAAA-EUR001", "TBUY1-AAAAA-AAAAAA", "trade", "", "ZEUR", "-500.0000", "1.3000", at(2))
	f.add("LTRD1-AAAAA-BTC001", "TBUY1-AAAAA-AAAAAA", "trade", "", "XXBT", "0.0100000000", "0.0000000000", at(2))
	// Spot sell: 0.005 BTC for 300 EUR, fee on the EUR leg; fee credits ignored.
	f.add("LTRD2-AAAAA-BTC001", "TSELL-AAAAA-AAAAAA", "trade", "", "XXBT", "-0.0050000000", "0.0000000000", at(3))
	f.add("LTRD2-AAAAA-EUR001", "TSELL-AAAAA-AAAAAA", "trade", "", "ZEUR", "300.0000", "0.7800", at(3))
	f.add("LTRD2-AAAAA-KFEE01", "TSELL-AAAAA-AAAAAA", "trade", "", "KFEE", "0.00", "50.00", at(3))
	// Buy Crypto button: spend EUR, receive ETH.
	f.add("LSPD1-AAAAA-EUR001", "RBUY1-AAAAA-AAAAAA", "spend", "", "ZEUR", "-100.0000", "1.5000", at(4))
	f.add("LRCV1-AAAAA-ETH001", "RBUY1-AAAAA-AAAAAA", "receive", "", "XETH", "0.0300000000", "0.0000000000", at(4))
	// Withdrawal with its fee.
	f.add("LWDR1-AAAAA-BTC001", "AWDR1-AAAAA-AAAAAA", "withdrawal", "", "XXBT", "-0.0500000000", "0.0001000000", at(5))
	// A withdrawal that failed and was given back: nothing.
	f.add("LWDR2-AAAAA-ETH001", "AWDR2-AAAAA-AAAAAA", "withdrawal", "", "XETH", "-0.0100000000", "0.0010000000", at(6))
	f.add("LWDR3-AAAAA-ETH001", "AWDR2-AAAAA-AAAAAA", "withdrawal", "", "XETH", "0.0100000000", "-0.0010000000", at(6))
	// DOT to staking (two transfers, internal), then a staking reward.
	f.add("LSTK1-AAAAA-DOT001", "RSTK1-AAAAA-AAAAAA", "transfer", "spottostaking", "DOT", "-20.0000000000", "0.0000000000", at(7))
	f.add("LSTK2-AAAAA-DOT001", "RSTK2-AAAAA-AAAAAA", "transfer", "stakingfromspot", "DOT.S", "20.0000000000", "0.0000000000", at(7))
	f.add("LRWD1-AAAAA-DOT001", "RRWD1-AAAAA-AAAAAA", "staking", "", "DOT.S", "0.1000000000", "0.0000000000", at(8))
	// Earn: ETH allocated to flexible Earn (internal), then a reward.
	f.add("LERN1-AAAAA-ETH001", "RERN1-AAAAA-AAAAAA", "earn", "allocation", "XETH", "-0.0100000000", "0.0000000000", at(9))
	f.add("LERN2-AAAAA-ETH001", "RERN1-AAAAA-AAAAAA", "earn", "allocation", "ETH.F", "0.0100000000", "0.0000000000", at(9))
	f.add("LERN3-AAAAA-ETH001", "RERN3-AAAAA-AAAAAA", "earn", "reward", "ETH.F", "0.0001000000", "0.0000000000", at(10))
	// Legacy ETH2 staking: a reward, then its virtual move to ETH (same refid).
	f.add("LETH2-AAAAA-ETH001", "RETH2-AAAAA-AAAAAA", "staking", "", "ETH2.S", "0.0010000000", "0.0000000000", at(11))
	f.add("LETH3-AAAAA-ETH001", "RETH3-AAAAA-AAAAAA", "staking", "", "ETH2", "-0.0010000000", "0.0000000000", at(12))
	f.add("LETH4-AAAAA-ETH001", "RETH3-AAAAA-AAAAAA", "staking", "", "XETH", "0.0010000000", "0.0000000000", at(12))
	// Margin rollover: a fee alone.
	f.add("LROL1-AAAAA-EUR001", "RROL1-AAAAA-AAAAAA", "rollover", "", "ZEUR", "0.0000", "0.0500", at(13))
	// 45 small DOGE deposits, so the ledger spans two pages.
	for i := 0; i < 45; i++ {
		f.add(fmt.Sprintf("LDOG%02d-AAAAA-DOGE01", i), fmt.Sprintf("QDOG%02d", i), "deposit", "", "XXDG", "2.00000000", "0.00000000", at(20+5*i))
	}

	// EUR: 1000 -500 -1.3 +300 -0.78 -100 -1.5 -0.05 = 696.37
	f.balance["ZEUR"] = "690.3700"
	f.balance["EUR.HOLD"] = "6.0000"
	// BTC: 0.1 +0.01 -0.005 -0.05 -0.0001 = 0.0549
	f.balance["XXBT"] = "0.0449000000"
	f.balance["XBT.M"] = "0.0100000000"
	// ETH: 0.03 -0.01 +0.01 +0.0001 +0.001 = 0.0311
	f.balance["XETH"] = "0.0210000000"
	f.balance["ETH.F"] = "0.0101000000"
	f.balance["ETH2.S"] = "0.0000000000"
	// DOT: 50 +0.1
	f.balance["DOT"] = "30.0000000000"
	f.balance["DOT.S"] = "20.1000000000"
	f.balance["XXDG"] = "90.00000000"
	f.balance["KFEE"] = "950.00"
}

func collect(t *testing.T, s *Source, since time.Time) map[string]model.Activity {
	t.Helper()
	got := map[string]model.Activity{}
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
	})
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func keys(m map[string]model.Activity) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestFullHistoryReconciles(t *testing.T) {
	f := newFake(t)
	scenario(f)
	s, _ := f.source(t, nil, at(100))
	acts := collect(t, s, time.Time{})

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
	if a := check("LDEP1-AAAAA-EUR001", model.Deposit, "", "EUR", "1000"); a.Base.Asset.Kind != model.Fiat || a.Fee != nil {
		t.Errorf("EUR deposit %+v", a)
	}
	check("LDEP2-AAAAA-BTC001", model.Deposit, "", "BTC", "0.1")

	buy := check("TBUY1-AAAAA-AAAAAA", model.Trade, model.Buy, "BTC", "0.01")
	if buy.Quote == nil || buy.Quote.Asset.Symbol != "EUR" || !buy.Quote.Qty.Equal(d("500")) ||
		buy.Fee == nil || buy.Fee.Asset.Symbol != "EUR" || !buy.Fee.Qty.Equal(d("1.3")) {
		t.Errorf("buy quote %+v fee %+v", buy.Quote, buy.Fee)
	}
	if !buy.Price().Equal(d("50000")) || !buy.Time.Equal(at(2)) {
		t.Errorf("buy price %s time %v", buy.Price(), buy.Time)
	}
	sell := check("TSELL-AAAAA-AAAAAA", model.Trade, model.Sell, "BTC", "0.005")
	if !sell.Quote.Qty.Equal(d("300")) || sell.Fee == nil || !sell.Fee.Qty.Equal(d("0.78")) {
		t.Errorf("sell quote %+v fee %+v", sell.Quote, sell.Fee)
	}
	bc := check("RBUY1-AAAAA-AAAAAA", model.Trade, model.Buy, "ETH", "0.03")
	if !bc.Quote.Qty.Equal(d("100")) || bc.Quote.Asset.Symbol != "EUR" || bc.Fee == nil || !bc.Fee.Qty.Equal(d("1.5")) {
		t.Errorf("buy crypto quote %+v fee %+v", bc.Quote, bc.Fee)
	}
	wd := check("LWDR1-AAAAA-BTC001", model.Withdrawal, "", "BTC", "0.05")
	if wd.Fee == nil || wd.Fee.Asset.Symbol != "BTC" || !wd.Fee.Qty.Equal(d("0.0001")) {
		t.Errorf("withdrawal fee %+v", wd.Fee)
	}
	check("LRWD1-AAAAA-DOT001", model.Reward, "", "DOT", "0.1")
	check("LERN3-AAAAA-ETH001", model.Reward, "", "ETH", "0.0001")
	check("LETH2-AAAAA-ETH001", model.Reward, "", "ETH", "0.001")
	check("LROL1-AAAAA-EUR001:fee", model.Fee, "", "EUR", "0.05")
	check("LDOG07-AAAAA-DOGE01", model.Deposit, "", "DOGE", "2")
	for id, a := range acts {
		if strings.Contains(id, "STK") || strings.HasPrefix(id, "LERN1") || strings.HasPrefix(id, "LERN2") ||
			strings.Contains(id, "WDR2") || strings.Contains(id, "RETH3") || strings.Contains(id, "KFEE") || a.Base.Asset.Symbol == "KFEE" {
			t.Errorf("internal or canceled movement emitted: %s %+v", id, a)
		}
	}
	if len(acts) != 11+45 {
		t.Errorf("got %d activities: %v", len(acts), keys(acts))
	}
	if n := len(f.calls["/0/private/Ledgers"]); n != 2 {
		t.Errorf("ledger pages = %d, want 2", n)
	}
	if got := f.calls["/0/private/Ledgers"][1].Get("ofs"); got != "50" {
		t.Errorf("page 2 ofs = %s", got)
	}
	if f.calls["/0/private/Ledgers"][0].Get("start") != "" {
		t.Error("first run must read the whole ledger")
	}

	bal, err := s.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	have := map[string]string{}
	for _, b := range bal {
		have[b.Asset.Symbol] = b.Qty.String()
	}
	want := map[string]string{"EUR": "696.37", "BTC": "0.0549", "ETH": "0.0311", "DOT": "50.1", "DOGE": "90"}
	if fmt.Sprint(have) != fmt.Sprint(want) {
		t.Errorf("balances %v, want %v", have, want)
	}
	if n := len(f.calls["/0/public/Assets"]); n != 1 {
		t.Errorf("asset list read %d times", n)
	}
	list := make([]model.Activity, 0, len(acts))
	for _, a := range acts {
		list = append(list, a)
	}
	if gaps := model.Reconcile(list, bal, decimal.Zero); len(gaps) != 0 {
		t.Fatalf("history does not add up to balances: %+v", gaps)
	}

	var st state
	if err := json.Unmarshal(s.State(), &st); err != nil || !unixTime(st.Last).Equal(at(240)) {
		t.Errorf("state %s (%v)", s.State(), unixTime(st.Last))
	}
}

func TestIncremental(t *testing.T) {
	f := newFake(t)
	scenario(f)
	s, _ := f.source(t, nil, at(250))
	collect(t, s, time.Time{})

	// A later run: new entries, read from the last one seen minus the overlap.
	f.add("LNEW1-AAAAA-EUR001", "QNEW1", "deposit", "", "ZEUR", "10.0000", "0.0000", at(300))
	s2, _ := f.source(t, s.State(), at(301))
	f.calls = map[string][]url.Values{}
	acts := collect(t, s2, at(250))
	calls := f.calls["/0/private/Ledgers"]
	if len(calls) != 1 {
		t.Fatalf("ledger calls %d", len(calls))
	}
	if want := strconv.FormatInt(at(240).Add(-overlap).Unix(), 10); calls[0].Get("start") != want {
		t.Errorf("start = %s, want %s", calls[0].Get("start"), want)
	}
	if _, ok := acts["LNEW1-AAAAA-EUR001"]; !ok {
		t.Errorf("new deposit missing: %v", keys(acts))
	}
	if _, ok := acts["LDEP1-AAAAA-EUR001"]; ok {
		t.Error("old entries before the overlap must not be read again")
	}
	if len(acts) != 15+1 { // DOGE deposits since at(168), and the new one
		t.Errorf("got %d activities", len(acts))
	}
	var st state
	json.Unmarshal(s2.State(), &st)
	if !unixTime(st.Last).Equal(at(300)) {
		t.Errorf("state last = %v", unixTime(st.Last))
	}
}

func TestAssetNames(t *testing.T) {
	names := map[string]string{"XXBT": "XBT", "XETH": "ETH", "ZEUR": "EUR", "ZETA": "ZETA", "XTZ": "XTZ", "ZCHF": "ZCHF", "XXDG": "XDG"}
	cases := map[string]string{
		"XXBT": "BTC", "XBT.M": "BTC", "XETH": "ETH", "ETH2": "ETH", "ETH2.S": "ETH", "ETH.F": "ETH",
		"ZEUR": "EUR", "EUR.HOLD": "EUR", "EUR.M": "EUR", "XXDG": "DOGE", "ZETA": "ZETA", "XTZ": "XTZ", "XTZ.S": "XTZ",
		"ZCHF": "ZCHF", "DOT.S": "DOT", "DOT28.S": "DOT", "ATOM21.S": "ATOM", "SOL03.S": "SOL", "KSM.P": "KSM",
		"USDC.B": "USDC", "FLOWH.S": "FLOW", "USDC.CORE": "USDC.CORE", "KFEE": "", "1INCH": "1INCH",
	}
	for code, want := range cases {
		if got := symbol(code, names); got != want {
			t.Errorf("symbol(%s) = %q, want %q", code, got, want)
		}
	}
	// Without the asset list, the legacy codes still map.
	for code, want := range map[string]string{"XXBT": "BTC", "ZUSD": "USD", "XXRP": "XRP", "ZETA": "ZETA", "XETH": "ETH"} {
		if got := symbol(code, nil); got != want {
			t.Errorf("symbol(%s) without list = %q, want %q", code, got, want)
		}
	}
	if a := model.CryptoOrFiat(symbol("ZGBP", nil)); a.Kind != model.Fiat || a.Symbol != "GBP" {
		t.Errorf("ZGBP = %+v", a)
	}
}

func TestRateLimitRetry(t *testing.T) {
	f := newFake(t)
	scenario(f)
	f.fail = func(path string, n int) (int, any, bool) {
		switch {
		case path == "/0/private/Ledgers" && n == 1:
			return 200, map[string]any{"error": []string{"EAPI:Rate limit exceeded"}}, true
		case path == "/0/private/Ledgers" && n == 2:
			return 502, map[string]any{"error": []string{}}, true
		}
		return 0, nil, false
	}
	s, sleeps := f.source(t, nil, at(100))
	acts := collect(t, s, time.Time{})
	if len(acts) != 56 {
		t.Errorf("got %d activities after retries", len(acts))
	}
	if len(*sleeps) < 2 || (*sleeps)[0] != 3*time.Second || (*sleeps)[1] < 6*time.Second {
		t.Errorf("sleeps %v", *sleeps)
	}
}

func TestThrottle(t *testing.T) {
	c := &client{now: func() time.Time { return t0 }}
	var slept time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { slept += d; return nil }
	for i := 0; i < 7; i++ { // 14 points: under the limit of 15
		c.throttle(context.Background(), 2)
	}
	if slept != 0 {
		t.Fatalf("slept %v under the limit", slept)
	}
	c.throttle(context.Background(), 2) // 16: must wait for 1 point to decay
	one := 1.0
	if want := time.Duration(one / counterDecay * float64(time.Second)); slept != want {
		t.Errorf("slept %v, want %v", slept, want)
	}
}

func TestPermissionError(t *testing.T) {
	f := newFake(t)
	f.fail = func(path string, n int) (int, any, bool) {
		if path == "/0/private/Ledgers" {
			return 200, map[string]any{"error": []string{"EGeneral:Permission denied"}}, true
		}
		return 0, nil, false
	}
	s, _ := f.source(t, nil, at(0))
	err := s.Test(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || !strings.Contains(err.Error(), `"Query Ledger Entries"`) {
		t.Fatalf("Test error = %v", err)
	}
	if len(f.calls["/0/private/Ledgers"]) != 1 {
		t.Error("a permission error must not be retried")
	}

	f.fail = nil
	if err := s.Test(context.Background()); err != nil {
		t.Errorf("Test with good key: %v", err)
	}
	if strings.Contains(fmt.Sprint(err), testSecret) {
		t.Error("secret in error")
	}
}

func TestBadSignature(t *testing.T) {
	f := newFake(t)
	s, err := New(source.Config{Values: map[string]string{"api_key": testKey, "api_secret": "c2VjcmV0"}, HTTP: f.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	src := s.(*Source)
	src.c.base = f.srv.URL
	err = src.Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "private key") || strings.Contains(err.Error(), "c2VjcmV0") {
		t.Fatalf("error = %v", err)
	}
	if _, err := New(source.Config{Values: map[string]string{"api_key": "k", "api_secret": "not base64!"}}); err == nil {
		t.Error("a private key that is not base64 must be refused")
	}
}

func TestDefinition(t *testing.T) {
	def, err := source.Get("kraken")
	if err != nil {
		t.Fatal(err)
	}
	if def.Category != "exchange" || len(def.Fields) != 2 || !def.Fields[1].Secret || len(strings.Fields(def.Description)) > 10 {
		t.Errorf("definition %+v", def)
	}
}
