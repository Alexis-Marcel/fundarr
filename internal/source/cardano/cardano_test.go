package cardano

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

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// CIP-19 test vectors: the same payment and stake keys in every form.
const (
	base0   = "addr1qx2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzer3n0d3vllmyqwsx5wktcd8cc3sq835lu7drv2xwl2wywfgse35a3x"
	base1   = "addr1z8phkx6acpnf78fuvxn0mkew3l0fd058hzquvz7w36x4gten0d3vllmyqwsx5wktcd8cc3sq835lu7drv2xwl2wywfgs9yc0hh"
	base2   = "addr1yx2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzerkr0vd4msrxnuwnccdxlhdjar77j6lg0wypcc9uar5d2shs2z78ve"
	ptr     = "addr1gx2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzer5pnz75xxcrzqf96k"
	enter   = "addr1vx2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzers66hrl8"
	stake   = "stake1uyehkck0lajq8gr28t9uxnuvgcqrc6070x3k9r8048z8y5gh6ffgw"
	stakeSh = "stake178phkx6acpnf78fuvxn0mkew3l0fd058hzquvz7w36x4gtcccycj5"
	byron   = "Ae2tdPwUPEZFRbyhz3cpfC2CumGzNkFBN2L42rcUc2yjQpEkxDbkPodpMAi"
	testnet = "addr_test1qz2fxv2umyhttkxyxp8x0dlpdt3k6cwng5pxj3jhsydzer3n0d3vllmyqwsx5wktcd8cc3sq835lu7drv2xwl2wywfgs68faae"
	// A real Ledger Live receive address and its stake address (Koios).
	ledger      = "addr1q9lka4pvlz94h02dszv77nemrgr0zcze0ezstnu00v0cgrpuq0mhcqwjtl847yaxdp58c9z8vyyttvnsgv9qydyz4lys2ygw5f"
	ledgerStake = "stake1uy7q8amuq8f9ln6lzwnxs6ruz3rkzz94kfcyxzszxjp2ljg3nexkc"

	djed    = "8db269c3ec630e06ae29f74bc39edd1f87c819f1056206e879a1cd61"
	djedHex = "446a65644d6963726f555344"
	spamPol = "3281c71082d256a60103418146fc2f175e2e02aed49d6b57555135d1"
	otherSt = "stake1u9qzfx6w5rd2xmuydfz4u8eeqwc55seyxjt0fwsga5z5y3qp69tgu"
)

func TestAddresses(t *testing.T) {
	for in, want := range map[string]string{base0: stake, base1: stake, base2: stakeSh, stake: stake, ledger: ledgerStake,
		"  " + base0 + "\n": stake} {
		got, err := stakeAddress(in)
		if err != nil || got != want {
			t.Fatalf("%s → %q, %v; want %s", in, got, err, want)
		}
		if !Valid(in) {
			t.Fatalf("Valid(%s) = false", in)
		}
	}
	bad := []string{ptr, enter, byron, testnet, "", "stake1", base0[:len(base0)-1] + "y", strings.ToUpper(base0[:5]) + base0[5:],
		"bc1qar0srrr7xfkvy5l643lydnw9re59gtzzwf5mdq"}
	for _, in := range bad {
		if Valid(in) {
			t.Fatalf("Valid(%s) = true", in)
		}
	}
	if _, err := New(source.Config{Values: map[string]string{"address": enter}}); err == nil || !strings.Contains(err.Error(), "addr1q") {
		t.Fatalf("enterprise address: %v", err)
	}
	if _, err := New(source.Config{Values: map[string]string{"address": byron}}); err == nil || !strings.Contains(err.Error(), "stake address") {
		t.Fatalf("byron address: %v", err)
	}
}

// ---- fake Koios

type out struct {
	stake, addr string
	lovelace    string
	assets      [][3]string // policy, name, quantity
}

func (o out) json() map[string]any {
	var al []map[string]any
	for _, a := range o.assets {
		al = append(al, map[string]any{"policy_id": a[0], "asset_name": a[1], "quantity": a[2], "decimals": 0})
	}
	if al == nil {
		al = []map[string]any{}
	}
	var st any
	if o.stake != "" {
		st = o.stake
	}
	return map[string]any{"value": o.lovelace, "stake_addr": st, "asset_list": al,
		"payment_addr": map[string]any{"bech32": o.addr, "cred": "00"}}
}

