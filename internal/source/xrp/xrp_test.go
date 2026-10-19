package xrp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

const (
	me     = "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh"
	other  = "rnCEEqDnCuSjDsHiJrca4VYUqaMyi4vGF7"
	taker  = "rUY6QvosDnP83WMKRcyj96kC3qCSttVoyz"
	rlusd  = "rMxCKbEDwqr76QuheSUMdEGf4B9xJ8m5De"
	circle = "rGm7WCVp9gb4jZHWTEtGUr4dd74z2XuWhE"
	scam   = "rpiFwLYi6Gb1ESHYorn2QG1WU5vw2u4exQ"

	hexRLUSD = "524C555344000000000000000000000000000000"
	hexUSDC  = "5553444300000000000000000000000000000000"
	hexSOLO  = "534F4C4F00000000000000000000000000000000"
)

type obj = map[string]any

// root is an AccountRoot change (drops); prev "" makes it a CreatedNode.
func root(acct, prev, final string) obj {
	if prev == "" {
		return obj{"CreatedNode": obj{"LedgerEntryType": "AccountRoot", "NewFields": obj{"Account": acct, "Balance": final}}}
	}
	return obj{"ModifiedNode": obj{"LedgerEntryType": "AccountRoot",
		"FinalFields": obj{"Account": acct, "Balance": final}, "PreviousFields": obj{"Balance": prev}}}
}

// line is a RippleState change, its balance seen from the low account.
func line(low, high, cur, prev, final string) obj {
	bal := func(v string) obj { return obj{"currency": cur, "issuer": "rrrrrrrrrrrrrrrrrrrrBZbvji", "value": v} }
	f := obj{"Balance": bal(final), "LowLimit": obj{"currency": cur, "issuer": low, "value": "0"},
		"HighLimit": obj{"currency": cur, "issuer": high, "value": "1000000"}}
	if prev == "" {
		return obj{"CreatedNode": obj{"LedgerEntryType": "RippleState", "NewFields": f}}
	}
	return obj{"ModifiedNode": obj{"LedgerEntryType": "RippleState", "FinalFields": f, "PreviousFields": obj{"Balance": bal(prev)}}}
}

func iou(cur, issuer, v string) obj { return obj{"currency": cur, "issuer": issuer, "value": v} }

func entry(ledger int64, hash, typ, from, to, fee string, result string, extra obj, delivered any, nodes ...obj) obj {
	tx := obj{"Account": from, "TransactionType": typ, "Fee": fee, "hash": hash, "ledger_index": ledger,
		"date": 800000000 + ledger, "Flags": 0}
	if to != "" {
		tx["Destination"] = to
	}
	for k, v := range extra {
		tx[k] = v
	}
	m := obj{"TransactionResult": result, "AffectedNodes": nodes}
	if delivered != nil {
		m["delivered_amount"] = delivered
	}
	return obj{"tx": tx, "meta": m, "validated": true}
}

