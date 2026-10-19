package bitcoin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/btcsuite/btcd/btcutil"
	"github.com/btcsuite/btcd/btcutil/base58"
)

// reversion writes the same key with other version bytes (Ltub, dgub…).
func reversion(key string, v [4]byte) string {
	raw, _, _ := base58.CheckDecode(key)
	return base58.CheckEncode(append([]byte{v[1], v[2], v[3]}, raw[3:]...), v[0])
}

func TestCashAddr(t *testing.T) {
	// The example of the cashaddr specification.
	a, err := btcutil.DecodeAddress("1BpEi6DfDAUFd7GtittLSdBeYJvcoaVggu", Bitcoin.params)
	if err != nil {
		t.Fatal(err)
	}
	if got := cashAddr(a.ScriptAddress()); got != "bitcoincash:qpm2qsznhks23z7629mms6s4cwef74vcwvy22gdx6a" {
		t.Errorf("cashaddr %s", got)
	}
}

func TestOtherCoinsDerive(t *testing.T) {
	btcKey, _, err := Bitcoin.parseKey(zpub84)
	if err != nil {
		t.Fatal(err)
	}
	btcAddr, _ := Bitcoin.derive(btcKey, p2pkh, 0, 0)
	hash := func(addr string, c Coin) string {
		a, err := btcutil.DecodeAddress(addr, c.params)
		if err != nil {
			t.Fatalf("%s %s: %v", c.Name, addr, err)
		}
		return fmt.Sprintf("%x", a.ScriptAddress())
	}
	want := hash(btcAddr, Bitcoin)
	for _, tc := range []struct {
		coin   Coin
		ver    [4]byte
		kind   scriptType
		prefix string
		types  int
	}{
		{Litecoin, verLtub, p2wpkh, "ltc1q", 3},
		{Litecoin, verLtub, p2pkh, "L", 3},
		{Litecoin, verMtub, p2shP2wpkh, "M", 1},
		{Dogecoin, verDgub, p2pkh, "D", 1},
	} {
		k, types, err := tc.coin.parseKey(reversion(zpub84, tc.ver))
		if err != nil || len(types) != tc.types {
			t.Fatalf("%s: %v %v", tc.coin.Name, types, err)
		}
		a, err := tc.coin.derive(k, tc.kind, 0, 0)
		if err != nil || !strings.HasPrefix(a, tc.prefix) {
			t.Errorf("%s %s: %s %v", tc.coin.Name, tc.kind, a, err)
		}
		if tc.kind == p2pkh && hash(a, tc.coin) != want {
			t.Errorf("%s: same key, other hash", tc.coin.Name)
		}
	}
	k, _, _ := BitcoinCash.parseKey(reversion(zpub84, verXpub))
	if a, _ := BitcoinCash.derive(k, p2pkh, 0, 0); !strings.HasPrefix(a, "bitcoincash:q") {
		t.Errorf("bch %s", a)
	}
	// A Dogecoin key is not a Bitcoin one.
	if _, _, err := Bitcoin.parseKey(reversion(zpub84, verDgub)); err == nil {
		t.Error("dgub accepted as Bitcoin")
	}
}

