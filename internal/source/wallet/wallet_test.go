package wallet

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
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
		"bc1qcr8te4kr609gcawutmrza0j4xv80jy8z306fyu": "xpub",
		"hello":  "not a supported",
		"0x1234": "line 1",
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