// fake is a JSON-RPC server holding one account's ledger.
type fake struct {
	mu       sync.Mutex
	txs      []obj
	balance  string // account_info Balance, "" = actNotFound
	lines    [][]obj
	calls    []string
	slowDown int // answers "slowDown" this many times first
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var req struct {
		Method string           `json:"method"`
		Params []map[string]any `json:"params"`
	}
	if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&req) != nil || len(req.Params) != 1 {
		http.Error(w, "bad request", 400)
		return
	}
	p := req.Params[0]
	f.calls = append(f.calls, fmt.Sprintf("%s:%v:%v", req.Method, p["ledger_index_min"], p["marker"] != nil))
	reply := func(v obj) { b, _ := json.Marshal(obj{"result": v}); w.Write(b) }
	if f.slowDown > 0 {
		f.slowDown--
		reply(obj{"status": "error", "error": "slowDown"})
		return
	}
	if p["account"] != me {
		reply(obj{"status": "error", "error": "actMalformed", "error_message": "Account malformed."})
		return
	}
	if f.balance == "" {
		reply(obj{"status": "error", "error": "actNotFound", "error_code": 19, "error_message": "Account not found."})
		return
	}
	switch req.Method {
	case "account_tx":
		if p["forward"] != true || p["api_version"] != float64(1) {
			http.Error(w, "forward history expected", 400)
			return
		}
		min := int64(p["ledger_index_min"].(float64))
		var sel []obj
		for _, t := range f.txs {
			if l := t["tx"].(obj)["ledger_index"].(int64); min < 0 || l >= min {
				sel = append(sel, t)
			}
		}
		start := 0
		if m, ok := p["marker"].(map[string]any); ok {
			start = int(m["seq"].(float64))
		}
		size := int(p["limit"].(float64))
		end := min2(start+size, len(sel))
		res := obj{"account": me, "transactions": sel[start:end], "validated": true, "status": "success"}
		if end < len(sel) {
			res["marker"] = obj{"ledger": sel[end]["tx"].(obj)["ledger_index"], "seq": end}
		}
		reply(res)
	case "account_info":
		reply(obj{"account_data": obj{"Account": me, "Balance": f.balance}, "validated": true, "status": "success"})
	case "account_lines":
		page := 0
		if m, ok := p["marker"].(string); ok {
			fmt.Sscanf(m, "page%d", &page)
		}
		res := obj{"account": me, "lines": f.lines[page], "status": "success"}
		if page+1 < len(f.lines) {
			res["marker"] = fmt.Sprintf("page%d", page+1)
		}
		reply(res)
	default:
		reply(obj{"status": "error", "error": "unknownCmd"})
	}
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func newFake() *fake {
	partial := obj{"Flags": tfPartialPayment, "Amount": iou(hexRLUSD, rlusd, "1000")}
	unvalidated := entry(950, "H11", "Payment", other, me, "12", "tesSUCCESS", nil, "5000000", root(me, "86999940", "91999940"))
	unvalidated["validated"] = false
	return &fake{
		txs: []obj{
			// +100 XRP, the account is created.
			entry(100, "H1", "Payment", other, me, "12", "tesSUCCESS", obj{"Amount": "100000000"}, "100000000",
				root(other, "500000000", "399999988"), root(me, "", "100000000")),
			// −30 XRP to other, fee 12 drops.
			entry(200, "H2", "Payment", me, other, "12", "tesSUCCESS", obj{"Amount": "30000000"}, "30000000",
				root(me, "100000000", "69999988"), root(other, "399999988", "429999988")),
			// Failed: only the fee.
			entry(300, "H3", "Payment", me, other, "12", "tecPATH_DRY", obj{"Amount": iou(hexRLUSD, rlusd, "5")}, nil,
				root(me, "69999988", "69999976")),
			// Trust line to RLUSD: only the fee.
			entry(400, "H4", "TrustSet", me, "", "12", "tesSUCCESS", nil, nil,
				root(me, "69999976", "69999964"), line(me, rlusd, hexRLUSD, "", "0")),
			// Partial payment: 1000 RLUSD asked, 50 delivered.
			entry(500, "H5", "Payment", other, me, "12", "tesSUCCESS", partial, iou(hexRLUSD, rlusd, "50"),
				line(me, rlusd, hexRLUSD, "0", "50"), root(other, "429999988", "429999976")),
			// The account's offer: 10 RLUSD sold for 20 XRP.
			entry(600, "H6", "OfferCreate", me, "", "12", "tesSUCCESS", obj{"TakerGets": iou(hexRLUSD, rlusd, "10"), "TakerPays": "20000000"}, nil,
				root(me, "69999964", "89999952"), line(me, rlusd, hexRLUSD, "50", "40")),
			// Someone fills the account's offer: 3 XRP for 5 USDC (the
			// account is the high side of that line).
			entry(700, "H7", "OfferCreate", taker, "", "15", "tesSUCCESS", nil, nil,
				root(me, "89999952", "86999952"), line(circle, me, hexUSDC, "0", "-5"), root(taker, "1000000000", "1002999985")),
			// Spam: a counterfeit USD airdrop, nothing counts.
			entry(800, "H8", "Payment", scam, me, "12", "tesSUCCESS", obj{"Amount": iou("USD", scam, "1000")}, iou("USD", scam, "1000"),
				line(me, scam, "USD", "", "1000")),
			// A readable token from an unknown issuer counts.
			entry(800, "H9", "Payment", scam, me, "12", "tesSUCCESS", obj{"Amount": iou(hexSOLO, scam, "7")}, iou(hexSOLO, scam, "7"),
				line(me, scam, hexSOLO, "0", "7")),
			// Payment to oneself: only the fee.
			entry(900, "H10", "Payment", me, me, "12", "tesSUCCESS", obj{"Amount": "1000000"}, "1000000",
				root(me, "86999952", "86999940")),
			unvalidated,
		},
		balance: "86999940",
		lines: [][]obj{
			{{"account": rlusd, "balance": "40", "currency": hexRLUSD}, {"account": scam, "balance": "1000", "currency": "USD"}},
			{{"account": circle, "balance": "5", "currency": hexUSDC}, {"account": scam, "balance": "7", "currency": hexSOLO},
				{"account": "rrrrrrrrrrrrrrrrrrrrrhoLvTp", "balance": "-2", "currency": "EUR"}},
		},
	}
}

