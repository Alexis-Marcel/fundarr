package evm

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

const (
	wallet  = "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed" // EIP-55 test vector
	wLower  = "0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed"
	other   = "0xfb6916095ca1df60bb79ce92ce3ea74c37c5d359"
	dex     = "0xdbf03b407c01e7cd3cbea99509d93f8dddc8c6fb"
	usdcEth = "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"
	daiEth  = "0x6b175474e89094c44da98b954eedeac495271d0f"
	fakeUSD = "0x1111111111111111111111111111111111111111"
	spamTok = "0x2222222222222222222222222222222222222222"
)

type row = map[string]string

// fake serves Blockscout-shaped answers per network (and Etherscan v2).
type fake struct {
	mu          sync.Mutex
	rows        map[string]map[string][]row  // chain → action → rows
	balances    map[string]map[string]string // chain → contract ("" native) → raw
	calls       []string
	limitOne    bool
	freeRefused int
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	chain := strings.Split(strings.Trim(r.URL.Path, "/"), "/")[0]
	if chain == "v2" {
		id, _ := strconv.Atoi(q.Get("chainid"))
		for _, c := range chains {
			if c.ID == id {
				chain = c.Name
			}
		}
		if q.Get("apikey") == "FREE" && chain == "base" {
			f.freeRefused++
			fmt.Fprint(w, `{"status":"0","message":"NOTOK","result":"Free API access is not supported for this chain. Please upgrade your api plan for full chain coverage. https://etherscan.io/apis"}`)
			return
		}
		if q.Get("apikey") != "KEY" && q.Get("apikey") != "FREE" {
			fmt.Fprint(w, `{"status":"0","message":"NOTOK","result":"Invalid API Key"}`)
			return
		}
	} else if q.Get("apikey") != "" {
		http.Error(w, "the key must not reach a free explorer", 400)
		return
	}
	f.calls = append(f.calls, chain+":"+q.Get("action")+":"+q.Get("startblock")+":"+q.Get("page"))
	if f.limitOne {
		f.limitOne = false
		fmt.Fprint(w, `{"status":"0","message":"NOTOK","result":"Max rate limit reached"}`)
		return
	}
	if q.Get("module") != "account" || strings.ToLower(q.Get("address")) != wLower {
		http.Error(w, "bad request", 400)
		return
	}
	switch a := q.Get("action"); a {
	case "balance", "tokenbalance":
		v := f.balances[chain][strings.ToLower(q.Get("contractaddress"))]
		if v == "" {
			v = "0"
		}
		fmt.Fprintf(w, `{"status":"1","message":"OK","result":%q}`, v)
	default:
		all := append([]row(nil), f.rows[chain][a]...)
		sort.SliceStable(all, func(i, j int) bool { return blk(all[i]) < blk(all[j]) })
		start, _ := strconv.ParseInt(q.Get("startblock"), 10, 64)
		page, _ := strconv.Atoi(q.Get("page"))
		size, _ := strconv.Atoi(q.Get("offset"))
		var sel []row
		for _, x := range all {
			if blk(x) >= start {
				sel = append(sel, x)
			}
		}
		lo := (page - 1) * size
		if lo >= len(sel) {
			fmt.Fprint(w, `{"status":"0","message":"No transactions found","result":[]}`)
			return
		}
		hi := min(lo+size, len(sel))
		b, _ := json.Marshal(sel[lo:hi])
		fmt.Fprintf(w, `{"status":"1","message":"OK","result":%s}`, b)
	}
}

func blk(r row) int64 { n, _ := strconv.ParseInt(r["blockNumber"], 10, 64); return n }

func tx(block, hash, from, to, value, gasUsed, gasPrice, isError string) row {
	return row{"blockNumber": block, "timeStamp": strconv.Itoa(1700000000 + atoi(block)), "hash": hash, "from": from, "to": to,
		"value": value, "gasUsed": gasUsed, "gasPrice": gasPrice, "isError": isError, "txreceipt_status": "1"}
}

