package wallet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/internal/source/bitcoin"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// BIP84 test vector, account 0.
const zpub = "zpub6rFR7y4Q2AijBEqTUquhVz398htDFrtymD9xYYfG1m4wAcvPhXNfE3EfH1r1ADqtfSdVCToUG868RvUUkgDKf31mGDtKsAYz2oz2AGutZYs"

const addr = "0x52908400098527886E0F7030069857D2E4169EE7" // EIP-55 example

func TestNewParsesTheList(t *testing.T) {
	s, err := New(source.Config{Values: map[string]string{"accounts": "# my Ledger\n" + zpub + "\n\n  " + addr + "  \n"}})
	if err != nil {
		t.Fatal(err)
	}
	w := s.(*Wallet)
	if len(w.accounts) != 2 || !strings.HasPrefix(w.accounts[0].id, "bitcoin-") || !strings.HasPrefix(w.accounts[1].id, "evm-") {
		t.Fatalf("accounts %+v", w.accounts)
	}
	if w.accounts[0].label != "bitcoin" || w.accounts[1].label != "" {
		t.Errorf("labels %q %q", w.accounts[0].label, w.accounts[1].label)
	}
	// Ids depend on the key, not on the order.
	s2, _ := New(source.Config{Values: map[string]string{"accounts": strings.ToLower(addr) + "\n" + zpub}})
	if s2.(*Wallet).accounts[1].id != w.accounts[0].id || s2.(*Wallet).accounts[0].id != w.accounts[1].id {
		t.Error("ids must not depend on order or case")
	}
	for in, want := range map[string]string{
		"":                                  "at least one",
		zpub + "\n" + zpub:                  "already",
		addr + "\n" + strings.ToLower(addr): "already",
		"hello":                             "does not recognize",
		"0x1234":                            "account 1",
	} {
		if _, err := New(source.Config{Values: map[string]string{"accounts": in}}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", in, err)
		}
	}
}

// fake is an account answering fixed activities and balances.
type fake struct {
	acts  []model.Activity
	bals  []model.Balance
	state string
}

func (f *fake) Activities(_ context.Context, fe source.Fetch) error {
	for _, a := range f.acts {
		if err := fe.Emit(a); err != nil {
			return err
		}
	}
	return nil
}
func (f *fake) Balances(context.Context) ([]model.Balance, error) { return f.bals, nil }
func (f *fake) State() []byte                                     { return []byte(`"` + f.state + `"`) }

func btc(q string) model.Amount {
	return model.Amount{Asset: model.Asset{Kind: model.Crypto, Symbol: "BTC"}, Qty: decimal.RequireFromString(q)}
}

func TestSendBetweenOwnAccountsIsAFee(t *testing.T) {
	at := time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC)
	fee := btc("0.0001")
	a := &fake{acts: []model.Activity{
		{NativeID: "tx1", Type: model.Withdrawal, Time: at, TxID: "ABC", Base: btc("0.5"), Fee: &fee},
		{NativeID: "tx2", Type: model.Withdrawal, Time: at.Add(time.Hour), TxID: "DEF", Base: btc("0.1")},
	}, bals: []model.Balance{{Amount: btc("1")}}, state: "a"}
	b := &fake{acts: []model.Activity{
		{NativeID: "tx1", Type: model.Deposit, Time: at, TxID: "abc", Base: btc("0.5")},
	}, bals: []model.Balance{{Amount: btc("0.5")}}, state: "b"}
	w := &Wallet{st: map[string]json.RawMessage{"gone": json.RawMessage(`1`)},
		accounts: []account{{"bitcoin-a", "bitcoin …aaaa", a}, {"bitcoin-b", "bitcoin …bbbb", b}}}
	var got []model.Activity
	if err := w.Activities(context.Background(), source.Fetch{Emit: func(x model.Activity) error { got = append(got, x); return nil }}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %+v", got)
	}
	f, out := got[0], got[1]
	if f.Type != model.Fee || f.NativeID != "bitcoin-a:tx1" || !f.Base.Qty.Equal(decimal.RequireFromString("0.0001")) ||
		f.Fee != nil || !strings.Contains(f.Note, "bitcoin …bbbb") {
		t.Errorf("fee %+v", f)
	}
	if out.Type != model.Withdrawal || out.NativeID != "bitcoin-a:tx2" || out.Account != "bitcoin …aaaa" {
		t.Errorf("withdrawal %+v", out)
	}
	bals, _ := w.Balances(context.Background())
	if len(bals) != 2 || bals[1].Account != "bitcoin …bbbb" {
		t.Errorf("balances %+v", bals)
	}
	var st map[string]string
	if err := json.Unmarshal(w.State(), &st); err != nil || st["bitcoin-a"] != "a" || st["bitcoin-b"] != "b" || len(st) != 2 {
		t.Errorf("state %s", w.State())
	}
}

