package bitcoin

import (
	"context"
	"crypto/sha512"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/btcsuite/btcd/chaincfg"
	"github.com/shopspring/decimal"
	"golang.org/x/crypto/pbkdf2"
)

// Test vectors for the mnemonic "abandon abandon … about".
const (
	zpub84 = "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs"
	xpub86 = "xpub6BgBgsespWvERF3LHQu6CnqdvfEvtMcQjYrcRzx53QJjSxarj2afYWcLteoGVky7D3UKDP9QyrLprQ3VCECoY49yfdDEHGCtMMj92pReUsQ"
)

func addr(t *testing.T, key string, st scriptType, chain, i uint32) string {
	t.Helper()
	k, _, err := parseKey(key)
	if err != nil {
		t.Fatal(err)
	}
	a, err := deriveAddress(k, st, chain, i)
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// asXpub re-encodes a zpub/ypub with xpub version bytes.
func asXpub(s string) string {
	raw, _, _ := base58.CheckDecode(s)
	return base58.CheckEncode(append([]byte{0x88, 0xb2, 0x1e}, raw[3:]...), 0x04)
}

// accountKey derives m/purpose'/0'/0' from the test mnemonic (BIP39 seed)
// and serializes its public key with the given SLIP-132 version.
func accountKey(t *testing.T, purpose uint32, ver [4]byte) string {
	t.Helper()
	mnemonic := strings.TrimSpace(strings.Repeat("abandon ", 11)) + " about"
	seed := pbkdf2.Key([]byte(mnemonic), []byte("mnemonic"), 2048, 64, sha512.New)
	k, err := hdkeychain.NewMaster(seed, &chaincfg.MainNetParams)
	if err != nil {
		t.Fatal(err)
	}
	for _, i := range []uint32{purpose, 0, 0} {
		if k, err = k.Derive(hdkeychain.HardenedKeyStart + i); err != nil {
			t.Fatal(err)
		}
	}
	pub, err := k.Neuter()
	if err != nil {
		t.Fatal(err)
	}
	raw, _, _ := base58.CheckDecode(pub.String())
	return base58.CheckEncode(append([]byte{ver[1], ver[2], ver[3]}, raw[3:]...), ver[0])
}

func TestDerivationVectors(t *testing.T) {
	if got := accountKey(t, 84, verZpub); got != zpub84 {
		t.Fatalf("BIP84 account key %s", got)
	}
	if got := accountKey(t, 86, verXpub); got != xpub86 {
		t.Fatalf("BIP86 account key %s", got)
	}
	ypub49 := accountKey(t, 49, verYpub)
	cases := []struct {
		key  string
		st   scriptType
		c, i uint32
		want string
	}{
		{zpub84, p2wpkh, 0, 0, "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu"},
		{zpub84, p2wpkh, 0, 1, "bc1qnjg0jd8228aq7egyzacy8cys3knf9xvrerkf9g"},
		{zpub84, p2wpkh, 1, 0, "bc1q8c6fshw2dlwun7ekn9qwf37cu2rn755upcp6el"},
		{ypub49, p2shP2wpkh, 0, 0, "37VucYSaXLCAsxYyAPfbSi9eh4iEcbShgf"},
		{xpub86, p2tr, 0, 0, "bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr"},
	}
	for _, c := range cases {
		if got := addr(t, c.key, c.st, c.c, c.i); got != c.want {
			t.Errorf("%s %s %d/%d = %s, want %s", c.key[:4], c.st, c.c, c.i, got, c.want)
		}
	}
	// Legacy format from an xpub starts with 1.
	if a := addr(t, asXpub(zpub84), p2pkh, 0, 0); !strings.HasPrefix(a, "1") {
		t.Errorf("p2pkh %s", a)
	}
	// A plain xpub scans every format; a zpub only native segwit.
	if _, types, _ := parseKey(asXpub(zpub84)); len(types) != 4 {
		t.Errorf("xpub types %v", types)
	}
	if _, types, _ := parseKey(zpub84); len(types) != 1 || types[0] != p2wpkh {
		t.Errorf("zpub types %v", types)
	}
	// The same key read as xpub gives the zpub's segwit addresses.
	if addr(t, asXpub(zpub84), p2wpkh, 0, 0) != "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu" {
		t.Error("xpub segwit")
	}
}

func TestParseErrors(t *testing.T) {
	for _, bad := range []string{
		"",
		"hello",
		"xprv9s21ZrQH143K3GJpoapnV8SFfukcVBSfeCficPSGfubmSFDxo1kuHnLisriDvSnRRuL2Qrg5ggqHKNVpxR86QEC8w35uxmGoggxtQTPvfUu", // private
		"tpubD6NzVbkrYhZ4XgiXtGrdW5XDAPFCL9h7we1vwNCpn8tGbBcgfVYjXyhWo4E1xkh56hjod1RhGjxbaTLV3X4FyWuejifB9jusQ46QzG87VKp", // testnet
	} {
		if _, _, err := parseKey(bad); err == nil {
			t.Errorf("%q must be refused", bad)
		}
	}
	if _, err := New(source.Config{Values: map[string]string{"xpub": "nope"}}); err == nil {
		t.Error("New must refuse a bad key")
	}
	d, err := source.Get("bitcoin")
	if err != nil || d.Category != "wallet" || !d.Fields[0].Secret {
		t.Fatalf("definition %+v %v", d, err)
	}
}

// fakeEsplora serves address stats and paginated confirmed txs.
type fakeEsplora struct {
	mu      sync.Mutex
	stats   map[string]addrStats
	txs     map[string][]tx // newest first, like Esplora
	calls   map[string]int
	fail    int // answer the next n requests with 429
	errPath string
}

func (f *fakeEsplora) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls[r.URL.Path]++
	if f.fail > 0 {
		f.fail--
		w.WriteHeader(http.StatusTooManyRequests)
		return
	}
	if r.URL.Path == f.errPath {
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, "Invalid Bitcoin address")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/address/"), "/")
	a := parts[0]
	if len(parts) == 1 {
		json.NewEncoder(w).Encode(f.stats[a])
		return
	}
	list := f.txs[a]
	start := 0
	if len(parts) == 4 { // /txs/chain/:last_seen
		for i, t := range list {
			if t.TxID == parts[3] {
				start = i + 1
			}
		}
	}
	end := min(start+pageSize, len(list))
	json.NewEncoder(w).Encode(list[start:end])
}

