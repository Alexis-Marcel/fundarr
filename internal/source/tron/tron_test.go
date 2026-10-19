package tron

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
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

var (
	wallet   = FromHex("41" + strings.Repeat("11", 20))
	other    = FromHex("41" + strings.Repeat("22", 20))
	dex      = FromHex("41" + strings.Repeat("33", 20))
	fakeUSDT = FromHex("41" + strings.Repeat("44", 20))
	spamTok  = FromHex("41" + strings.Repeat("55", 20))
	unseen   = FromHex("41" + strings.Repeat("66", 20))
	inactive = FromHex("41" + strings.Repeat("77", 20))
)

const usdt = "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"

type obj = map[string]any

// fake serves TronGrid-shaped answers.
type fake struct {
	mu       sync.Mutex
	txs      []obj
	trc20    []obj
	accounts map[string]obj
	rewards  map[string]int64
	key      string // when set, required in TRON-PRO-API-KEY
	calls    []string
	limitOne bool
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.key != "" && r.Header.Get("TRON-PRO-API-KEY") != f.key {
		w.WriteHeader(401)
		fmt.Fprint(w, `{"Error":"ApiKey not exists"}`)
		return
	}
	q := r.URL.Query()
	f.calls = append(f.calls, r.URL.Path+"?"+q.Get("min_timestamp")+":"+q.Get("fingerprint"))
	if f.limitOne {
		f.limitOne = false
		w.WriteHeader(429)
		return
	}
	if r.URL.Path == "/wallet/gettransactioninfobyid" {
		var in struct{ Value string }
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &in)
		if r.Method != http.MethodPost {
			http.Error(w, "post", 405)
			return
		}
		fmt.Fprintf(w, `{"id":%q,"withdraw_amount":%d}`, in.Value, f.rewards[in.Value])
		return
	}
	parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/") // v1 accounts ADDR [transactions [trc20]]
	if len(parts) < 3 || parts[0] != "v1" || parts[1] != "accounts" {
		http.Error(w, "not found", 404)
		return
	}
	addr := parts[2]
	if len(parts) == 3 {
		var data []obj
		if a, ok := f.accounts[addr]; ok {
			data = append(data, a)
		}
		b, _ := json.Marshal(obj{"data": data, "success": true, "meta": obj{"page_size": len(data)}})
		w.Write(b)
		return
	}
	if q.Get("only_confirmed") != "true" || q.Get("order_by") != "block_timestamp,asc" {
		http.Error(w, "bad query", 400)
		return
	}
	rows := f.txs
	if len(parts) == 5 && parts[4] == "trc20" {
		rows = f.trc20
	}
	if addr != wallet {
		rows = nil
	}
	min, _ := strconv.ParseInt(q.Get("min_timestamp"), 10, 64)
	var sel []obj
	for _, x := range rows {
		if ts(x) >= min {
			sel = append(sel, x)
		}
	}
	sort.SliceStable(sel, func(i, j int) bool { return ts(sel[i]) < ts(sel[j]) })
	lo, _ := strconv.Atoi(q.Get("fingerprint"))
	size, _ := strconv.Atoi(q.Get("limit"))
	hi := min2(lo+size, len(sel))
	if lo > hi {
		lo = hi
	}
	meta := obj{"page_size": hi - lo}
	if hi < len(sel) {
		meta["fingerprint"] = strconv.Itoa(hi)
	}
	b, _ := json.Marshal(obj{"data": sel[lo:hi], "success": true, "meta": meta})
	w.Write(b)
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func ts(x obj) int64 {
	switch v := x["block_timestamp"].(type) {
	case int64:
		return v
	case int:
		return int64(v)
	}
	return 0
}

