package solana

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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

// Real mainnet addresses, used only as well-formed keys.
const (
	owner    = "9WzDXwBbmkg8ZTbNMqUxvQRAyrZzDsGYdLVL9zYtAWWM"
	other    = "DmwcZyBvy2adBm7nwxt9vvDQonbe99nZebyFfJkRKNqY"
	pool     = "wJG2h48defZNLtcEcX9paJPXLFrvgW3HQUnKmoD7gfq"
	usdcATA  = "DdHs2sMpW9ujkCeg1Nud4jon6DhRJdPkPNpabHriBGFx" // the owner's USDC account
	otherATA = "BA3nbk1kxkd4ngfK3gRU7fGaL9rRAN7JRSN1QWoN7X9H"
	poolATA  = "7mFLeHbzNxaYA89SuFtysdWXSZbHokGcTZe5vGKTdRiA"
	goodATA  = "ENWRZf4YG7AFtgCn1a6mXj8xZV1MvZsPeHQTPD1vYAPD" // the owner's GOOD account (Token-2022)
	spamATA  = "AyZ3brL2ErYotc8FSrNXjSdSojwAe7A5yA5RMBL8CD3Z" // closed since: only seen in a transaction
	fakeATA  = "F2gLsLzwLBPHkT6metp3N1xkXsuF82S4dzT8jghzKZ6c"
	wsolATA  = "85WTujfJ9meJq5hfjAeb5gftj7n8Q7QTsZJbRqMD5ERS"
	usdc     = "EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v"
	goodMint = "96HiV4cGWTJNCjGVff3RTHgPXmpYz7MSrGTAmxNKVWM9" // verified on the fake Jupiter as GOOD
	spamMint = "3PEMevfCoBCJegTknTTHjxwgTBP9piwkYZXwyzzXpump" // unverified
	fakeUSDC = "ERRGqu3dh6zYBg7MNAHKL33TyVb7efMmaKxnmdukdNYa" // verified, but calls itself USDC
)

type obj = map[string]any

// fake serves JSON-RPC and Jupiter's token search.
type fake struct {
	mu       sync.Mutex
	sigs     map[string][]string // address → signatures, newest first
	txs      map[string]obj
	accounts map[string][]obj // program → token accounts
	lamports int64
	calls    []string
	limitOne bool
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.URL.Path == "/tokens/v2/search" {
		f.calls = append(f.calls, "jupiter:"+r.URL.Query().Get("query"))
		var out []obj
		for _, m := range strings.Split(r.URL.Query().Get("query"), ",") {
			switch m {
			case goodMint:
				out = append(out, obj{"id": m, "symbol": "Good", "isVerified": true, "tags": []string{"verified"}})
			case spamMint:
				out = append(out, obj{"id": m, "symbol": "PUMPTEST", "tags": []string{"unknown"}})
			case fakeUSDC:
				out = append(out, obj{"id": m, "symbol": "USDC", "isVerified": true})
			}
		}
		json.NewEncoder(w).Encode(out)
		return
	}
	body, _ := io.ReadAll(r.Body)
	var req struct {
		ID     int               `json:"id"`
		Method string            `json:"method"`
		Params []json.RawMessage `json:"params"`
	}
	if r.Method != http.MethodPost || json.Unmarshal(body, &req) != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if f.limitOne {
		f.limitOne = false
		w.WriteHeader(http.StatusTooManyRequests)
		fmt.Fprint(w, `{"jsonrpc":"2.0","error":{"code":429,"message":"Too many requests"},"id":1}`)
		return
	}
	var first string
	_ = json.Unmarshal(req.Params[0], &first)
	var result any
	switch req.Method {
	case "getBalance":
		f.calls = append(f.calls, "getBalance")
		result = obj{"context": obj{"slot": 1}, "value": f.lamports}
	case "getTokenAccountsByOwner":
		var p struct {
			ProgramID string `json:"programId"`
		}
		_ = json.Unmarshal(req.Params[1], &p)
		f.calls = append(f.calls, "accounts:"+p.ProgramID)
		result = obj{"context": obj{"slot": 1}, "value": append([]obj{}, f.accounts[p.ProgramID]...)}
	case "getSignaturesForAddress":
		var o struct {
			Limit         int
			Before, Until string
		}
		_ = json.Unmarshal(req.Params[1], &o)
		f.calls = append(f.calls, "sigs:"+first+":"+o.Before+":"+o.Until)
		var page []obj
		on := o.Before == ""
		for _, s := range f.sigs[first] {
			if s == o.Until {
				break
			}
			if on && len(page) < o.Limit {
				page = append(page, obj{"signature": s, "slot": f.txs[s]["slot"], "err": nil})
			}
			if s == o.Before {
				on = true
			}
		}
		result = append([]obj{}, page...)
	case "getTransaction":
		f.calls = append(f.calls, "tx:"+first)
		var o struct {
			Max int `json:"maxSupportedTransactionVersion"`
		}
		_ = json.Unmarshal(req.Params[1], &o)
		if t, ok := f.txs[first]; ok {
			if v, _ := t["version"].(int); v > o.Max {
				fmt.Fprintf(w, `{"jsonrpc":"2.0","error":{"code":-32015,"message":"Transaction version (%d) is not supported by the requesting client. Please try the request again with the following configuration parameter: \"maxSupportedTransactionVersion\": %d"},"id":%d}`, v, v, req.ID)
				return
			}
			result = t
		}
	default:
		fmt.Fprintf(w, `{"jsonrpc":"2.0","error":{"code":-32601,"message":"Method not found"},"id":%d}`, req.ID)
		return
	}
	json.NewEncoder(w).Encode(obj{"jsonrpc": "2.0", "id": req.ID, "result": result})
}