type ftx struct {
	hash        string
	height      int64
	fee         string
	deposit     string
	ins, outs   []out
	withdrawals [][2]string // stake, amount
	certs       []string    // stake addresses
}

func (t ftx) json() map[string]any {
	var ins, outs, wd, certs []map[string]any
	for _, o := range t.ins {
		ins = append(ins, o.json())
	}
	for _, o := range t.outs {
		outs = append(outs, o.json())
	}
	for _, w := range t.withdrawals {
		wd = append(wd, map[string]any{"stake_addr": w[0], "amount": w[1]})
	}
	for _, c := range t.certs {
		certs = append(certs, map[string]any{"type": "stake_registration", "index": 0, "info": map[string]any{"stake_address": c}})
	}
	dep := t.deposit
	if dep == "" {
		dep = "0"
	}
	return map[string]any{"tx_hash": t.hash, "block_height": t.height, "tx_timestamp": 1700000000 + t.height, "fee": t.fee,
		"deposit": dep, "inputs": ins, "outputs": outs, "withdrawals": wd, "certificates": certs}
}

type fake struct {
	mu        sync.Mutex
	txs       []ftx
	rewards   [][4]string // earned, spendable, amount, type
	info      map[string]string
	assets    [][3]string
	calls     []string
	limitOne  bool
	wantKey   string
	sawTxInfo [][]string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wantKey != "" && r.Header.Get("Authorization") != "Bearer "+f.wantKey {
		w.WriteHeader(403)
		fmt.Fprint(w, "Malformed Auth Token")
		return
	}
	q := r.URL.Query()
	path := strings.Trim(r.URL.Path, "/")
	f.calls = append(f.calls, path+"?"+q.Encode())
	if f.limitOne {
		f.limitOne = false
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	var body struct {
		Stakes []string `json:"_stake_addresses"`
		Hashes []string `json:"_tx_hashes"`
		Inputs bool     `json:"_inputs"`
		Assets bool     `json:"_assets"`
	}
	if r.Method == http.MethodPost {
		b, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(b, &body); err != nil {
			http.Error(w, "bad body", 400)
			return
		}
		if path != "tx_info" && (len(body.Stakes) != 1 || body.Stakes[0] != stake) {
			http.Error(w, `{"message":"wrong account"}`, 400)
			return
		}
	}
	var rows []any
	switch path {
	case "account_txs":
		if r.Method != http.MethodGet || q.Get("_stake_address") != stake {
			http.Error(w, "bad request", 400)
			return
		}
		after, _ := strconv.ParseInt(q.Get("_after_block_height"), 10, 64)
		for _, t := range f.txs {
			if t.height >= after {
				rows = append(rows, map[string]any{"tx_hash": t.hash, "block_height": t.height, "epoch_no": 1, "block_time": 1})
			}
		}
	case "tx_info":
		if len(body.Hashes) > batch || !body.Inputs || !body.Assets {
			http.Error(w, "bad tx_info", 400)
			return
		}
		f.sawTxInfo = append(f.sawTxInfo, body.Hashes)
		for _, h := range body.Hashes {
			for _, t := range f.txs {
				if t.hash == h {
					rows = append(rows, t.json())
				}
			}
		}
	case "account_reward_history":
		from := 0
		if v := q.Get("earned_epoch"); v != "" {
			from, _ = strconv.Atoi(strings.TrimPrefix(v, "gte."))
		}
		for _, x := range f.rewards {
			e, _ := strconv.Atoi(x[0])
			s, _ := strconv.Atoi(x[1])
			if e >= from {
				rows = append(rows, map[string]any{"stake_address": stake, "earned_epoch": e, "spendable_epoch": s, "amount": x[2], "type": x[3]})
			}
		}
	case "account_info":
		if f.info != nil {
			rows = append(rows, map[string]any{"stake_address": stake, "total_balance": f.info["total"], "utxo": f.info["utxo"]})
		}
	case "account_assets":
		for _, a := range f.assets {
			if strings.Contains(q.Get("policy_id"), a[0]) {
				rows = append(rows, map[string]any{"stake_address": stake, "policy_id": a[0], "asset_name": a[1], "quantity": a[2]})
			}
		}
	default:
		http.Error(w, "not found", 404)
		return
	}
	if lim := q.Get("limit"); lim != "" {
		l, _ := strconv.Atoi(lim)
		o, _ := strconv.Atoi(q.Get("offset"))
		rows = rows[min(o, len(rows)):min(o+l, len(rows))]
	}
	if rows == nil {
		rows = []any{}
	}
	json.NewEncoder(w).Encode(rows)
}

func me(lovelace string, assets ...[3]string) out {
	return out{stake: stake, addr: base0, lovelace: lovelace, assets: assets}
}

func them(lovelace string, assets ...[3]string) out {
	return out{stake: otherSt, addr: "addr1them", lovelace: lovelace, assets: assets}
}

func newFake() *fake {
	return &fake{
		txs: []ftx{
			// Received 100 ADA, 50 DJED and an airdropped NFT.
			{hash: "h1", height: 100, fee: "170000", ins: []out{them("150500000")},
				outs: []out{me("100000000", [3]string{djed, djedHex, "50000000"}, [3]string{spamPol, "4e4654", "1"}), them("50330000")}},
			// Stake key registration and delegation: deposit 2, fee 0.2.
			{hash: "h2", height: 200, fee: "200000", deposit: "2000000", certs: []string{stake, stake},
				ins: []out{me("100000000")}, outs: []out{me("97800000")}},
			// Sent 30 ADA, fee 0.2.
			{hash: "h3", height: 300, fee: "200000", ins: []out{me("97800000")}, outs: []out{them("30000000"), me("67600000")}},
			// Withdrew 5 ADA of rewards: only the fee is spent.
			{hash: "h4", height: 400, fee: "200000", withdrawals: [][2]string{{stake, "5000000"}},
				ins: []out{me("67600000")}, outs: []out{me("72400000")}},
			// To oneself: fee only.
			{hash: "h5", height: 500, fee: "200000", ins: []out{me("72400000")}, outs: []out{me("72200000")}},
			// Sent 20 DJED with 1.5 ADA.
			{hash: "h6", height: 600, fee: "200000", ins: []out{me("72200000", [3]string{djed, djedHex, "50000000"})},
				outs: []out{them("1500000", [3]string{djed, djedHex, "20000000"}), me("70500000", [3]string{djed, djedHex, "30000000"})}},
		},
		rewards: [][4]string{
			{"300", "302", "3000000", "member"},
			{"301", "303", "2000000", "member"},
			{"301", "303", "500000", "leader"},
			{"302", "304", "0", "member"},
			{"999", "1001", "7000000", "member"}, // not spendable yet
		},
		// 70.5 on addresses + 0.5 rewards left.
		info:   map[string]string{"total": "71000000", "utxo": "70500000"},
		assets: [][3]string{{djed, djedHex, "30000000"}, {spamPol, "4e4654", "1"}},
	}
}

func connect(t *testing.T, srv *httptest.Server, values map[string]string, state []byte) *Source {
	t.Helper()
	v := map[string]string{"address": base0, "base_url": srv.URL}
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
	src.now = func() time.Time { return epochStart(400) }
	return src
}

func fetch(t *testing.T, s *Source) []model.Activity {
	t.Helper()
	var acts []model.Activity
	err := s.Activities(context.Background(), source.Fetch{
		Emit:     func(a model.Activity) error { acts = append(acts, a); return nil },
		Progress: func(string, ...any) {},
	})
	if err != nil {
		t.Fatal(err)
	}
	return acts
}

func byNativeID(t *testing.T, acts []model.Activity) map[string]model.Activity {
	t.Helper()
	m := map[string]model.Activity{}
	for _, a := range acts {
		if _, dup := m[a.NativeID]; dup {
			t.Fatalf("duplicate %s", a.NativeID)
		}
		if a.Time.IsZero() || a.Network != "cardano" || !a.Base.Qty.IsPositive() {
			t.Fatalf("incomplete %+v", a)
		}
		m[a.NativeID] = a
	}
	return m
}

func keys(m map[string]model.Activity) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func TestHistory(t *testing.T) {
	f := newFake()
	f.limitOne = true
	f.wantKey = "SECRET"
	srv := httptest.NewServer(f)
	defer srv.Close()
	old := pageSize
	pageSize = 2 // force paging
	defer func() { pageSize = old }()

	s := connect(t, srv, map[string]string{"api_key": "SECRET"}, nil)
	if err := s.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	byID := byNativeID(t, fetch(t, s))
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
	if a := check("h1:ada", model.Deposit, "ADA", "100"); a.TxID != "h1" || a.Address != base0 || a.Fee != nil ||
		!a.Time.Equal(time.Unix(1700000100, 0)) {
		t.Fatalf("receive %+v", a)
	}
	check("h1:"+djed+"."+djedHex, model.Deposit, "DJED", "50")
	if a := check("h2:deposit", model.Withdrawal, "ADA", "2"); a.Note != "stake key deposit" {
		t.Fatalf("deposit note %q", a.Note)
	}
	check("h2:ada", model.Fee, "ADA", "0.2")
	if a := check("h3:ada", model.Withdrawal, "ADA", "30"); a.Fee == nil || !a.Fee.Qty.Equal(decimal.RequireFromString("0.2")) || a.Address != "addr1them" {
		t.Fatalf("send %+v", a)
	}
	if a := check("h4:ada", model.Fee, "ADA", "0.2"); !strings.Contains(a.Note, "withdrawal") {
		t.Fatalf("withdrawal note %q", a.Note)
	}
	check("h5:ada", model.Fee, "ADA", "0.2")
	check("h6:ada", model.Withdrawal, "ADA", "1.5")
	check("h6:"+djed+"."+djedHex, model.Withdrawal, "DJED", "20")
	r := check("reward:300", model.Reward, "ADA", "3")
	if !r.Time.Equal(epochStart(302)) || r.TxID != "" {
		t.Fatalf("reward %+v", r)
	}
	if a := check("reward:301", model.Reward, "ADA", "2.5"); !strings.Contains(a.Note, "leader") {
		t.Fatalf("reward note %q", a.Note)
	}
	for _, id := range keys(byID) {
		if strings.Contains(id, spamPol) || id == "reward:302" || id == "reward:999" {
			t.Fatalf("should be skipped: %s", id)
		}
	}
	if len(byID) != 11 {
		t.Fatalf("%d activities: %v", len(byID), keys(byID))
	}

	// The history adds up to the balance.
	bals, err := s.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	sum := map[string]decimal.Decimal{}
	for _, a := range byID {
		q := a.Base.Qty
		if a.Outflow() {
			q = q.Neg()
		}
		sum[a.Base.Asset.Symbol] = sum[a.Base.Asset.Symbol].Add(q)
		if a.Fee != nil {
			sum[a.Fee.Asset.Symbol] = sum[a.Fee.Asset.Symbol].Sub(a.Fee.Qty)
		}
	}
	if len(bals) != 2 {
		t.Fatalf("balances %+v", bals)
	}
	for _, b := range bals {
		if !b.Qty.Equal(sum[b.Asset.Symbol]) {
			t.Fatalf("%s: balance %s, history %s", b.Asset.Symbol, b.Qty, sum[b.Asset.Symbol])
		}
	}
	if !bals[0].Qty.Equal(decimal.RequireFromString("71")) || bals[1].Asset.Symbol != "DJED" {
		t.Fatalf("balances %+v", bals)
	}

	// Incremental: the next run starts at the last block and epoch read.
	var st state
	if err := json.Unmarshal(s.State(), &st); err != nil || st.Height != 600 || st.RewardEpoch != 302 {
		t.Fatalf("state %s", s.State())
	}
	f.txs = append(f.txs, ftx{hash: "h7", height: 700, fee: "170000", ins: []out{them("5000000")}, outs: []out{me("1000000"), them("3830000")}})
	f.rewards = append(f.rewards, [4]string{"303", "305", "1000000", "member"})
	f.calls, f.sawTxInfo = nil, nil
	s2 := connect(t, srv, map[string]string{"api_key": "SECRET"}, s.State())
	byID = byNativeID(t, fetch(t, s2))
	check("h7:ada", model.Deposit, "ADA", "1")
	check("reward:303", model.Reward, "ADA", "1")
	if _, ok := byID["h1:ada"]; ok {
		t.Fatal("old history fetched again")
	}
	if len(f.sawTxInfo) != 1 || strings.Join(f.sawTxInfo[0], ",") != "h6,h7" {
		t.Fatalf("tx_info asked %v", f.sawTxInfo)
	}
	joined := strings.Join(f.calls, "\n")
	if !strings.Contains(joined, "_after_block_height=600") || !strings.Contains(joined, "earned_epoch=gte.302") {
		t.Fatalf("calls %s", joined)
	}
	if err := json.Unmarshal(s2.State(), &st); err != nil || st.Height != 700 || st.RewardEpoch != 303 {
		t.Fatalf("state %s", s2.State())
	}
}

func TestStakeAddressAndUnknownAccount(t *testing.T) {
	f := newFake()
	f.info = nil
	f.assets = nil
	f.txs, f.rewards = nil, nil
	srv := httptest.NewServer(f)
	defer srv.Close()
	s := connect(t, srv, map[string]string{"address": stake}, nil)
	if s.StakeAddress() != stake {
		t.Fatal(s.StakeAddress())
	}
	if acts := fetch(t, s); len(acts) != 0 {
		t.Fatalf("%v", acts)
	}
	bals, err := s.Balances(context.Background())
	if err != nil || len(bals) != 0 {
		t.Fatalf("%v %v", bals, err)
	}
	// A wrong key is reported, the key never is.
	f.wantKey = "GOOD"
	err = connect(t, srv, map[string]string{"api_key": "BAD-KEY"}, nil).Test(context.Background())
	if err == nil || strings.Contains(err.Error(), "BAD-KEY") || !strings.Contains(err.Error(), "403") {
		t.Fatalf("bad key: %v", err)
	}
}

func TestDeregistration(t *testing.T) {
	// Leaving staking: rewards withdrawn, deposit refunded, fee paid.
	tx := ftx{hash: "d", height: 1, fee: "200000", deposit: "-2000000", certs: []string{stake},
		withdrawals: [][2]string{{stake, "500000"}}, ins: []out{me("10000000")}, outs: []out{me("12300000")}}
	var ti txInfo
	b, _ := json.Marshal(tx.json())
	if err := json.Unmarshal(b, &ti); err != nil {
		t.Fatal(err)
	}
	acts, err := classify(ti, stake)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 2 || acts[0].Type != model.Deposit || acts[0].Note != "stake key deposit refund" ||
		!acts[0].Base.Qty.Equal(decimal.RequireFromString("2")) ||
		acts[1].Type != model.Fee || !acts[1].Base.Qty.Equal(decimal.RequireFromString("0.2")) {
		t.Fatalf("%+v", acts)
	}
}

func TestEpochStart(t *testing.T) {
	// Epoch 662 started at 1792187091 (Koios tip: block_time − epoch_slot).
	if got := epochStart(662).Unix(); got != 1792187091 {
		t.Fatal(got)
	}
}

// TestLive reads a real account on api.koios.rest: FUNDARR_LIVE_CARDANO=1.
func TestLive(t *testing.T) {
	if os.Getenv("FUNDARR_LIVE_CARDANO") != "1" {
		t.Skip("set FUNDARR_LIVE_CARDANO=1")
	}
	addr := os.Getenv("FUNDARR_CARDANO_ADDRESS")
	if addr == "" {
		addr = ledger
	}
	src, err := New(source.Config{Values: map[string]string{"address": addr, "api_key": os.Getenv("FUNDARR_KOIOS_KEY")}})
	if err != nil {
		t.Fatal(err)
	}
	s := src.(*Source)
	ctx := context.Background()
	if err := s.Test(ctx); err != nil {
		t.Fatal(err)
	}
	var acts []model.Activity
	err = s.Activities(ctx, source.Fetch{Emit: func(a model.Activity) error { acts = append(acts, a); return nil },
		Progress: func(f string, a ...any) { t.Logf(f, a...) }})
	if err != nil {
		t.Fatal(err)
	}
	bals, err := s.Balances(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sum := decimal.Zero
	for _, a := range acts {
		if a.Base.Asset.Symbol != "ADA" {
			continue
		}
		q := a.Base.Qty
		if a.Outflow() {
			q = q.Neg()
		}
		sum = sum.Add(q)
		if a.Fee != nil {
			sum = sum.Sub(a.Fee.Qty)
		}
		t.Logf("%s %-10s %s ADA %s", a.Time.Format(time.DateOnly), a.Type, a.Base.Qty, a.Note)
	}
	t.Logf("%d activities, history %s ADA, balances %+v, state %s", len(acts), sum, bals, s.State())
	if len(bals) > 0 && bals[0].Asset.Symbol == "ADA" && !bals[0].Qty.Equal(sum) {
		t.Errorf("balance %s ≠ history %s", bals[0].Qty, sum)
	}
}