// tx builds a transaction row as /v1/accounts/{a}/transactions returns it:
// addresses in hex, amounts in sun.
func tx(id string, at int64, typ, status string, fee int64, value obj, internal ...obj) obj {
	for _, k := range []string{"owner_address", "to_address", "contract_address"} {
		if s, ok := value[k].(string); ok {
			value[k] = ToHex(s)
		}
	}
	for _, in := range internal {
		in["from_address"], in["to_address"] = ToHex(in["from_address"].(string)), ToHex(in["to_address"].(string))
	}
	if internal == nil {
		internal = []obj{}
	}
	return obj{
		"ret": []obj{{"contractRet": status, "fee": fee}}, "txID": id, "blockNumber": at / 3, "block_timestamp": at,
		"net_fee": 0, "energy_fee": 0, "net_usage": 0, "energy_usage": 0,
		"raw_data": obj{"contract": []obj{{"type": typ, "parameter": obj{"value": value,
			"type_url": "type.googleapis.com/protocol." + typ}}}},
		"internal_transactions": internal,
	}
}

func trc(id string, at int64, from, to, value, contract, symbol, name string, dec int) obj {
	return obj{"transaction_id": id, "block_timestamp": at, "from": from, "to": to, "type": "Transfer", "value": value,
		"token_info": obj{"symbol": symbol, "address": contract, "decimals": dec, "name": name}}
}

func newFake() *fake {
	energy := tx("t3", 3000, "TriggerSmartContract", "SUCCESS", 13845000,
		obj{"owner_address": wallet, "contract_address": usdt, "data": "a9059cbb"})
	energy["net_fee"], energy["energy_fee"] = 345000, 13500000
	return &fake{
		txs: []obj{
			tx("t1", 1000, "TransferContract", "SUCCESS", 0, obj{"owner_address": other, "to_address": wallet, "amount": 100000000}),     // +100 TRX
			tx("t2", 2000, "TransferContract", "SUCCESS", 1100000, obj{"owner_address": wallet, "to_address": other, "amount": 3000000}), // −3, fee 1.1 (new account)
			energy, // USDT sent: fee 13.845
			tx("t4", 4000, "TriggerSmartContract", "REVERT", 2000000, obj{"owner_address": wallet, "contract_address": usdt}), // failed: fee 2
			tx("t5", 5000, "FreezeBalanceV2Contract", "SUCCESS", 268000, obj{"owner_address": wallet, "frozen_balance": 2000000, "resource": "ENERGY"}),
			tx("t6", 6000, "TriggerSmartContract", "SUCCESS", 345000, obj{"owner_address": other, "contract_address": usdt}), // USDT received: their fee
			tx("t7", 7000, "WithdrawBalanceContract", "SUCCESS", 0, obj{"owner_address": wallet}),                            // +0.5 rewards
			tx("t8", 7000, "TriggerSmartContract", "SUCCESS", 0, obj{"owner_address": wallet, "contract_address": dex, "call_value": 1000000},
				obj{"internal_tx_id": "i1", "from_address": dex, "to_address": wallet, "data": obj{"note": "call", "rejected": false, "call_value": obj{"_": 250000}}},
				obj{"internal_tx_id": "i2", "from_address": dex, "to_address": wallet, "data": obj{"note": "call", "rejected": true, "call_value": obj{"_": 9000000}}}),
			tx("t9", 8000, "TransferAssetContract", "SUCCESS", 0, obj{"owner_address": other, "to_address": wallet, "amount": 5, "asset_name": "1002000"}),
			tx("t10", 8000, "TransferContract", "REVERT", 0, obj{"owner_address": other, "to_address": wallet, "amount": 7000000}),
		},
		trc20: []obj{
			trc("t3", 3000, wallet, other, "40000000", usdt, "USDT", "Tether USD", 6),
			trc("t6", 6000, other, wallet, "100500000", usdt, "USDT", "Tether USD", 6),
			trc("t6", 6000, other, wallet, "1000000", usdt, "USDT", "Tether USD", 6),
			trc("s1", 6500, fakeUSDT, wallet, "5000000000", fakeUSDT, "USDT", "Tether USD", 6),
			trc("s2", 6500, spamTok, wallet, "1000000000000000000000", spamTok, "Visit tron-claim.xyz", "Claim", 18),
			trc("s3", 6600, wallet, fakeUSDT, "0", usdt, "USDT", "Tether USD", 6), // address poisoning
		},
		accounts: map[string]obj{wallet: {
			"address": ToHex(wallet),
			// 100 − 3 − 1.1 − 13.845 − 2 − 0.268 + 0.5 − 1 + 0.25 = 79.537, of which 2 frozen.
			"balance":          77537000,
			"frozenV2":         []obj{{}, {"type": "ENERGY", "amount": 2000000}, {"type": "TRON_POWER"}},
			"account_resource": obj{"acquired_delegated_frozenV2_balance_for_energy": 999000000}, // someone else's
			"trc20":            []obj{{usdt: "61500000"}, {fakeUSDT: "5000000000"}, {spamTok: "1000000000000000000000"}, {unseen: "7"}},
		}},
		rewards: map[string]int64{"t7": 500000},
	}
}