func mk(id string, at int64, fee int64, ins []output, outs []output) tx {
	var t tx
	t.TxID, t.Fee, t.Vout = id, fee, outs
	t.Status.Confirmed, t.Status.BlockTime = true, at
	for _, in := range ins {
		in := in
		t.Vin = append(t.Vin, struct {
			Prevout *output `json:"prevout"`
		}{Prevout: &in})
	}
	return t
}

func setup(t *testing.T) (*fakeEsplora, *httptest.Server, map[string]string) {
	t.Helper()
	r0, r1, r2 := addr(t, zpub84, p2wpkh, 0, 0), addr(t, zpub84, p2wpkh, 0, 1), addr(t, zpub84, p2wpkh, 0, 2)
	c0 := addr(t, zpub84, p2wpkh, 1, 0)
	foreign := "bc1qforeign0000000000000000000000000000000"
	t0 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC).Unix()

	A := mk("aa", t0, 200, []output{{Address: foreign, Value: 1_000_200}}, []output{{Address: r0, Value: 1_000_000}})
	B := mk("bb", t0+3600, 10_000, []output{{Address: r0, Value: 1_000_000}},
		[]output{{Address: foreign, Value: 300_000}, {Address: c0, Value: 690_000}})
	C := mk("cc", t0+7200, 5_000, []output{{Address: c0, Value: 690_000}}, []output{{Address: r1, Value: 685_000}})
	f := &fakeEsplora{calls: map[string]int{}, stats: map[string]addrStats{}, txs: map[string][]tx{}}
	f.txs[r0] = []tx{B, A}
	f.txs[c0] = []tx{C, B}
	f.txs[r1] = []tx{C}
	// 30 small deposits on r2: two pages.
	for i := 0; i < 30; i++ {
		e := mk(fmt.Sprintf("e%02d", i), t0+int64(10_000+i), 100, []output{{Address: foreign, Value: 1_100}}, []output{{Address: r2, Value: 1_000}})
		f.txs[r2] = append([]tx{e}, f.txs[r2]...)
	}
	f.stats[r0] = addrStats{Chain: stats{TxCount: 2, Funded: 1_000_000, Spent: 1_000_000}}
	f.stats[c0] = addrStats{Chain: stats{TxCount: 2, Funded: 690_000, Spent: 690_000}}
	f.stats[r1] = addrStats{Chain: stats{TxCount: 1, Funded: 685_000}, Mempool: stats{TxCount: 1, Funded: 50_000}}
	f.stats[r2] = addrStats{Chain: stats{TxCount: 30, Funded: 30_000}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return f, srv, map[string]string{"r0": r0, "r1": r1, "r2": r2, "c0": c0}
}