// key is an account of a transaction with its lamports before and after.
type key struct {
	pubkey    string
	pre, post int64
}

// tok is a token account's balance before and after ("" when absent).
type tok struct {
	index            int
	mint, owner      string
	pre, post        string
	decimals         int
	programIsToken22 bool
}

func tx(slot int64, fee int64, failed bool, keys []key, toks []tok, ins ...obj) obj {
	var ak []obj
	var pre, post []int64
	for i, k := range keys {
		ak = append(ak, obj{"pubkey": k.pubkey, "signer": i == 0, "writable": true, "source": "transaction"})
		pre, post = append(pre, k.pre), append(post, k.post)
	}
	bal := func(t tok, amt string) obj {
		return obj{"accountIndex": t.index, "mint": t.mint, "owner": t.owner, "programId": tokenProgram,
			"uiTokenAmount": obj{"amount": amt, "decimals": t.decimals, "uiAmountString": amt}}
	}
	preT, postT := []obj{}, []obj{}
	for _, t := range toks {
		if t.pre != "" {
			preT = append(preT, bal(t, t.pre))
		}
		if t.post != "" {
			postT = append(postT, bal(t, t.post))
		}
	}
	var errv any
	if failed {
		errv = obj{"InstructionError": []any{0, obj{"Custom": 1}}}
	}
	if ins == nil {
		ins = []obj{}
	}
	return obj{"slot": slot, "blockTime": 1700000000 + slot, "version": 0,
		"meta": obj{"err": errv, "fee": fee, "preBalances": pre, "postBalances": post,
			"preTokenBalances": preT, "postTokenBalances": postT, "innerInstructions": []obj{}},
		"transaction": obj{"message": obj{"accountKeys": ak, "instructions": ins}, "signatures": []string{"x"}}}
}

func transfer(from, to string, lamports int64) obj {
	return obj{"program": "system", "programId": systemProgram, "stackHeight": 1,
		"parsed": obj{"type": "transfer", "info": obj{"source": from, "destination": to, "lamports": lamports}}}
}