func TestTokenSentBetweenOwnAccountsKeepsItsGas(t *testing.T) {
	at := time.Now().UTC()
	usdc := model.Amount{Asset: model.Asset{Kind: model.Crypto, Symbol: "USDC"}, Qty: decimal.NewFromInt(100)}
	gas := model.Amount{Asset: model.Asset{Kind: model.Crypto, Symbol: "ETH"}, Qty: decimal.RequireFromString("0.001")}
	// One account getting part of what it sent back is not a transfer.
	if got := internal([]model.Activity{
		{NativeID: "x:out", Type: model.Withdrawal, Time: at, TxID: "0x9", Base: usdc},
		{NativeID: "x:refund", Type: model.Deposit, Time: at, TxID: "0x9", Base: usdc},
	}); len(got) != 2 {
		t.Errorf("same account merged %+v", got)
	}
	got := internal([]model.Activity{
		{NativeID: "x:out", Type: model.Withdrawal, Time: at, TxID: "0x1", Base: usdc, Fee: &gas},
		{NativeID: "y:in", Type: model.Deposit, Time: at, TxID: "0x1", Base: usdc},
	})
	if len(got) != 1 || got[0].Base.Asset.Symbol != "ETH" || got[0].NativeID != "x:out:fee" {
		t.Fatalf("got %+v", got)
	}
	// More arriving than leaving is not one transfer: kept as is.
	more := usdc
	more.Qty = decimal.NewFromInt(200)
	if got := internal([]model.Activity{
		{NativeID: "o", Type: model.Withdrawal, Time: at, TxID: "0x2", Base: usdc},
		{NativeID: "i", Type: model.Deposit, Time: at, TxID: "0x2", Base: more},
	}); len(got) != 2 {
		t.Errorf("kept %+v", got)
	}
}

func TestDetect(t *testing.T) {
	d, err := Detect("  " + zpub + " ")
	if err != nil || d.Chain != "Bitcoin" || d.Value != zpub || len(d.Choices) != 0 {
		t.Errorf("zpub %+v %v", d, err)
	}
	// A plain xpub can be read on every Bitcoin-like chain.
	xpub := "xpub6BgBgsespWvERF3LHQu6CnqdvfEvtMcQjYrcRzx53QJjSxarj2afYWcLteoGVky7D3UKDP9QyrLprQ3VCECoY49yfdDEHGCtMMj92pReUsQ"
	d, err = Detect(xpub)
	if err != nil || d.Chain != "Bitcoin" || len(d.Choices) != 4 || d.Choices[1].Value != "bch:"+xpub || d.Choices[1].Chain != "Bitcoin Cash" {
		t.Errorf("xpub %+v %v", d, err)
	}
	if d, err := Detect("bch:" + xpub); err != nil || d.Chain != "Bitcoin Cash" {
		t.Errorf("bch %+v %v", d, err)
	}
	if d, err := Detect(addr); err != nil || d.Chain != "Ethereum & EVM networks" {
		t.Errorf("evm %+v %v", d, err)
	}
	for _, bad := range []string{"bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyv", "0x12", "ltc:" + addr} {
		if _, err := Detect(bad); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestDetectTron(t *testing.T) {
	if d, err := Detect("TEgfk1gjsMdXZYm9743PWYuM6Fc5MGJYeq"); err != nil || d.Chain != "Tron" {
		t.Errorf("tron %+v %v", d, err)
	}
	if _, err := Detect("TEgfk1gjsMdXZYm9743PWYuM6Fc5MGJYeQ"); err == nil {
		t.Error("bad checksum accepted")
	}
}

func TestDetectXRPAndCardano(t *testing.T) {
	if d, err := Detect("rMxCKbEDwqr76QuheSUMdEGf4B9xJ8m5De"); err != nil || d.Chain != "XRP" {
		t.Errorf("xrp %+v %v", d, err)
	}
	if _, err := Detect("stake1uy"); err == nil {
		t.Error("broken stake address accepted")
	}
}

func TestDetectSolana(t *testing.T) {
	if d, err := Detect("EB7KkCBCvAETtkPMU9a2zCwZK51az1V6hm7Bgs1Xeqfs"); err != nil || d.Chain != "Solana" {
		t.Errorf("solana %+v %v", d, err)
	}
	// Tron and XRP addresses stay theirs.
	if d, _ := Detect("TEgfk1gjsMdXZYm9743PWYuM6Fc5MGJYeq"); d.Chain != "Tron" {
		t.Errorf("tron read as %s", d.Chain)
	}
}

func TestDetectSingleAddresses(t *testing.T) {
	for in, want := range map[string]string{
		"bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu":     "Bitcoin",
		"btc:bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu": "Bitcoin",
		"LQTpS3VaYTjCr4s9Y1t5zbeY26zevf7Fb3":             "Litecoin",
		"DH5yaieqoZN36fDVciNyRueRGvGLR3mr7L":             "Dogecoin",
		"rMxCKbEDwqr76QuheSUMdEGf4B9xJ8m5De":             "XRP",
	} {
		if d, err := Detect(in); err != nil || d.Chain != want {
			t.Errorf("%s: %+v %v", in, d, err)
		}
	}
}

func TestNicknames(t *testing.T) {
	s, err := New(source.Config{Values: map[string]string{"accounts": zpub + "\tSavings\n" + addr + "\tMetaMask"}})
	if err != nil {
		t.Fatal(err)
	}
	w := s.(*Wallet)
	if w.accounts[0].label != "Savings" || w.accounts[1].label != "MetaMask" {
		t.Errorf("labels %q %q", w.accounts[0].label, w.accounts[1].label)
	}
	// The name does not change the account.
	s2, _ := New(source.Config{Values: map[string]string{"accounts": zpub}})
	if s2.(*Wallet).accounts[0].id != w.accounts[0].id {
		t.Error("a name must not change the id")
	}
	if d, err := Detect(zpub + "\tSavings"); err != nil || d.Kind != "bitcoin" || d.Value != zpub {
		t.Errorf("detect with name %+v %v", d, err)
	}
	if _, err := Preview(context.Background(), "nope", source.Config{}); err == nil {
		t.Error("preview of junk")
	}
}

// esplora is a node of one chain: it answers its genesis hash and empty
// addresses, and remembers which addresses it was asked.
type esplora struct {
	genesis string
	mu      sync.Mutex
	asked   []string
}

func (e *esplora) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/block-height/0" {
		fmt.Fprint(w, e.genesis)
		return
	}
	e.mu.Lock()
	e.asked = append(e.asked, strings.TrimPrefix(r.URL.Path, "/address/"))
	e.mu.Unlock()
	fmt.Fprint(w, `{}`)
}