func tok(block, hash, from, to, value, contract, symbol, name, dec, logIndex string) row {
	return row{"blockNumber": block, "timeStamp": strconv.Itoa(1700000000 + atoi(block)), "hash": hash, "from": from, "to": to,
		"value": value, "contractAddress": contract, "tokenSymbol": symbol, "tokenName": name, "tokenDecimal": dec, "logIndex": logIndex}
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func newFake() *fake {
	return &fake{
		rows: map[string]map[string][]row{
			"ethereum": {
				"txlist": {
					tx("100", "0xa1", other, wLower, "1000000000000000000", "21000", "10000000000", "0"), // +1 ETH
					tx("200", "0xa2", wLower, other, "300000000000000000", "21000", "10000000000", "0"),  // −0.3, gas 0.00021
					tx("300", "0xa3", wLower, dex, "500000000000000000", "50000", "10000000000", "1"),    // failed: gas 0.0005 only
					tx("400", "0xa4", wLower, usdcEth, "0", "40000", "10000000000", "0"),                 // approve: gas 0.0004
					tx("450", "0xa5", wLower, wLower, "100000000000000000", "21000", "10000000000", "0"), // to self: gas 0.00021
					tx("460", "0xa6", other, wLower, "700000000000000000", "21000", "10000000000", "1"),  // incoming failed: nothing
				},
				"txlistinternal": {
					{"blockNumber": "500", "timeStamp": "1700000500", "transactionHash": "0xb1", "from": dex, "to": wLower, "value": "200000000000000000", "isError": "0", "index": "0"},
					{"blockNumber": "510", "timeStamp": "1700000510", "transactionHash": "0xb2", "from": dex, "to": wLower, "value": "900000000000000000", "isError": "1", "index": "0"},
				},
				"tokentx": {
					tok("600", "0xc1", other, wLower, "100500000", usdcEth, "USDC", "USD Coin", "6", "3"),
					tok("600", "0xc1", other, wLower, "2500000000000000000", daiEth, "DAI", "Dai Stablecoin", "18", "4"),
					tok("600", "0xc2", spamTok, wLower, "1000000000000000000000", spamTok, "Visit x.com", "Claim rewards", "18", "1"),
					tok("650", "0xc3", fakeUSD, wLower, "5000000000", fakeUSD, "USDT", "Tether USD", "6", "1"),
					tok("700", "0xc4", wLower, dex, "40000000", usdcEth, "USDC", "USD Coin", "6", "9"),
					tok("710", "0xc5", wLower, other, "0", usdcEth, "USDC", "USD Coin", "6", "2"), // address poisoning
				},
			},
			"base": {
				"txlist": {tx("10", "0xd1", other, wLower, "50000000000000000", "21000", "1000000", "0")},
			},
		},
		balances: map[string]map[string]string{
			// 1 − 0.3 − 0.00021 − 0.0005 − 0.0004 − 0.00021 + 0.2
			"ethereum": {"": "898680000000000000", usdcEth: "60500000", daiEth: "2500000000000000000", fakeUSD: "5000000000"},
			"base":     {"": "50000000000000000"},
		},
	}
}

func connect(t *testing.T, srv *httptest.Server, values map[string]string, state []byte) *Source {
	t.Helper()
	def, err := source.Get("evm")
	if err != nil {
		t.Fatal(err)
	}
	v := map[string]string{"address": wallet, "base_url": srv.URL}
	for k, x := range values {
		v[k] = x
	}
	s, err := def.New(source.Config{Values: v, HTTP: srv.Client(), State: state})
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
	srv := httptest.NewServer(f)
	defer srv.Close()
	old := pageSize
	pageSize = 2 // force startblock and page walking
	defer func() { pageSize = old }()

	s := connect(t, srv, map[string]string{"chains": "ethereum, base,ethereum"}, nil)
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
		if a.Time.IsZero() || a.Network == "" || a.Account != a.Network || a.TxID == "" {
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
	check("ethereum:0xa1:tx", model.Deposit, "ETH", "1")
	w := check("ethereum:0xa2:tx", model.Withdrawal, "ETH", "0.3")
	if w.Fee == nil || !w.Fee.Qty.Equal(decimal.RequireFromString("0.00021")) || w.Address != other {
		t.Fatalf("withdrawal fee %+v", w)
	}
	if a := check("ethereum:0xa3:tx", model.Fee, "ETH", "0.0005"); !strings.Contains(a.Note, "failed") {
		t.Fatal("failed tx note")
	}
	check("ethereum:0xa4:tx", model.Fee, "ETH", "0.0004")
	check("ethereum:0xa5:tx", model.Fee, "ETH", "0.00021")
	check("ethereum:0xb1:internal:0", model.Deposit, "ETH", "0.2")
	check("ethereum:0xc1:token:3", model.Deposit, "USDC", "100.5")
	check("ethereum:0xc1:token:4", model.Deposit, "DAI", "2.5")
	check("ethereum:0xc4:token:9", model.Withdrawal, "USDC", "40")
	check("base:0xd1:tx", model.Deposit, "ETH", "0.05")
	for id := range byID {
		if strings.Contains(id, "0xa6") || strings.Contains(id, "0xb2") || strings.Contains(id, "0xc2") || strings.Contains(id, "0xc3") || strings.Contains(id, "0xc5") {
			t.Fatalf("should be skipped: %s", id)
		}
	}
	if len(acts) != 10 {
		t.Fatalf("%d activities: %v", len(acts), keys(byID))
	}
	if !strings.Contains(strings.Join(logs, "\n"), "base: transactions") {
		t.Fatal("progress")
	}
	// Paging: the same block continued with page=2.
	paged := false
	for _, c := range f.calls {
		if strings.HasPrefix(c, "ethereum:tokentx:600:2") {
			paged = true
		}
	}
	if !paged {
		t.Fatalf("no page walk in %v", f.calls)
	}

	bal, err := s.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range bal {
		if b.Asset.Symbol == "USDT" || b.Asset.Symbol == "VISIT X.COM" {
			t.Fatalf("spam balance %+v", b)
		}
	}
	if gaps := model.Reconcile(acts, bal, decimal.Zero); len(gaps) != 0 {
		t.Fatalf("gaps %+v", gaps)
	}

	// Next run starts from the saved blocks and finds nothing new.
	state := s.State()
	var st state2
	json.Unmarshal(state, &st)
	if st.Chains["ethereum"].Blocks["tokentx"] != 710 || st.Chains["ethereum"].Blocks["txlist"] != 460 {
		t.Fatalf("state %s", state)
	}
	f.calls = nil
	s2 := connect(t, srv, map[string]string{"chains": "ethereum"}, state)
	acts2, _ := fetch(t, s2)
	for _, c := range f.calls {
		if strings.HasPrefix(c, "ethereum:txlist:0:") {
			t.Fatal("incremental run restarted from 0")
		}
	}
	if len(acts2) > 3 { // only rows of the last blocks again (deduped by the engine)
		t.Fatalf("incremental %d", len(acts2))
	}
}

type state2 struct {
	Chains map[string]struct {
		Blocks map[string]int64 `json:"blocks"`
	} `json:"chains"`
}

func keys(m map[string]model.Activity) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestEtherscanV2(t *testing.T) {
	f := newFake()
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := connect(t, srv, map[string]string{"chains": "base", "etherscan_key": "KEY"}, nil)
	acts, _ := fetch(t, s)
	if len(acts) != 1 || acts[0].NativeID != "base:0xd1:tx" {
		t.Fatalf("v2 %+v", acts)
	}
	if !strings.HasPrefix(f.calls[0], "base:") {
		t.Fatalf("chainid routing %v", f.calls)
	}
	// A free key does not cover base: its free explorer answers, once refused.
	free := connect(t, srv, map[string]string{"chains": "base", "etherscan_key": "FREE"}, nil)
	if acts, _ := fetch(t, free); len(acts) != 1 || f.freeRefused != 1 {
		t.Fatalf("free plan fallback %+v %d", acts, f.freeRefused)
	}
	bad := connect(t, srv, map[string]string{"chains": "base", "etherscan_key": "nope"}, nil)
	if err := bad.Test(context.Background()); err == nil || !strings.Contains(err.Error(), "Invalid API Key") {
		t.Fatalf("bad key: %v", err)
	}
}

func TestSettingsAndErrors(t *testing.T) {
	def, _ := source.Get("evm")
	cases := map[string]map[string]string{
		"0x followed by 40":    {"address": "0x123"},
		"hexadecimal":          {"address": "0xZZZeb6053F3E94C9b9A09f33669435E7Ef1BeAed"},
		"checksum":             {"address": "0x5AAeb6053F3E94C9b9A09f33669435E7Ef1BeAed"},
		"unknown network":      {"address": wallet, "chains": "ethereum,solana"},
		"at least one network": {"address": wallet, "chains": " , "},
	}
	for want, v := range cases {
		if _, err := def.New(source.Config{Values: v}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%v: %v", v, err)
		}
	}
	for _, a := range []string{wLower, strings.ToUpper(wLower[:2]) + strings.ToUpper(wLower[2:]), "0xfB6916095ca1df60bB79Ce92cE3Ea74c37c5d359", "0xD1220A0cf47c7B9Be7A2E6BA89F429762e7b9aDb"} {
		if err := validAddress(strings.Replace(a, "0X", "0x", 1)); err != nil {
			t.Errorf("%s: %v", a, err)
		}
	}
	if _, err := def.New(source.Config{Values: map[string]string{"address": wallet}, State: []byte("{")}); err == nil {
		t.Error("bad state")
	}
	s, err := def.New(source.Config{Values: map[string]string{"address": wallet}})
	if err != nil || s.(*Source).chains[0].Name != "ethereum" || s.(*Source).api.free(chains["ethereum"]) != "https://eth.blockscout.com/api" {
		t.Fatalf("defaults %v", err)
	}
	if err := s.Activities(context.Background(), source.Fetch{}); err == nil {
		t.Error("no emit")
	}

	// Server errors, rate limits that never lift, bad payloads.
	mode := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch mode {
		case "500":
			w.WriteHeader(500)
		case "429":
			w.WriteHeader(429)
		case "junk":
			fmt.Fprint(w, `not json`)
		case "notok":
			fmt.Fprint(w, `{"status":"0","message":"NOTOK","result":"Error! Invalid address format"}`)
		case "badresult":
			fmt.Fprint(w, `{"status":"1","message":"OK","result":{"x":1}}`)
		case "badamount":
			b, _ := json.Marshal([]row{tx("1", "0x1", other, wLower, "1e", "0", "0", "0")})
			fmt.Fprintf(w, `{"status":"1","message":"OK","result":%s}`, b)
		}
	}))
	defer srv.Close()
	src := connect(t, srv, nil, nil)
	for _, m := range []string{"500", "429", "junk", "notok", "badresult", "badamount"} {
		mode = m
		err := src.Activities(context.Background(), source.Fetch{Emit: func(model.Activity) error { return nil }})
		if err == nil {
			t.Errorf("%s: no error", m)
		}
	}
	mode = "500"
	if _, err := src.Balances(context.Background()); err == nil {
		t.Error("balances error")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) == nil || sleepCtx(context.Background(), time.Millisecond) != nil || sleepCtx(ctx, 0) == nil {
		t.Error("sleep")
	}
}