func account(pubkey, mint, amount string, dec int) obj {
	return obj{"pubkey": pubkey, "account": obj{"lamports": 2039280, "data": obj{"program": "spl-token",
		"parsed": obj{"type": "account", "info": obj{"mint": mint, "owner": owner, "state": "initialized",
			"tokenAmount": obj{"amount": amount, "decimals": dec}}}}}}
}

const sol1 = 1_000_000_000

func newFake() *fake {
	return &fake{
		sigs: map[string][]string{
			owner:   {"s6", "s5", "s4", "s2", "s1"},
			usdcATA: {"s4", "s3"},
			spamATA: {"s6"},
		},
		txs: map[string]obj{
			// Receive 2 SOL; the sender pays the fee.
			"s1": tx(10, 5000, false, []key{{other, 5 * sol1, 3*sol1 - 5000}, {owner, 1 * sol1, 3 * sol1}, {systemProgram, 1, 1}}, nil,
				transfer(other, owner, 2*sol1)),
			// Send 1 SOL, fee 0.000005.
			"s2": tx(20, 5000, false, []key{{owner, 3 * sol1, 2*sol1 - 5000}, {other, 0, sol1}, {systemProgram, 1, 1}}, nil,
				transfer(owner, other, sol1)),
			// 50 USDC into the owner's token account: the owner's key is absent.
			"s3": tx(30, 5000, false, []key{{other, sol1, sol1 - 5000}, {otherATA, 2039280, 2039280}, {usdcATA, 2039280, 2039280}}, []tok{
				{index: 1, mint: usdc, owner: other, pre: "100000000", post: "50000000", decimals: 6},
				{index: 2, mint: usdc, owner: owner, pre: "0", post: "50000000", decimals: 6},
			}),
			// Swap 0.5 SOL for 70 USDC and 12.5 GOOD.
			"s4": tx(40, 10000, false, []key{{owner, 2*sol1 - 5000, 1.5*sol1 - 15000}, {pool, 100 * sol1, 100.5 * sol1}, {usdcATA, 2039280, 2039280},
				{poolATA, 2039280, 2039280}, {goodATA, 0, 0}}, []tok{
				{index: 2, mint: usdc, owner: owner, pre: "50000000", post: "120000000", decimals: 6},
				{index: 3, mint: usdc, owner: pool, pre: "1000000000", post: "930000000", decimals: 6},
				{index: 4, mint: goodMint, owner: owner, pre: "0", post: "12500", decimals: 3},
			}),
			// Failed: only the fee went.
			"s5": tx(50, 7000, true, []key{{owner, 1.5*sol1 - 15000, 1.5*sol1 - 22000}, {pool, 0, 0}}, nil),
			// Spam airdrop, with a counterfeit USDC; someone else pays.
			"s6": tx(60, 5000, false, []key{{other, sol1, sol1 - 5000}, {owner, 1.5*sol1 - 22000, 1.5*sol1 - 22000}, {spamATA, 0, 2039280}, {fakeATA, 0, 2039280}}, []tok{
				{index: 2, mint: spamMint, owner: owner, post: "1000000000", decimals: 6},
				{index: 3, mint: fakeUSDC, owner: owner, post: "5000000000", decimals: 6},
			}),
		},
		accounts: map[string][]obj{
			tokenProgram: {
				account(usdcATA, usdc, "120000000", 6),
				account(fakeATA, fakeUSDC, "5000000000", 6),
				account(wsolATA, wrappedSOL, "250000000", 9),
				account(otherATA, usdc, "0", 6),
			},
			token2022: {account(goodATA, goodMint, "12500", 3)},
		},
		lamports: 1.5*sol1 - 22000,
	}
}

func connect(t *testing.T, srv *httptest.Server, state []byte) *Source {
	t.Helper()
	s, err := New(source.Config{Values: map[string]string{"address": owner, "base_url": srv.URL}, HTTP: srv.Client(), State: state})
	if err != nil {
		t.Fatal(err)
	}
	src := s.(*Source)
	src.api.pause = 0
	src.api.sleep = func(context.Context, time.Duration) error { return nil }
	return src
}