func connect(t *testing.T, srv *httptest.Server, state []byte) *Source {
	t.Helper()
	s, err := New(source.Config{Values: map[string]string{"address": me, "base_url": srv.URL}, HTTP: srv.Client(), State: state})
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

func TestAccountHistory(t *testing.T) {
	f := newFake()
	f.slowDown = 1
	srv := httptest.NewServer(f)
	defer srv.Close()
	old := pageSize
	pageSize = 3 // force marker pagination
	defer func() { pageSize = old }()

	s := connect(t, srv, nil)
	if err := s.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	acts, _ := fetch(t, s)
	byID := map[string]model.Activity{}
	for _, a := range acts {
		if _, dup := byID[a.NativeID]; dup {
			t.Fatalf("duplicate %s", a.NativeID)
		}
		byID[a.NativeID] = a
		if a.Time.IsZero() || a.Network != "xrp" || a.TxID == "" || !strings.HasPrefix(a.NativeID, a.TxID+":") {
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
	if a := check("H1:XRP", model.Deposit, "XRP", "100"); a.Address != other || a.Fee != nil ||
		!a.Time.Equal(time.Unix(946684800+800000100, 0).UTC()) || a.Time.Location() != time.UTC {
		t.Fatalf("receive %+v", a)
	}
	if a := check("H2:XRP", model.Withdrawal, "XRP", "30"); a.Fee == nil || !a.Fee.Qty.Equal(decimal.RequireFromString("0.000012")) || a.Address != other {
		t.Fatalf("send %+v", a)
	}
	if a := check("H3:fee", model.Fee, "XRP", "0.000012"); !strings.Contains(a.Note, "tecPATH_DRY") {
		t.Fatalf("failed tx %+v", a)
	}
	check("H4:fee", model.Fee, "XRP", "0.000012")
	if a := check("H5:RLUSD", model.Deposit, "RLUSD", "50"); a.Note != "partial payment" {
		t.Fatalf("partial %+v", a)
	}
	check("H6:XRP", model.Deposit, "XRP", "20")
	if a := check("H6:RLUSD", model.Withdrawal, "RLUSD", "10"); a.Fee == nil || a.Note != "OfferCreate" {
		t.Fatalf("trade fee %+v", a)
	}
	check("H7:XRP", model.Withdrawal, "XRP", "3")
	if a := check("H7:USDC", model.Deposit, "USDC", "5"); a.Fee != nil || a.Address != taker {
		t.Fatalf("filled offer %+v", a)
	}
	if a := check("H9:SOLO."+scam, model.Deposit, "SOLO", "7"); !strings.Contains(a.Note, scam) {
		t.Fatalf("unknown issuer note %+v", a)
	}
	check("H10:fee", model.Fee, "XRP", "0.000012")
	for id := range byID {
		if strings.HasPrefix(id, "H8:") || strings.HasPrefix(id, "H11:") || strings.HasPrefix(id, "H7:fee") {
			t.Fatalf("should be skipped: %s", id)
		}
	}
	if len(acts) != 11 {
		t.Fatalf("%d activities: %v", len(acts), keys(byID))
	}
	paged := 0
	for _, c := range f.calls {
		if strings.HasPrefix(c, "account_tx:") && strings.HasSuffix(c, ":true") {
			paged++
		}
	}
	if paged != 3 {
		t.Fatalf("marker pages %d in %v", paged, f.calls)
	}

	bal, err := s.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, b := range bal {
		got[b.Asset.Symbol] = b.Qty.String()
	}
	want := map[string]string{"XRP": "86.99994", "RLUSD": "40", "USDC": "5", "SOLO": "7"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("balances %v", got)
	}
	if gaps := model.Reconcile(acts, bal, decimal.Zero); len(gaps) != 0 {
		t.Fatalf("gaps %+v", gaps)
	}

	// Next run starts at the last ledger seen.
	state := s.State()
	if string(state) != `{"ledger":900}` {
		t.Fatalf("state %s", state)
	}
	f.calls = nil
	s2 := connect(t, srv, state)
	acts2, logs := fetch(t, s2)
	if len(acts2) != 1 || acts2[0].NativeID != "H10:fee" || !strings.HasPrefix(f.calls[0], "account_tx:900:") {
		t.Fatalf("incremental %+v %v", acts2, f.calls)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "from ledger 900") {
		t.Fatal("progress")
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

func TestDeleteAndCrossCurrency(t *testing.T) {
	f := newFake()
	f.txs = []obj{
		// A cross-currency payment out: 12 RLUSD spent, other receives XRP.
		entry(10, "C1", "Payment", me, other, "10", "tesSUCCESS", obj{"Amount": "5000000", "SendMax": iou(hexRLUSD, rlusd, "15")}, "5000000",
			root(me, "50000000", "49999990"), line(me, rlusd, hexRLUSD, "12", "0"), root(other, "1", "5000001")),
		// AccountDelete: what is left goes to other (fee 0.2 XRP).
		entry(20, "C2", "AccountDelete", me, other, "200000", "tesSUCCESS", nil, "49799990",
			obj{"DeletedNode": obj{"LedgerEntryType": "AccountRoot", "FinalFields": obj{"Account": me, "Balance": "0"},
				"PreviousFields": obj{"Balance": "49999990"}}}, root(other, "5000001", "54799991")),
	}
	srv := httptest.NewServer(f)
	defer srv.Close()
	acts, _ := fetch(t, connect(t, srv, nil))
	if len(acts) != 2 {
		t.Fatalf("%+v", acts)
	}
	if a := acts[0]; a.NativeID != "C1:RLUSD" || a.Type != model.Withdrawal || a.Base.Qty.String() != "12" || a.Fee == nil || a.Address != other {
		t.Fatalf("cross-currency %+v", a)
	}
	if a := acts[1]; a.NativeID != "C2:XRP" || a.Type != model.Withdrawal || a.Base.Qty.String() != "49.79999" ||
		a.Fee.Qty.String() != "0.2" || a.Address != other || a.Note != "AccountDelete" {
		t.Fatalf("delete %+v", a)
	}
}

func TestUnfunded(t *testing.T) {
	f := newFake()
	f.balance = ""
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := connect(t, srv, nil)
	if err := s.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	if acts, _ := fetch(t, s); len(acts) != 0 {
		t.Fatalf("%+v", acts)
	}
	if bal, err := s.Balances(context.Background()); err != nil || len(bal) != 0 {
		t.Fatalf("%v %v", bal, err)
	}
}

func TestValid(t *testing.T) {
	for _, a := range []string{me, rlusd, circle, "rrrrrrrrrrrrrrrrrrrrrhoLvTp", "rrrrrrrrrrrrrrrrrrrrBZbvji"} {
		if !Valid(a) {
			t.Errorf("%s should be valid", a)
		}
	}
	for _, a := range []string{"", "r", me[:len(me)-1] + "i", "rHb9CJAWyB4rj91VRWn96DkukG4bwdtyT", "1Hb9CJAWyB4rj91VRWn96DkukG4bwdtyTh",
		"rHb9CJAWyB4rj91VRWn96DkukG4bwdty0h", "X7AcgcsBL6XDcUb289X4mJ8djcdyKaB5hJDWMArnXr61cqZ",
		"0x5aaeb6053f3e94c9b9a09f33669435e7ef1beaed", "bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq"} {
		if Valid(a) {
			t.Errorf("%s should be invalid", a)
		}
	}
	if _, err := New(source.Config{Values: map[string]string{"address": me[:len(me)-1] + "i"}}); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("bad checksum: %v", err)
	}
	if _, err := New(source.Config{Values: map[string]string{"address": me}, State: []byte("{")}); err == nil {
		t.Error("bad state")
	}
	s, err := New(source.Config{Values: map[string]string{"address": " " + me + " "}})
	if err != nil || s.(*Source).api.base != servers[0] || len(s.(*Source).api.fallback) != 2 {
		t.Fatalf("defaults %v", err)
	}
	if err := s.Activities(context.Background(), source.Fetch{}); err == nil {
		t.Error("no emit")
	}
}

func TestClassify(t *testing.T) {
	cases := []struct {
		cur, issuer, sym string
	}{
		{hexRLUSD, rlusd, "RLUSD"},
		{hexUSDC, circle, "USDC"},
		{"USD", "rvYAfWj5gh67oV6fW32ZzP3Aw4Eubs59B", "USD"},
		{hexRLUSD, scam, ""}, // counterfeit
		{hexUSDC, scam, ""},
		{"USD", scam, ""},
		{"EUR", scam, ""},
		{"XRP", scam, ""},
		{"5553445400000000000000000000000000000000", scam, ""}, // USDT
		{hexSOLO, scam, "SOLO"},
		{"CSC", scam, "CSC"},
		{"03B6D1A8A0D4A1C9E0B3F2A7C1E0D4B5A6C7D8E9", scam, ""}, // AMM LP token
		{"0000000000000000000000004254430000000000", scam, ""}, // BTC, standard format in hex
		{"5669736974207777772E78797A00000000000000", scam, ""}, // "Visit www.xyz"
		{"zz", scam, "ZZ"},
	}
	for _, c := range cases {
		tk := classify(c.cur, c.issuer)
		if tk.Symbol != c.sym || tk.Spam != (c.sym == "") {
			t.Errorf("%s/%s: %+v", c.cur, c.issuer, tk)
		}
	}
	if currencyCode("0000000000000000000000004555520000000000") != "EUR" || currencyCode("ZZ") != "ZZ" || currencyCode(strings.Repeat("G", 40)) != "" {
		t.Error("currencyCode")
	}
}

func TestRetriesAndFailover(t *testing.T) {
	mode := ""
	var hits int
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		switch mode {
		case "500":
			w.WriteHeader(500)
		case "429":
			w.WriteHeader(429)
		case "junk":
			fmt.Fprint(w, `not json`)
		case "malformed":
			fmt.Fprint(w, `{"result":{"status":"error","error":"actMalformed","error_message":"Account malformed."}}`)
		case "badresult":
			fmt.Fprint(w, `{"result":{"status":"success","transactions":{"x":1}}}`)
		case "slow":
			fmt.Fprint(w, `{"result":{"status":"error","error":"slowDown"}}`)
		}
	}))
	defer bad.Close()
	good := httptest.NewServer(newFake())
	defer good.Close()

	src := connect(t, bad, nil)
	for _, m := range []string{"500", "429", "junk", "malformed", "badresult", "slow"} {
		mode = m
		err := src.Activities(context.Background(), source.Fetch{Emit: func(model.Activity) error { return nil }})
		if err == nil {
			t.Errorf("%s: no error", m)
		}
		if m == "malformed" && !strings.Contains(err.Error(), "Account malformed") {
			t.Errorf("message: %v", err)
		}
	}
	if _, err := src.Balances(context.Background()); err == nil {
		t.Error("balances error")
	}

	// The default list moves on to the next server when one keeps failing.
	mode, hits = "500", 0
	src.api.base, src.api.fallback = bad.URL, []string{good.URL}
	acts, _ := fetch(t, src)
	if len(acts) != 11 || hits != 2 || src.api.base != good.URL {
		t.Fatalf("failover: %d activities, %d hits on the bad server", len(acts), hits)
	}
	// A failing Emit stops the run.
	stop := errors.New("stop")
	if err := src.Activities(context.Background(), source.Fetch{Emit: func(model.Activity) error { return stop }}); !errors.Is(err, stop) {
		t.Errorf("emit error: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) == nil || sleepCtx(context.Background(), time.Millisecond) != nil || sleepCtx(ctx, 0) == nil {
		t.Error("sleep")
	}
}

// TestLive reads a real account on the public servers (FUNDARR_LIVE_XRP=1).
func TestLive(t *testing.T) {
	if os.Getenv("FUNDARR_LIVE_XRP") != "1" {
		t.Skip("set FUNDARR_LIVE_XRP=1 to read the XRP Ledger")
	}
	addr := os.Getenv("FUNDARR_XRP_ADDRESS")
	if addr == "" {
		addr = rlusd
	}
	s, err := New(source.Config{Values: map[string]string{"address": addr}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.(source.Tester).Test(ctx); err != nil {
		t.Fatal(err)
	}
	bal, err := s.Balances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("balances %v", bal)
	stop := errors.New("enough")
	n := 0
	err = s.Activities(ctx, source.Fetch{Emit: func(a model.Activity) error {
		t.Logf("%s %s %s %s %s %s", a.Time.Format(time.DateTime), a.NativeID, a.Type, a.Base.Qty, a.Base.Asset, a.Note)
		if n++; n >= 20 {
			return stop
		}
		return nil
	}})
	if err != nil && !errors.Is(err, stop) {
		t.Fatal(err)
	}
}