func TestOwnServerPerChain(t *testing.T) {
	btcNode, ltcNode := &esplora{genesis: bitcoin.Bitcoin.Genesis}, &esplora{genesis: bitcoin.Litecoin.Genesis}
	btcSrv, ltcSrv := httptest.NewServer(btcNode), httptest.NewServer(ltcNode)
	defer btcSrv.Close()
	defer ltcSrv.Close()
	// The BIP84 test key written as a Litecoin Ltub, and a Litecoin address.
	raw, _, _ := base58.CheckDecode(zpub)
	ltub := base58.CheckEncode(append([]byte{0x9d, 0xa4, 0x62}, raw[3:]...), 0x01)
	accounts := zpub + "\n" + ltub + "\nLQTpS3VaYTjCr4s9Y1t5zbeY26zevf7Fb3"
	build := func(btcURL, ltcURL string) source.Source {
		t.Helper()
		s, err := New(source.Config{HTTP: btcSrv.Client(), Values: map[string]string{
			"accounts": accounts, "esplora_url": btcURL, "litecoin_esplora_url": ltcURL}})
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	s := build(btcSrv.URL, ltcSrv.URL)
	if err := s.(source.Tester).Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Balances(context.Background()); err != nil {
		t.Fatal(err)
	}
	// Each node is asked about its own chain only: no fallback, no mix.
	for _, a := range btcNode.asked {
		if !strings.HasPrefix(a, "bc1") {
			t.Errorf("bitcoin node asked %s", a)
		}
	}
	ltc := 0
	for _, a := range ltcNode.asked {
		if strings.HasPrefix(a, "bc1") || strings.HasPrefix(a, "1") || strings.HasPrefix(a, "3") {
			t.Errorf("litecoin node asked %s", a)
		}
		if a == "LQTpS3VaYTjCr4s9Y1t5zbeY26zevf7Fb3" {
			ltc++
		}
	}
	if len(btcNode.asked) == 0 || ltc == 0 {
		t.Errorf("asked: bitcoin %d, litecoin %v", len(btcNode.asked), ltcNode.asked)
	}
	// Servers swapped: Test names the account and the wrong network.
	err := build(ltcSrv.URL, btcSrv.URL).(source.Tester).Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "bitcoin: wrong network") {
		t.Errorf("swapped: %v", err)
	}
}