func newSource(t *testing.T, srv *httptest.Server, state []byte) *Wallet {
	t.Helper()
	s, err := New(source.Config{Values: map[string]string{"xpub": zpub84, "esplora_url": srv.URL + "/"}, HTTP: srv.Client(), State: state})
	if err != nil {
		t.Fatal(err)
	}
	b := s.(*Wallet)
	b.c.pause = 0
	b.c.sleep = func(ctx context.Context, d time.Duration) error { return ctx.Err() }
	return b
}

func collect(t *testing.T, b *Wallet, since time.Time) ([]model.Activity, []string) {
	t.Helper()
	var acts []model.Activity
	var logs []string
	err := b.Activities(context.Background(), source.Fetch{
		Since:    since,
		Emit:     func(a model.Activity) error { acts = append(acts, a); return nil },
		Progress: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return acts, logs
}

func TestWalletHistoryAndBalance(t *testing.T) {
	f, srv, a := setup(t)
	b := newSource(t, srv, nil)
	if err := b.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	acts, logs := collect(t, b, time.Time{})
	if len(acts) != 33 {
		t.Fatalf("%d activities", len(acts))
	}
	by := map[string]model.Activity{}
	for _, x := range acts {
		by[x.NativeID] = x
		if x.Time.Location() != time.UTC || x.Network != "BTC" || x.TxID != x.NativeID {
			t.Fatalf("meta %+v", x)
		}
	}
	dep := by["aa"]
	if dep.Type != model.Deposit || !dep.Base.Qty.Equal(decimal.RequireFromString("0.01")) || dep.Address != a["r0"] || dep.Fee != nil {
		t.Errorf("deposit %+v", dep)
	}
	wd := by["bb"]
	if wd.Type != model.Withdrawal || !wd.Base.Qty.Equal(decimal.RequireFromString("0.003")) || wd.Fee == nil ||
		!wd.Fee.Qty.Equal(decimal.RequireFromString("0.0001")) || wd.Address != "bc1qforeign0000000000000000000000000000000" {
		t.Errorf("withdrawal %+v", wd)
	}
	self := by["cc"]
	if self.Type != model.Fee || !self.Base.Qty.Equal(decimal.RequireFromString("0.00005")) {
		t.Errorf("self transfer %+v", self)
	}
	bal, err := b.Balances(context.Background())
	if err != nil || len(bal) != 1 || !bal[0].Qty.Equal(decimal.RequireFromString("0.00715")) {
		t.Fatalf("balance %+v %v", bal, err)
	}
	if gaps := model.Reconcile(acts, bal, decimal.Zero); len(gaps) != 0 {
		t.Fatalf("history does not add up: %+v", gaps)
	}
	// r2 has 30 txs: two pages.
	if f.calls["/address/"+a["r2"]+"/txs/chain"] != 1 || f.calls["/address/"+a["r2"]+"/txs/chain/e05"] != 1 {
		t.Errorf("paging calls %v", f.calls)
	}
	// Gap limit: receive 0..2 used, then 20 empty (0..22); change 0 used, then 20 (0..20).
	n := 0
	for p := range f.calls {
		if strings.Count(p, "/") == 2 {
			n++
		}
	}
	if n != 23+21 {
		t.Errorf("addresses checked %d", n)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "4 used") {
		t.Errorf("logs %v", logs)
	}

	// Incremental: same state, nothing changed: no tx fetched again.
	b2 := newSource(t, srv, b.State())
	before := f.calls["/address/"+a["r0"]+"/txs/chain"]
	acts2, _ := collect(t, b2, time.Now())
	if len(acts2) != 0 || f.calls["/address/"+a["r0"]+"/txs/chain"] != before {
		t.Errorf("incremental refetched: %d", len(acts2))
	}
	// A new confirmed tx on r1 is picked up.
	f.mu.Lock()
	D := mk("dd", time.Now().Unix(), 100, []output{{Address: "bc1qforeign0000000000000000000000000000000", Value: 50_100}}, []output{{Address: a["r1"], Value: 50_000}})
	f.txs[a["r1"]] = append([]tx{D}, f.txs[a["r1"]]...)
	f.stats[a["r1"]] = addrStats{Chain: stats{TxCount: 2, Funded: 735_000}}
	f.mu.Unlock()
	b3 := newSource(t, srv, b2.State())
	acts3, _ := collect(t, b3, time.Now())
	ids := map[string]bool{}
	for _, x := range acts3 {
		ids[x.NativeID] = true
	}
	if !ids["dd"] || ids["aa"] {
		t.Errorf("incremental %v", ids)
	}
}

func TestClassifyEdges(t *testing.T) {
	owned := map[string]bool{"me": true}
	u := mk("u", 0, 10, nil, []output{{Address: "me", Value: 5}})
	u.Status.Confirmed = false
	if classify(u, owned) != nil {
		t.Error("unconfirmed ignored")
	}
	if classify(mk("x", 1, 10, []output{{Address: "a", Value: 9}}, []output{{Address: "b", Value: 5}}), owned) != nil {
		t.Error("foreign tx ignored")
	}
	// A joint transaction where we put in less than we got back.
	j := classify(mk("j", 1, 10, []output{{Address: "me", Value: 100}, {Address: "o", Value: 100}}, []output{{Address: "me", Value: 150}, {Address: "o", Value: 40}}), owned)
	if len(j) != 1 || j[0].Type != model.Deposit || !j[0].Base.Qty.Equal(decimal.New(50, -8)) {
		t.Errorf("joint %+v", j)
	}
	// Spent exactly what came back: nothing.
	if classify(mk("z", 1, 0, []output{{Address: "me", Value: 100}}, []output{{Address: "me", Value: 100}}), owned) != nil {
		t.Error("zero delta")
	}
	// Coinbase input (no prevout) paying us.
	cb := mk("cb", 1, 0, nil, []output{{Address: "me", Value: 625_000_000}})
	cb.Vin = append(cb.Vin, struct {
		Prevout *output `json:"prevout"`
	}{})
	if r := classify(cb, owned); len(r) != 1 || r[0].Type != model.Deposit {
		t.Errorf("coinbase %+v", r)
	}
}

func TestClientRetriesAndErrors(t *testing.T) {
	f, srv, a := setup(t)
	b := newSource(t, srv, nil)
	var waits []time.Duration
	b.c.sleep = func(ctx context.Context, d time.Duration) error {
		if d > 0 {
			waits = append(waits, d)
		}
		return nil
	}
	f.fail = 2
	if _, err := b.c.address(context.Background(), a["r0"]); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 || waits[0] != 2*time.Second || waits[1] != 4*time.Second {
		t.Errorf("backoff %v", waits)
	}
	f.errPath = "/address/" + a["r0"]
	if _, err := b.c.address(context.Background(), a["r0"]); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Errorf("error %v", err)
	}
	// Errors propagate from Activities, Balances and Test.
	if err := b.Test(context.Background()); err == nil {
		t.Error("test must fail")
	}
	if err := b.Activities(context.Background(), source.Fetch{Emit: func(model.Activity) error { return nil }}); err == nil {
		t.Error("activities must fail")
	}
	if _, err := b.Balances(context.Background()); err == nil {
		t.Error("balances must fail")
	}
	f.errPath = ""
	f.fail = 6
	if _, err := b.c.address(context.Background(), a["r1"]); err == nil {
		t.Error("give up after 5 retries")
	}
	// Undecodable answer.
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, "not json") }))
	defer bad.Close()
	b.c.base = bad.URL
	if _, err := b.c.address(context.Background(), "x"); err == nil {
		t.Error("decode error")
	}
	// Emit errors stop the run; an empty wallet has no balance.
	b2 := newSource(t, srv, nil)
	if err := b2.Activities(context.Background(), source.Fetch{Emit: func(model.Activity) error { return fmt.Errorf("stop") }}); err == nil {
		t.Error("emit error")
	}
	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { fmt.Fprint(w, `{}`) }))
	defer empty.Close()
	b3 := newSource(t, empty, nil)
	if bal, err := b3.Balances(context.Background()); err != nil || bal != nil {
		t.Errorf("empty wallet %v %v", bal, err)
	}
	// Default and bare-host Esplora URLs.
	s, _ := New(source.Config{Values: map[string]string{"xpub": zpub84}})
	if s.(*Wallet).c.base != defaultEsplora {
		t.Error("default esplora")
	}
	s, _ = New(source.Config{Values: map[string]string{"xpub": zpub84, "esplora_url": "mempool.local/api"}, State: []byte(`{"txCounts":{"a":1}}`)})
	if s.(*Wallet).c.base != "https://mempool.local/api" || s.(*Wallet).st.TxCounts["a"] != 1 {
		t.Error("bare host / state")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) == nil || sleepCtx(context.Background(), time.Millisecond) != nil {
		t.Error("sleepCtx")
	}
}

func TestClientFailsOverToNextServer(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	down.Close() // refuses connections, like a blocked host
	busy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer busy.Close()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"chain_stats":{"tx_count":3}}`)
	}))
	defer up.Close()
	c := &client{base: down.URL, fallback: []server{{url: busy.URL}, {url: up.URL}}, http: up.Client(),
		sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
	s, err := c.address(context.Background(), "x")
	if err != nil || s.Chain.TxCount != 3 || c.base != up.URL {
		t.Fatalf("failover: %v %+v %s", err, s, c.base)
	}
	// Past the last server, the error comes back.
	c = &client{base: down.URL, http: up.Client(), sleep: func(ctx context.Context, d time.Duration) error { return nil }}
	if _, err := c.address(context.Background(), "x"); err == nil {
		t.Error("no server left must fail")
	}
	// The default comes with a fallback; a server the user chose does not.
	s1, _ := New(source.Config{Values: map[string]string{"xpub": zpub84}})
	s2, _ := New(source.Config{Values: map[string]string{"xpub": zpub84, "esplora_url": "https://my.node/api"}})
	if len(s1.(*Wallet).c.fallback) == 0 || len(s2.(*Wallet).c.fallback) != 0 {
		t.Error("fallback only for the default")
	}
}