func connect(t *testing.T, srv *httptest.Server, values map[string]string, state []byte) *Source {
	t.Helper()
	v := map[string]string{"address": wallet, "base_url": srv.URL + "/"}
	for k, x := range values {
		v[k] = x
	}
	s, err := New(source.Config{Values: v, HTTP: srv.Client(), State: state})
	if err != nil {
		t.Fatal(err)
	}
	src := s.(*Source)
	src.api.pause = 0
	src.api.sleep = func(context.Context, time.Duration) error { return nil }
	return src
}

func fetch(t *testing.T, s *Source) ([]model.Activity, []string) {
	t.Helper()
	var acts []model.Activity
	var logs []string
	err := s.Activities(context.Background(), source.Fetch{
		Emit:     func(a model.Activity) error { acts = append(acts, a); return nil },
		Progress: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return acts, logs
}

func TestWalletHistory(t *testing.T) {
	f := newFake()
	f.limitOne = true
	f.key = "KEY"
	srv := httptest.NewServer(f)
	defer srv.Close()
	old := pageSize
	pageSize = 2 // force fingerprint paging
	defer func() { pageSize = old }()

	s := connect(t, srv, map[string]string{"api_key": "KEY"}, nil)
	if err := s.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	acts, logs := fetch(t, s)
	byID := map[string]model.Activity{}
	for _, a := range acts {
		if _, dup := byID[a.NativeID]; dup {
			t.Fatalf("duplicate %s", a.NativeID)
		}
		byID[a.NativeID] = a
		if a.Time.IsZero() || a.Time.Location() != time.UTC || a.Network != "tron" || a.TxID == "" {
			t.Fatalf("incomplete %+v", a)
		}
	}
	check := func(id string, typ model.Type, sym, qty string) model.Activity {
		t.Helper()
		a, ok := byID[id]
		if !ok {
			t.Fatalf("missing %s in %v", id, keys(byID))
		}
		if a.Type != typ || a.Base.Asset.Symbol != sym || !a.Base.Qty.Equal(decimal.RequireFromString(qty)) {
			t.Fatalf("%s = %s %s %s", id, a.Type, a.Base.Qty, a.Base.Asset.Symbol)
		}
		return a
	}
	if a := check("t1:trx", model.Deposit, "TRX", "100"); a.Address != other || a.Fee != nil || !a.Time.Equal(time.UnixMilli(1000)) {
		t.Fatalf("deposit %+v", a)
	}
	if a := check("t2:trx", model.Withdrawal, "TRX", "3"); a.Fee == nil || !a.Fee.Qty.Equal(decimal.RequireFromString("1.1")) || a.Address != other {
		t.Fatalf("withdrawal fee %+v", a)
	}
	check("t3:fee", model.Fee, "TRX", "13.845")
	if a := check("t4:fee", model.Fee, "TRX", "2"); !strings.Contains(a.Note, "failed") {
		t.Fatal("failed tx note")
	}
	if a := check("t5:fee", model.Fee, "TRX", "0.268"); !strings.Contains(a.Note, "FreezeBalanceV2") {
		t.Fatalf("note %q", a.Note)
	}
	check("t7:reward", model.Reward, "TRX", "0.5")
	if a := check("t8:trx", model.Withdrawal, "TRX", "1"); a.Address != dex {
		t.Fatalf("call value %+v", a)
	}
	check("t8:internal:i1", model.Deposit, "TRX", "0.25")
	check("t3:trc20:"+usdt+":0", model.Withdrawal, "USDT", "40")
	in0 := check("t6:trc20:"+usdt+":0", model.Deposit, "USDT", "1")
	check("t6:trc20:"+usdt+":1", model.Deposit, "USDT", "100.5")
	if in0.Address != other || in0.Note != "" {
		t.Fatalf("usdt in %+v", in0)
	}
	for id := range byID {
		for _, skip := range []string{"t6:fee", "t6:trx", "t9", "t10", "s1", "s2", "s3", "i2"} {
			if strings.Contains(id, skip) {
				t.Fatalf("should be skipped: %s", id)
			}
		}
	}
	if len(acts) != 11 {
		t.Fatalf("%d activities: %v", len(acts), keys(byID))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "token transfers") {
		t.Fatal("progress")
	}
	paged := false
	for _, c := range f.calls {
		if strings.HasSuffix(c, ":2") && strings.Contains(c, "/trc20") {
			paged = true
		}
	}
	if !paged {
		t.Fatalf("no fingerprint walk in %v", f.calls)
	}

	bal, err := s.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(bal) != 2 {
		t.Fatalf("balances %+v", bal)
	}
	for _, b := range bal {
		if b.Asset.Symbol == "TRX" && !b.Qty.Equal(decimal.RequireFromString("79.537")) {
			t.Fatalf("TRX with frozen %s", b.Qty)
		}
	}
	if gaps := model.Reconcile(acts, bal, decimal.Zero); len(gaps) != 0 {
		t.Fatalf("gaps %+v", gaps)
	}

	// Next run starts from the saved timestamps and finds only the last
	// millisecond again, with the same ids.
	state := s.State()
	var st struct {
		Since  map[string]int64 `json:"since"`
		Tokens map[string]token `json:"tokens"`
	}
	json.Unmarshal(state, &st)
	if st.Since["transactions"] != 8000 || st.Since["trc20"] != 6600 || !st.Tokens[fakeUSDT].Spam || st.Tokens[usdt].Decimals != 6 {
		t.Fatalf("state %s", state)
	}
	f.calls = nil
	f.trc20 = append(f.trc20, trc("t11", 9000, other, wallet, "2000000", usdt, "USDT", "Tether USD", 6))
	s2 := connect(t, srv, map[string]string{"api_key": "KEY"}, state)
	acts2, _ := fetch(t, s2)
	for _, c := range f.calls {
		if strings.Contains(c, "?0:") {
			t.Fatalf("incremental run restarted from 0: %v", f.calls)
		}
	}
	ids := map[string]bool{}
	for _, a := range acts2 {
		ids[a.NativeID] = true
	}
	if len(acts2) != 1 || !ids["t11:trc20:"+usdt+":0"] {
		t.Fatalf("incremental %v", ids)
	}
	// Rows of a millisecond fetched again keep their ids.
	s3 := connect(t, srv, map[string]string{"api_key": "KEY"}, []byte(`{"since":{"transactions":9999,"trc20":6000}}`))
	acts3, _ := fetch(t, s3)
	for _, a := range acts3 {
		if b, ok := byID[a.NativeID]; ok && !b.Base.Qty.Equal(a.Base.Qty) {
			t.Fatalf("id %s moved: %s then %s", a.NativeID, b.Base.Qty, a.Base.Qty)
		}
	}
	if len(acts3) != 3 {
		t.Fatalf("refetch %d", len(acts3))
	}
}

func keys(m map[string]model.Activity) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestInactiveAccount(t *testing.T) {
	srv := httptest.NewServer(newFake())
	defer srv.Close()
	s := connect(t, srv, map[string]string{"address": inactive}, nil)
	if err := s.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	if acts, _ := fetch(t, s); len(acts) != 0 {
		t.Fatalf("activities %+v", acts)
	}
	bal, err := s.Balances(context.Background())
	if err != nil || len(bal) != 0 {
		t.Fatalf("balances %+v %v", bal, err)
	}
	// A canonical token held but never seen in history still counts.
	f := newFake()
	f.accounts[inactive] = obj{"balance": 0, "trc20": []obj{{usdt: "1500000"}}}
	srv2 := httptest.NewServer(f)
	defer srv2.Close()
	bal, _ = connect(t, srv2, map[string]string{"address": inactive}, nil).Balances(context.Background())
	if len(bal) != 1 || bal[0].Asset.Symbol != "USDT" || !bal[0].Qty.Equal(decimal.RequireFromString("1.5")) {
		t.Fatalf("canonical balance %+v", bal)
	}
}

func TestAddresses(t *testing.T) {
	good := []string{usdt, "TXkWRFnC2Jd8SVktsThNeQ2SA1pLdb4jsA", "TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8", wallet}
	for _, a := range good {
		if !Valid(a) {
			t.Errorf("%s should be valid", a)
		}
	}
	bad := []string{"", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6u", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj60",
		"1BvBMSEYstWetqTFn5Au4m4GFg7xJaNVN2", "0xdac17f958d2ee523a2206206994597c13d831ec7",
		encode(append([]byte{0x42}, make([]byte, 20)...))} // other version byte
	for _, a := range bad {
		if Valid(a) {
			t.Errorf("%q should be invalid", a)
		}
	}
	if h := ToHex(usdt); h != "41a614f803b6fd780986a42c78ec9c7f77e6ded13c" {
		t.Fatalf("hex %s", h)
	}
	for _, h := range []string{"41a614f803b6fd780986a42c78ec9c7f77e6ded13c", "0xa614f803b6fd780986a42c78ec9c7f77e6ded13c", "a614f803b6fd780986a42c78ec9c7f77e6ded13c"} {
		if FromHex(h) != usdt {
			t.Errorf("%s → %s", h, FromHex(h))
		}
	}
	if FromHex(usdt) != usdt || FromHex("42"+strings.Repeat("00", 20)) == usdt || ToHex("nope") != "" {
		t.Error("passthrough")
	}
	if _, err := New(source.Config{Values: map[string]string{"address": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6u"}}); err == nil || !strings.Contains(err.Error(), "Tron address") {
		t.Errorf("bad address: %v", err)
	}
	if _, err := New(source.Config{Values: map[string]string{"address": wallet}, State: []byte("{")}); err == nil {
		t.Error("bad state")
	}
	s, err := New(source.Config{Values: map[string]string{"address": " " + wallet + " "}})
	if err != nil || s.(*Source).api.base != "https://api.trongrid.io" || s.(*Source).api.pause != 350*time.Millisecond {
		t.Fatalf("defaults %v", err)
	}
	if err := s.Activities(context.Background(), source.Fetch{}); err == nil {
		t.Error("no emit")
	}
	s, _ = New(source.Config{Values: map[string]string{"address": wallet}, State: []byte(`{}`)})
	if s.(*Source).st.Since == nil || s.(*Source).st.Tokens == nil {
		t.Error("empty state maps")
	}
}

func TestSpam(t *testing.T) {
	cases := []struct {
		contract, sym, name string
		want                bool
	}{
		{usdt, "USDT", "Tether USD", false},
		{usdt, "USDTOLD", "renamed", false},
		{fakeUSDT, "USDT", "Tether USD", true},
		{fakeUSDT, "usdc", "USD Coin", true},
		{spamTok, "JST", "JUST", true},
		{spamTok, "WIN", "WINkLink", false},
		{spamTok, "WIN", "visit win-airdrop.io", true},
		{spamTok, "", "", true},
		{spamTok, "@ZAPZEE", "ZAPZEE", true},
		{spamTok, "$AFRO", "Afro", false},
		{spamTok, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "x", true},
	}
	for _, c := range cases {
		if got := spam(c.contract, c.sym, c.name); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
	for c := range canonical {
		if !Valid(c) {
			t.Errorf("canonical %s is not a valid address", c)
		}
	}
	if ascii("Ｕ SDT") || !ascii("USDT") {
		t.Error("ascii")
	}
}

func TestErrors(t *testing.T) {
	mode := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "500":
			w.WriteHeader(500)
		case "429":
			w.WriteHeader(429)
		case "403":
			w.WriteHeader(403)
			fmt.Fprint(w, `{"Error":"request rate exceeds the allowed_rps(3), and the query server is suspended for 1s"}`)
		case "401":
			w.WriteHeader(401)
			fmt.Fprint(w, `{"Error":"ApiKey not exists"}`)
		case "junk":
			fmt.Fprint(w, `not json`)
		case "refused":
			fmt.Fprint(w, `{"success":false,"error":"A valid account address is required.","statusCode":400}`)
		case "baddata":
			fmt.Fprint(w, `{"success":true,"data":{"x":1}}`)
		case "badamount":
			b, _ := json.Marshal(obj{"success": true, "data": []obj{tx("x", 1, "TransferContract", "SUCCESS", 0,
				obj{"owner_address": other, "to_address": wallet, "amount": "1e"})}})
			w.Write(b)
		}
	}))
	defer srv.Close()
	src := connect(t, srv, nil, nil)
	for _, m := range []string{"500", "429", "403", "401", "junk", "refused", "baddata", "badamount"} {
		mode = m
		err := src.Activities(context.Background(), source.Fetch{Emit: func(model.Activity) error { return nil }})
		if err == nil {
			t.Errorf("%s: no error", m)
		}
		if m == "401" && !strings.Contains(err.Error(), "ApiKey not exists") {
			t.Errorf("401: %v", err)
		}
	}
	mode = "500"
	if _, err := src.Balances(context.Background()); err == nil {
		t.Error("balances error")
	}
	if err := src.Test(context.Background()); err == nil {
		t.Error("test error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) == nil || sleepCtx(context.Background(), time.Millisecond) != nil || sleepCtx(ctx, 0) == nil {
		t.Error("sleep")
	}
}

// TestLive reads a real busy address on TronGrid (FUNDARR_LIVE_TRON=1).
func TestLive(t *testing.T) {
	if os.Getenv("FUNDARR_LIVE_TRON") != "1" {
		t.Skip("FUNDARR_LIVE_TRON=1 to call TronGrid")
	}
	addr := os.Getenv("FUNDARR_LIVE_TRON_ADDRESS")
	if addr == "" {
		addr = "TEgfk1gjsMdXZYm9743PWYuM6Fc5MGJYeq"
	}
	s, err := New(source.Config{Values: map[string]string{"address": addr, "api_key": os.Getenv("TRONGRID_API_KEY")}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	if err := s.(*Source).Test(ctx); err != nil {
		t.Fatal(err)
	}
	n := map[model.Type]int{}
	var acts []model.Activity
	err = s.Activities(ctx, source.Fetch{Emit: func(a model.Activity) error { n[a.Type]++; acts = append(acts, a); return nil },
		Progress: func(f string, a ...any) { t.Logf(f, a...) }})
	if err != nil {
		t.Fatal(err)
	}
	bal, err := s.Balances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("activities %v, balances %v", n, bal)
	for _, g := range model.Reconcile(acts, bal, decimal.Zero) {
		t.Logf("gap %+v", g)
	}
}