func TestBlockbook(t *testing.T) {
	k, _, _ := Dogecoin.parseKey(reversion(zpub84, verDgub))
	mine, _ := Dogecoin.derive(k, p2pkh, 0, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/address/"+mine) {
			fmt.Fprint(w, `{"txs":0,"totalReceived":"0","totalSent":"0","totalPages":1}`)
			return
		}
		if r.URL.Query().Get("details") == "basic" {
			fmt.Fprint(w, `{"txs":3,"unconfirmedTxs":1,"totalReceived":"500000000","totalSent":"100000000"}`)
			return
		}
		if r.URL.Query().Get("page") == "1" {
			fmt.Fprintf(w, `{"totalPages":2,"transactions":[
				{"txid":"in1","blockTime":1700000000,"confirmations":10,"fees":"1000",
				 "vin":[{"addresses":["Dother"],"value":"500001000"}],"vout":[{"addresses":["%s"],"value":"500000000"}]},
				{"txid":"pending","blockTime":1700000500,"confirmations":0,"fees":"1",
				 "vin":[{"addresses":["Dother"],"value":"2"}],"vout":[{"addresses":["%s"],"value":"1"}]}]}`, mine, mine)
			return
		}
		fmt.Fprintf(w, `{"totalPages":2,"transactions":[
			{"txid":"out1","blockTime":1700001000,"confirmations":5,"fees":"20000",
			 "vin":[{"addresses":["%s"],"value":"500000000"}],
			 "vout":[{"addresses":["Dshop"],"value":"100000000"},{"addresses":["%s"],"value":"399980000"}]}]}`, mine, mine)
	}))
	defer srv.Close()
	s, err := NewCoin(Dogecoin, source.Config{Values: map[string]string{"xpub": reversion(zpub84, verDgub)}, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	b := s.(*Wallet)
	if !b.c.blockbook || b.c.base != Dogecoin.servers[0].url {
		t.Fatalf("default server %+v", b.c)
	}
	b.c.base, b.c.pause = srv.URL, 0
	var acts []model.Activity
	if err := b.Activities(context.Background(), source.Fetch{Emit: func(a model.Activity) error { acts = append(acts, a); return nil }}); err != nil {
		t.Fatal(err)
	}
	if len(acts) != 2 || acts[0].Type != model.Deposit || acts[0].Base.Asset.Symbol != "DOGE" || acts[0].Network != "DOGE" ||
		acts[1].Type != model.Withdrawal || acts[1].Base.Qty.String() != "1" || acts[1].Fee.Qty.String() != "0.0002" || acts[1].Address != "Dshop" {
		t.Fatalf("activities %+v", acts)
	}
	bal, err := b.Balances(context.Background())
	if err != nil || len(bal) != 1 || bal[0].Qty.String() != "4" {
		t.Errorf("balances %+v %v", bal, err)
	}
}

// Live check of the public servers: FUNDARR_LIVE_UTXO=1 go test -run Live ./internal/source/bitcoin/
func TestLiveServers(t *testing.T) {
	if os.Getenv("FUNDARR_LIVE_UTXO") == "" {
		t.Skip("set FUNDARR_LIVE_UTXO=1")
	}
	for coin, addr := range map[string]struct {
		c Coin
		a string
	}{
		"ltc":  {Litecoin, "LQTpS3VaYTjCr4s9Y1t5zbeY26zevf7Fb3"},
		"doge": {Dogecoin, "DH5yaieqoZN36fDVciNyRueRGvGLR3mr7L"},
		"bch":  {BitcoinCash, "bitcoincash:qpm2qsznhks23z7629mms6s4cwef74vcwvy22gdx6a"},
	} {
		for _, sv := range addr.c.servers {
			c := &client{base: sv.url, blockbook: sv.blockbook, http: http.DefaultClient, sleep: sleepCtx}
			st, err := c.address(context.Background(), addr.a)
			t.Logf("%s %s: %d txs, %v", coin, sv.url, st.Chain.TxCount, err)
			if err != nil || st.Chain.TxCount == 0 {
				t.Errorf("%s %s does not answer", coin, sv.url)
			}
		}
	}
}

func TestSingleAddress(t *testing.T) {
	for _, tc := range []struct {
		c    Coin
		a    string
		good bool
	}{
		{Bitcoin, "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu", true},
		{Bitcoin, "1BpEi6DfDAUFd7GtittLSdBeYJvcoaVggu", true},
		{Bitcoin, "3J98t1WpEZ73CNmQviecrnyiWrnqRhWNLy", true},
		{Bitcoin, "bc1p5cyxnuxmeuwuvkwfem96lqzszd02n6xdcjrs20cac6yqjjwudpxqkedrcr", true},
		{Bitcoin, "1BpEi6DfDAUFd7GtittLSdBeYJvcoaVggv", false},
		{Bitcoin, "LQTpS3VaYTjCr4s9Y1t5zbeY26zevf7Fb3", false},
		{Litecoin, "LQTpS3VaYTjCr4s9Y1t5zbeY26zevf7Fb3", true},
		{Litecoin, "bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu", false},
		{Dogecoin, "DH5yaieqoZN36fDVciNyRueRGvGLR3mr7L", true},
		{BitcoinCash, "1BpEi6DfDAUFd7GtittLSdBeYJvcoaVggu", false},
	} {
		if got := tc.c.ValidAddress(tc.a); got != tc.good {
			t.Errorf("%s %s: %v", tc.c.Name, tc.a, got)
		}
	}
	// A one-address wallet reads that address only.
	_, srv, a := setup(t)
	s, err := NewCoin(Bitcoin, source.Config{Values: map[string]string{"address": a["r0"], "esplora_url": srv.URL}, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	b := s.(*Wallet)
	b.c.pause = 0
	sc, err := b.scan(context.Background(), nil)
	if err != nil || len(sc.owned) != 1 || !sc.owned[a["r0"]] {
		t.Errorf("scan %+v %v", sc, err)
	}
	if err := b.Test(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestPlainXpubScansUsedFormatsOnly(t *testing.T) {
	f, srv, _ := setup(t)
	xpub := reversion(zpub84, verXpub)
	s, err := NewCoin(Bitcoin, source.Config{Values: map[string]string{"xpub": xpub, "esplora_url": srv.URL}, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	b := s.(*Wallet)
	b.c.pause = 0
	bal, err := b.Balances(context.Background())
	if err != nil || len(bal) != 1 {
		t.Fatalf("balances %+v %v", bal, err)
	}
	// Only index 0 of the unused formats was asked.
	k, _, _ := Bitcoin.parseKey(xpub)
	for _, typ := range []scriptType{p2tr, p2shP2wpkh, p2pkh} {
		a1, _ := Bitcoin.derive(k, typ, 0, 1)
		a0, _ := Bitcoin.derive(k, typ, 0, 0)
		if f.calls["/address/"+a1] != 0 || f.calls["/address/"+a0] != 1 {
			t.Errorf("%s scanned", typ)
		}
	}
}