func fetch(t *testing.T, s *Source) []model.Activity {
	t.Helper()
	var acts []model.Activity
	err := s.Activities(context.Background(), source.Fetch{Emit: func(a model.Activity) error { acts = append(acts, a); return nil }})
	if err != nil {
		t.Fatal(err)
	}
	return acts
}

func byID(t *testing.T, acts []model.Activity) map[string]model.Activity {
	t.Helper()
	m := map[string]model.Activity{}
	for _, a := range acts {
		if _, dup := m[a.NativeID]; dup {
			t.Fatalf("duplicate %s", a.NativeID)
		}
		if a.Time.IsZero() || a.Network != network || a.TxID == "" {
			t.Fatalf("incomplete %+v", a)
		}
		m[a.NativeID] = a
	}
	return m
}

func ids(m map[string]model.Activity) []string {
	var k []string
	for id := range m {
		k = append(k, id)
	}
	sort.Strings(k)
	return k
}

func TestHistory(t *testing.T) {
	f := newFake()
	f.limitOne = true
	f.txs["s3"]["version"] = 2 // newer than asked: the RPC says which to ask
	srv := httptest.NewServer(f)
	defer srv.Close()
	old := pageSize
	pageSize = 2 // the owner's signatures take three pages
	defer func() { pageSize = old }()

	s := connect(t, srv, nil)
	got := byID(t, fetch(t, s))
	check := func(id string, typ model.Type, sym, qty, addr string) model.Activity {
		t.Helper()
		a, ok := got[id]
		if !ok {
			t.Fatalf("missing %s in %v", id, ids(got))
		}
		if a.Type != typ || a.Base.Asset.Symbol != sym || !a.Base.Qty.Equal(decimal.RequireFromString(qty)) || a.Address != addr {
			t.Fatalf("%s = %s %s %s from/to %q", id, a.Type, a.Base.Qty, a.Base.Asset.Symbol, a.Address)
		}
		return a
	}
	if a := check("s1:SOL", model.Deposit, "SOL", "2", other); a.Fee != nil || !a.Time.Equal(time.Unix(1700000010, 0)) {
		t.Fatalf("receive %+v", a)
	}
	if a := check("s2:SOL", model.Withdrawal, "SOL", "1", other); a.Fee == nil || !a.Fee.Qty.Equal(decimal.RequireFromString("0.000005")) {
		t.Fatalf("send fee %+v", a.Fee)
	}
	check("s3:"+usdc, model.Deposit, "USDC", "50", other)
	if a := check("s4:SOL", model.Withdrawal, "SOL", "0.5", ""); a.Fee == nil || !a.Fee.Qty.Equal(decimal.RequireFromString("0.00001")) {
		t.Fatalf("swap fee %+v", a.Fee)
	}
	check("s4:"+usdc, model.Deposit, "USDC", "70", pool)
	if a := check("s4:"+goodMint, model.Deposit, "GOOD", "12.5", ""); !strings.Contains(a.Note, goodMint) {
		t.Fatalf("note %q", a.Note)
	}
	if a := check("s5:fee", model.Fee, "SOL", "0.000007", ""); !strings.Contains(a.Note, "failed") {
		t.Fatal("failed tx note")
	}
	if len(got) != 7 {
		t.Fatalf("%d activities: %v", len(got), ids(got))
	}

	// Every page, the token accounts, and the closed one found in s6.
	calls := strings.Join(f.calls, "\n")
	for _, want := range []string{"sigs:" + owner + "::", "sigs:" + owner + ":s5:", "sigs:" + owner + ":s2:", "sigs:" + usdcATA + "::",
		"sigs:" + goodATA + "::", "sigs:" + spamATA + "::", "accounts:" + token2022} {
		if !strings.Contains(calls, want) {
			t.Fatalf("no %q in\n%s", want, calls)
		}
	}
	if strings.Count(calls, "tx:s4") != 1 {
		t.Fatal("s4 fetched twice")
	}
	if strings.Count(calls, "tx:s3") != 2 {
		t.Fatal("s3 not asked again with its version")
	}

	// Next run: only the new transaction.
	f.mu.Lock()
	f.sigs[owner] = append([]string{"s7"}, f.sigs[owner]...)
	f.txs["s7"] = tx(70, 5000, false, []key{{other, sol1, sol1 / 2}, {owner, 0, sol1/2 - 5000 + 5000}}, nil)
	f.calls = nil
	f.mu.Unlock()
	s2 := connect(t, srv, s.State())
	got = byID(t, fetch(t, s2))
	if len(got) != 1 {
		t.Fatalf("second run: %v", ids(got))
	}
	check("s7:SOL", model.Deposit, "SOL", "0.5", "")
	calls = strings.Join(f.calls, "\n")
	if strings.Count(calls, "tx:") != 1 || !strings.Contains(calls, "sigs:"+owner+"::s6") || strings.Contains(calls, "jupiter:") {
		t.Fatalf("second run calls:\n%s", calls)
	}
}