func TestSpam(t *testing.T) {
	cases := []struct {
		chain, contract, sym, name string
		want                       bool
	}{
		{"ethereum", usdcEth, "USDC", "USD Coin", false},
		{"ethereum", fakeUSD, "USDC", "USD Coin", true},
		{"base", "0x4200000000000000000000000000000000000006", "WETH", "Wrapped Ether", false},
		{"ethereum", spamTok, "LINK", "visit chainlink-claim.io", true},
		{"ethereum", spamTok, "", "", true},
		{"ethereum", spamTok, "LINK", "ChainLink Token", false},
		{"ethereum", spamTok, "ABCDEFGHIJKLMNOPQRSTUVWXYZ", "x", true},
	}
	for _, c := range cases {
		if got := spam(c.chain, c.contract, c.sym, c.name); got != c.want {
			t.Errorf("%+v: %v", c, got)
		}
	}
	if ascii("Ｕ SDT") || !ascii("USDT") {
		t.Error("ascii")
	}
}

func TestKeyOnlyNetworks(t *testing.T) {
	addr := "0x52908400098527886e0f7030069857d2e4169ee7"
	s, err := New(source.Config{Values: map[string]string{"address": addr, "chains": "all"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range s.(*Source).chains {
		if c.Name == "bsc" {
			t.Error("bsc read without a key")
		}
	}
	if _, err := New(source.Config{Values: map[string]string{"address": addr, "chains": "bsc"}}); err == nil || !strings.Contains(err.Error(), "Etherscan") {
		t.Errorf("bsc without key: %v", err)
	}
	s, _ = New(source.Config{Values: map[string]string{"address": addr, "chains": "all", "etherscan_key": "K"}})
	found := false
	for _, c := range s.(*Source).chains {
		found = found || c.Name == "bsc"
	}
	if !found {
		t.Error("bsc must be read with a key")
	}
}

func TestRoutes(t *testing.T) {
	addr := "0x52908400098527886e0f7030069857d2e4169ee7"
	names := func(s source.Source) string {
		var n []string
		for _, c := range s.(*Source).chains {
			n = append(n, c.Name)
		}
		return strings.Join(n, ",")
	}
	// Without a key: what free explorers still answer.
	s, err := New(source.Config{Values: map[string]string{"address": addr, "chains": All}})
	if err != nil || names(s) != "avalanche,ethereum,optimism" {
		t.Fatalf("free %s %v", names(s), err)
	}
	if _, err := New(source.Config{Values: map[string]string{"address": addr, "chains": "arbitrum"}}); err == nil || !strings.Contains(err.Error(), "Blockscout") {
		t.Errorf("arbitrum without key: %v", err)
	}
	// A free Blockscout key reads its networks through the PRO API.
	s, _ = New(source.Config{Values: map[string]string{"address": addr, "chains": All, "blockscout_key": "proapi_x"}})
	if names(s) != "arbitrum,avalanche,base,ethereum,gnosis,optimism,polygon" {
		t.Errorf("pro %s", names(s))
	}
	q := url.Values{}
	if base, ok := s.(*Source).api.route(chains["base"], q); !ok || base != "https://api.blockscout.com/v2/api" || q.Get("chain_id") != "8453" || q.Get("apikey") != "proapi_x" {
		t.Errorf("pro route %s %v", base, q)
	}
	q = url.Values{}
	if base, _ := s.(*Source).api.route(chains["avalanche"], q); base != chains["avalanche"].Free || q.Get("apikey") != "" {
		t.Errorf("avalanche route %s %v", base, q)
	}
}