func TestBalances(t *testing.T) {
	srv := httptest.NewServer(newFake())
	defer srv.Close()
	s := connect(t, srv, nil)
	if err := s.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	bals, err := s.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, b := range bals {
		got = append(got, b.Asset.Symbol+"="+b.Qty.String())
	}
	// SOL: 1.499978 held + 0.25 wrapped; the counterfeit USDC is ignored.
	if want := "GOOD=12.5 SOL=1.749978 USDC=120"; strings.Join(got, " ") != want {
		t.Fatalf("balances %v, want %s", got, want)
	}
}

func TestValid(t *testing.T) {
	for _, a := range []string{owner, usdc, systemProgram, wrappedSOL, "mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So"} {
		if !Valid(a) {
			t.Errorf("%s should be valid", a)
		}
	}
	for _, a := range []string{"", "1A1zP1eP5QGefi2DMPTfTL5SLmv7DivfNa", "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t",
		"rHb9CJAWyB4rj91VRWn96DkukG4bwdtyTh", "0x5aAeb6053F3E94C9b9A09f33669435E7Ef1BeAed",
		"bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq", owner[:len(owner)-1] + "0", owner + "1"} {
		if Valid(a) {
			t.Errorf("%s should be invalid", a)
		}
	}
	if _, err := New(source.Config{Values: map[string]string{"address": "TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t"}}); err == nil {
		t.Fatal("Tron address accepted")
	}
}

// TestLive reads a real wallet: FUNDARR_LIVE_SOLANA=1 go test -run Live -v
func TestLive(t *testing.T) {
	if os.Getenv("FUNDARR_LIVE_SOLANA") != "1" {
		t.Skip("set FUNDARR_LIVE_SOLANA=1")
	}
	addr := os.Getenv("FUNDARR_SOLANA_ADDRESS")
	if addr == "" {
		addr = "vines1vzrYbzLMRdu58ou5XTby4qAqVRLmqo36NKPTg" // small, old devnet-era wallet
	}
	s, err := New(source.Config{Values: map[string]string{"address": addr}})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 9*time.Minute)
	defer cancel()
	n := 0
	net := decimal.Zero // SOL by the history, to compare with the balance
	err = s.Activities(ctx, source.Fetch{
		Emit: func(a model.Activity) error {
			n++
			if a.Base.Asset.Symbol == "SOL" {
				if a.Inflow() {
					net = net.Add(a.Base.Qty)
				} else {
					net = net.Sub(a.Base.Qty)
				}
			}
			if a.Fee != nil {
				net = net.Sub(a.Fee.Qty)
			}
			if n <= 20 {
				t.Logf("%s %s %s %s %s", a.Time.Format(time.DateTime), a.Type, a.Base.Qty, a.Base.Asset, a.NativeID)
			}
			return nil
		},
		Progress: func(f string, a ...any) { t.Logf(f, a...) },
	})
	if err != nil {
		t.Fatal(err)
	}
	bals, err := s.Balances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("%d activities, SOL by history %s, balances %v", n, net, bals)
}
