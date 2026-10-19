package coinbase

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

const testKeyName = "organizations/org-1/apiKeys/key-1"

// testKey is a generated key of either kind, with its secret as Coinbase
// hands it out.
type testKey struct {
	secret string
	verify func(msg, sig []byte) bool
	alg    string
}

func ecKey(t *testing.T) testKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalECPrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
	return testKey{secret: pemText, alg: "ES256", verify: func(msg, sig []byte) bool {
		if len(sig) != 64 {
			return false
		}
		h := sha256.Sum256(msg)
		return ecdsa.Verify(&k.PublicKey, h[:], new(big.Int).SetBytes(sig[:32]), new(big.Int).SetBytes(sig[32:]))
	}}
}

func edKey(t *testing.T) testKey {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return testKey{secret: base64.StdEncoding.EncodeToString(priv), alg: "EdDSA", verify: func(msg, sig []byte) bool {
		return ed25519.Verify(pub, msg, sig)
	}}
}

// fake is a Coinbase server serving documented response shapes. Every
// request must carry a valid, fresh JWT for its own method, host and path.
type fake struct {
	t      *testing.T
	srv    *httptest.Server
	key    testKey
	now    time.Time
	mu     sync.Mutex
	routes map[string]func(r *http.Request) (int, any)
	calls  map[string]int
	nonces map[string]bool
}

func newFake(t *testing.T, key testKey, now time.Time) *fake {
	f := &fake{t: t, key: key, now: now, routes: map[string]func(*http.Request) (int, any){}, calls: map[string]int{}, nonces: map[string]bool{}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) checkJWT(r *http.Request) error {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		return fmt.Errorf("no bearer token")
	}
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return fmt.Errorf("token has %d parts", len(parts))
	}
	enc := base64.RawURLEncoding
	hb, _ := enc.DecodeString(parts[0])
	cb, _ := enc.DecodeString(parts[1])
	sig, _ := enc.DecodeString(parts[2])
	var h struct{ Alg, Kid, Nonce, Typ string }
	var c struct {
		Iss, Sub, URI string
		Nbf, Exp      int64
	}
	if json.Unmarshal(hb, &h) != nil || json.Unmarshal(cb, &c) != nil {
		return fmt.Errorf("bad token json")
	}
	if !f.key.verify([]byte(parts[0]+"."+parts[1]), sig) {
		return fmt.Errorf("bad signature")
	}
	if h.Alg != f.key.alg || h.Kid != testKeyName || h.Typ != "JWT" || len(h.Nonce) != 32 {
		return fmt.Errorf("bad header %+v", h)
	}
	if f.nonces[h.Nonce] {
		return fmt.Errorf("nonce reused")
	}
	f.nonces[h.Nonce] = true
	want := "GET " + r.Host + r.URL.Path
	if c.Iss != "cdp" || c.Sub != testKeyName || c.URI != want {
		return fmt.Errorf("bad claims %+v, want uri %q", c, want)
	}
	if c.Nbf != f.now.Unix() || c.Exp != c.Nbf+120 {
		return fmt.Errorf("bad validity nbf=%d exp=%d", c.Nbf, c.Exp)
	}
	return nil
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	err := f.checkJWT(r)
	f.calls[r.URL.Path]++
	h := f.routes[r.URL.Path]
	f.mu.Unlock()
	if err != nil {
		f.t.Errorf("%s: %v", r.URL.Path, err)
		w.WriteHeader(http.StatusUnauthorized)
		json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"id": "authentication_error", "message": "invalid signature"}}})
		return
	}
	if h == nil {
		w.WriteHeader(http.StatusNotFound)
		json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]any{"id": "not_found", "message": "Not found"}}})
		return
	}
	status, body := h(r)
	if status == http.StatusTooManyRequests {
		w.Header().Set("Retry-After", "3")
	}
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(body)
}

func (f *fake) on(path string, h func(r *http.Request) (int, any)) { f.routes[path] = h }

// list serves items newest first, size per page, with next_uri cursors.
func (f *fake) list(path string, size int, items func() []map[string]any) {
	f.on(path, func(r *http.Request) (int, any) {
		all := items()
		start := 0
		if after := r.URL.Query().Get("starting_after"); after != "" {
			for i, it := range all {
				if it["id"] == after {
					start = i + 1
				}
			}
		}
		end := min(start+size, len(all))
		next := any(nil)
		if end < len(all) {
			next = path + "?limit=100&starting_after=" + all[end-1]["id"].(string)
		}
		return 200, map[string]any{"pagination": map[string]any{"limit": size, "order": "desc", "next_uri": next}, "data": all[start:end]}
	})
}

func (f *fake) source(t *testing.T, state []byte) *Source {
	return f.sourceWith(t, f.key.secret, state)
}

func (f *fake) sourceWith(t *testing.T, secret string, state []byte) *Source {
	s, err := New(source.Config{Values: map[string]string{"api_key": testKeyName, "api_secret": secret}, HTTP: f.srv.Client(), State: state})
	if err != nil {
		t.Fatal(err)
	}
	src := s.(*Source)
	if err := src.setBase(f.srv.URL); err != nil {
		t.Fatal(err)
	}
	src.c.now = func() time.Time { return f.now }
	src.now = func() time.Time { return f.now }
	src.c.sleep = func(context.Context, time.Duration) error { return nil }
	return src
}

var t0 = time.Date(2024, 4, 10, 12, 0, 0, 0, time.UTC)

func at(h int) string { return t0.Add(time.Duration(h) * time.Hour).Format(time.RFC3339) }

func m(amount, ccy string) map[string]any { return map[string]any{"amount": amount, "currency": ccy} }

func txn(id, typ string, h int, amount, ccy string, extra map[string]any) map[string]any {
	t := map[string]any{"id": id, "type": typ, "status": "completed", "amount": m(amount, ccy),
		"native_amount": m("1.00", "EUR"), "description": nil, "created_at": at(h), "resource": "transaction"}
	for k, v := range extra {
		t[k] = v
	}
	return t
}

func acct(id, name, typ, ccy, ctype, bal string) map[string]any {
	return map[string]any{"id": id, "name": name, "type": typ, "primary": false,
		"currency": map[string]any{"code": ccy, "type": ctype}, "balance": m(bal, ccy)}
}

func fillOf(order, side, price, comm string) map[string]any {
	return map[string]any{"advanced_trade_fill": map[string]any{"fill_price": price, "product_id": "BTC-EUR", "order_id": order, "commission": comm, "order_side": side}}
}

// scenario wires a consistent account: replaying the history must give the
// balances. Transactions are listed newest first, as Coinbase does.
func scenario(f *fake) map[string][]map[string]any {
	txs := map[string][]map[string]any{
		"a-eur": {
			txn("e5", "fiat_withdrawal", 9, "-50.00", "EUR", nil),
			txn("e4", "advanced_trade_fill", 7, "-300.60", "EUR", fillOf("O1", "BUY", "30000", "0.6")),
			txn("e3", "sell", 3, "98.50", "EUR", map[string]any{"sell": map[string]any{"id": "S1"}}),
			txn("e2", "buy", 2, "-101.49", "EUR", map[string]any{"buy": map[string]any{"id": "B1"}}),
			txn("e1", "fiat_deposit", 1, "1000.00", "EUR", nil),
		},
		"a-btc": {
			txn("b9", "trade", 12, "-0.00100000", "BTC", map[string]any{"trade": map[string]any{"id": "C1"}}),
			txn("b8", "send", 11, "-0.00050000", "BTC", map[string]any{"to": map[string]any{"id": "a-vault", "resource": "account", "resource_path": "/v2/accounts/a-vault"}}),
			txn("b7", "transfer", 10, "-0.00100000", "BTC", map[string]any{"to": map[string]any{"id": "a-vault", "resource": "account"}}),
			txn("b6", "send", 8, "0.00500000", "BTC", map[string]any{"network": map[string]any{"status": "confirmed", "hash": "0xin", "network_name": "bitcoin"}}),
			txn("b5", "send", 8, "-0.00133000", "BTC", map[string]any{"idem": "x",
				"network": map[string]any{"status": "confirmed", "hash": "0xout", "network_name": "bitcoin", "transaction_fee": m("0.00033", "BTC"), "transaction_amount": m("0.001", "BTC")},
				"to":      map[string]any{"resource": "bitcoin_address", "address": "bc1qdest"}}),
			txn("b4", "advanced_trade_fill", 7, "0.01000000", "BTC", fillOf("O1", "BUY", "30000", "0.6")),
			txn("b3", "buy", 4, "0.00100000", "BTC", map[string]any{"buy": map[string]any{"id": "B2",
				"total": m("51.49", "EUR"), "subtotal": m("50.00", "EUR"), "fee": m("1.49", "EUR"), "payment_method_name": "Visa"}}),
			txn("b2", "sell", 3, "-0.00200000", "BTC", map[string]any{"sell": map[string]any{"id": "S1",
				"total": m("98.50", "EUR"), "subtotal": m("100.00", "EUR"), "fee": m("1.50", "EUR")}}),
			txn("b1", "buy", 2, "0.00200000", "BTC", map[string]any{"buy": map[string]any{"id": "B1",
				"total": m("101.49", "EUR"), "subtotal": m("100.00", "EUR"), "fee": m("1.49", "EUR")}}),
		},
		"a-vault": {
			txn("v2", "send", 11, "0.00050000", "BTC", map[string]any{"from": map[string]any{"id": "a-btc", "resource": "account"}}),
			txn("v1", "transfer", 10, "0.00100000", "BTC", map[string]any{"from": map[string]any{"id": "a-btc", "resource": "account"}}),
		},
		"a-eth": {
			txn("h4", "staking_reward", 30*24-1, "0.001", "ETH", nil),
			txn("h3", "send", 30*24-2, "-0.01", "ETH", map[string]any{"status": "pending"}),
			txn("h2", "staking_reward", 13, "0.001", "ETH", nil),
			txn("h1", "trade", 12, "0.05", "ETH", map[string]any{"trade": map[string]any{"id": "C1"}}),
		},
	}
	accounts := []map[string]any{
		acct("a-eur", "EUR Wallet", "fiat", "EUR", "fiat", "646.41"),
		acct("a-btc", "BTC Wallet", "wallet", "BTC", "crypto", "0.01217000"),
		acct("a-vault", "BTC Vault", "vault", "BTC", "crypto", "0.00150000"),
		acct("a-eth", "ETH Wallet", "wallet", "ETH", "crypto", "0.05200000"),
		acct("a-sol", "SOL Wallet", "wallet", "SOL", "crypto", "0"),
	}
	f.list("/v2/accounts", 2, func() []map[string]any { return accounts })
	for id := range txs {
		f.list("/v2/accounts/"+id+"/transactions", 3, func() []map[string]any { return txs[id] })
	}
	f.list("/v2/accounts/a-sol/transactions", 3, func() []map[string]any { return nil })
	return txs
}

func collect(t *testing.T, s *Source) ([]model.Activity, []string) {
	t.Helper()
	var acts []model.Activity
	var log []string
	err := s.Activities(context.Background(), source.Fetch{
		Emit:     func(a model.Activity) error { acts = append(acts, a); return nil },
		Progress: func(format string, args ...any) { log = append(log, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return acts, log
}

func byID(acts []model.Activity) map[string]model.Activity {
	out := map[string]model.Activity{}
	for _, a := range acts {
		if _, dup := out[a.NativeID]; dup {
			panic("duplicate native id " + a.NativeID)
		}
		out[a.NativeID] = a
	}
	return out
}

func eq(t *testing.T, what string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(decimal.RequireFromString(want)) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

func TestHistoryAndReconcile(t *testing.T) {
	for _, k := range []struct {
		name string
		key  func(*testing.T) testKey
	}{{"ecdsa", ecKey}, {"ed25519", edKey}} {
		t.Run(k.name, func(t *testing.T) {
			now := t0.Add(30 * 24 * time.Hour)
			f := newFake(t, k.key(t), now)
			scenario(f)
			s := f.source(t, nil)
			acts, log := collect(t, s)
			got := byID(acts)

			b1 := got["buy:B1"]
			if b1.Type != model.Trade || b1.Side != model.Buy || b1.Quote == nil || b1.Fee == nil {
				t.Fatalf("buy B1 = %+v", b1)
			}
			eq(t, "B1 base", b1.Base.Qty, "0.002")
			eq(t, "B1 quote", b1.Quote.Qty, "100")
			eq(t, "B1 fee", b1.Fee.Qty, "1.49")
			if b1.Quote.Asset.Kind != model.Fiat || b1.Fee.Asset.Symbol != "EUR" {
				t.Errorf("B1 quote asset %+v", b1.Quote.Asset)
			}
			if _, ok := got["buy:B1:funding"]; ok {
				t.Error("B1 was paid from the EUR wallet: no funding deposit")
			}

			card, fund := got["buy:B2"], got["buy:B2:funding"]
			eq(t, "card quote", card.Quote.Qty, "50")
			eq(t, "card fee", card.Fee.Qty, "1.49")
			if fund.Type != model.Deposit {
				t.Errorf("card funding = %+v", fund)
			}
			eq(t, "card funding", fund.Base.Qty, "51.49")

			s1 := got["sell:S1"]
			if s1.Side != model.Sell {
				t.Errorf("sell = %+v", s1)
			}
			eq(t, "sell base", s1.Base.Qty, "0.002")
			eq(t, "sell quote", s1.Quote.Qty, "100")
			eq(t, "sell fee", s1.Fee.Qty, "1.5")

			conv := got["convert:C1"]
			if conv.Type != model.Trade || conv.Side != model.Buy || conv.Base.Asset.Symbol != "ETH" || conv.Quote.Asset.Symbol != "BTC" {
				t.Errorf("convert = %+v", conv)
			}
			eq(t, "convert base", conv.Base.Qty, "0.05")
			eq(t, "convert quote", conv.Quote.Qty, "0.001")

			fill := got["fill:O1:"+fmt.Sprint(t0.Add(7*time.Hour).Unix())]
			if fill.Side != model.Buy || fill.Quote == nil || fill.Fee == nil {
				t.Fatalf("fill = %+v", fill)
			}
			eq(t, "fill base", fill.Base.Qty, "0.01")
			eq(t, "fill quote", fill.Quote.Qty, "300")
			eq(t, "fill fee", fill.Fee.Qty, "0.6")

			out := got["tx:b5"]
			if out.Type != model.Withdrawal || out.TxID != "0xout" || out.Network != "bitcoin" || out.Address != "bc1qdest" || out.Fee == nil {
				t.Errorf("send out = %+v", out)
			}
			eq(t, "send out", out.Base.Qty, "0.001")
			eq(t, "send fee", out.Fee.Qty, "0.00033")
			in := got["tx:b6"]
			if in.Type != model.Deposit || in.TxID != "0xin" {
				t.Errorf("send in = %+v", in)
			}
			if r := got["tx:h2"]; r.Type != model.Reward || r.Base.Asset.Symbol != "ETH" {
				t.Errorf("reward = %+v", r)
			}
			if d := got["tx:e1"]; d.Type != model.Deposit || d.Base.Asset.Kind != model.Fiat {
				t.Errorf("fiat deposit = %+v", d)
			}
			if w := got["tx:e5"]; w.Type != model.Withdrawal {
				t.Errorf("fiat withdrawal = %+v", w)
			}
			for _, id := range []string{"tx:b7", "tx:b8", "tx:v1", "tx:v2", "tx:h3"} {
				if _, ok := got[id]; ok {
					t.Errorf("%s is internal or pending: not an activity", id)
				}
			}
			if !strings.Contains(strings.Join(log, "\n"), "moves between your own Coinbase accounts ignored") {
				t.Errorf("log: %v", log)
			}

			bal, err := s.Balances(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if len(bal) != 3 {
				t.Errorf("balances = %+v (SOL is empty, vault summed with wallet)", bal)
			}
			if gaps := model.Reconcile(acts, bal, decimal.Zero); len(gaps) > 0 {
				t.Errorf("history does not add up to balances: %+v", gaps)
			}
			// Pagination: accounts on 3 pages, BTC ledger on 3.
			if f.calls["/v2/accounts"] != 6 || f.calls["/v2/accounts/a-btc/transactions"] != 3 {
				t.Errorf("calls = %v", f.calls)
			}
		})
	}
}

func TestIncremental(t *testing.T) {
	now := t0.Add(30 * 24 * time.Hour)
	f := newFake(t, ecKey(t), now)
	txs := scenario(f)
	s := f.source(t, nil)
	collect(t, s)
	var st state
	if err := json.Unmarshal(s.State(), &st); err != nil {
		t.Fatal(err)
	}
	if !st.Accounts["a-btc"].Equal(t0.Add(12 * time.Hour)) {
		t.Errorf("btc resumes at %v", st.Accounts["a-btc"])
	}
	// The pending send keeps the ETH ledger open from its time, before the
	// newer reward.
	if !st.Accounts["a-eth"].Equal(t0.Add((30*24 - 2) * time.Hour)) {
		t.Errorf("eth resumes at %v", st.Accounts["a-eth"])
	}

	// Later: a new reward, and the send has completed.
	f.now = now.Add(10 * 24 * time.Hour)
	txs["a-btc"] = append([]map[string]any{txn("b10", "earn_payout", 30*24+5, "0.0001", "BTC", nil)}, txs["a-btc"]...)
	txs["a-eth"][1]["status"] = "completed"
	s2 := f.source(t, s.State())
	got := byID(func() []model.Activity { a, _ := collect(t, s2); return a }())
	if r := got["tx:b10"]; r.Type != model.Reward {
		t.Errorf("new reward = %+v", r)
	}
	if w := got["tx:h3"]; w.Type != model.Withdrawal {
		t.Errorf("completed send = %+v", w)
	}

	// Once more: the BTC ledger is read only down to the overlap.
	txs["a-btc"] = append([]map[string]any{txn("b11", "earn_payout", 30*24+100, "0.0001", "BTC", nil)}, txs["a-btc"]...)
	f.calls = map[string]int{}
	got = byID(func() []model.Activity { a, _ := collect(t, f.source(t, s2.State())); return a }())
	if _, ok := got["tx:b11"]; !ok {
		t.Error("newest reward missing")
	}
	if _, ok := got["buy:B1"]; ok {
		t.Error("old history read again")
	}
	if n := f.calls["/v2/accounts/a-btc/transactions"]; n != 1 {
		t.Errorf("btc ledger read %d pages, want 1", n)
	}
}

func TestTradeLegWaitsForItsPair(t *testing.T) {
	now := t0.Add(30 * time.Minute)
	f := newFake(t, edKey(t), now)
	btc := []map[string]any{txn("b1", "advanced_trade_fill", 0, "0.01", "BTC", fillOf("O9", "BUY", "30000", "0.6"))}
	var eur []map[string]any
	f.list("/v2/accounts", 100, func() []map[string]any {
		return []map[string]any{acct("a-btc", "BTC", "wallet", "BTC", "crypto", "0.01"), acct("a-eur", "EUR", "fiat", "EUR", "fiat", "0")}
	})
	f.list("/v2/accounts/a-btc/transactions", 100, func() []map[string]any { return btc })
	f.list("/v2/accounts/a-eur/transactions", 100, func() []map[string]any { return eur })
	s := f.source(t, nil)
	acts, _ := collect(t, s)
	if len(acts) != 0 {
		t.Fatalf("a fresh fill with one leg must wait: %+v", acts)
	}
	// Its quote leg shows up later: the fill is read whole.
	eur = []map[string]any{txn("e1", "advanced_trade_fill", 0, "-300.60", "EUR", fillOf("O9", "BUY", "30000", "0.6"))}
	f.now = t0.Add(2 * time.Hour)
	acts, _ = collect(t, f.source(t, s.State()))
	if len(acts) != 1 || acts[0].Quote == nil {
		t.Fatalf("fill = %+v", acts)
	}
	eq(t, "quote", acts[0].Quote.Qty, "300")

	// A leg alone for longer than settle is read from its price.
	eur = nil
	acts, _ = collect(t, f.source(t, nil))
	if len(acts) != 1 || acts[0].Quote == nil {
		t.Fatalf("lone fill = %+v", acts)
	}
	eq(t, "computed quote", acts[0].Quote.Qty, "300")
	eq(t, "fee", acts[0].Fee.Qty, "0.6")
}

func TestUnknownTypeSkipped(t *testing.T) {
	f := newFake(t, ecKey(t), t0)
	f.list("/v2/accounts", 100, func() []map[string]any {
		return []map[string]any{acct("a-btc", "BTC", "wallet", "BTC", "crypto", "1")}
	})
	f.list("/v2/accounts/a-btc/transactions", 100, func() []map[string]any {
		return []map[string]any{
			txn("x1", "brand_new_type", -2, "0.5", "BTC", nil),
			txn("x2", "fiat_deposit", -3, "1", "BTC", nil),
		}
	})
	acts, log := collect(t, f.source(t, nil))
	if len(acts) != 1 || acts[0].NativeID != "tx:x2" {
		t.Errorf("acts = %+v", acts)
	}
	if !strings.Contains(strings.Join(log, "\n"), `skipped 1 "brand_new_type"`) {
		t.Errorf("log = %v", log)
	}
}

func TestRetry429(t *testing.T) {
	f := newFake(t, ecKey(t), t0)
	n := 0
	f.on("/v2/accounts", func(*http.Request) (int, any) {
		n++
		if n < 3 {
			return http.StatusTooManyRequests, map[string]any{"errors": []any{map[string]any{"id": "rate_limit_exceeded", "message": "Too many requests"}}}
		}
		return 200, map[string]any{"pagination": map[string]any{}, "data": []any{acct("a", "BTC", "wallet", "BTC", "crypto", "2")}}
	})
	s := f.source(t, nil)
	var waits []time.Duration
	s.c.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	bal, err := s.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(bal) != 1 || len(waits) != 2 || waits[0] != 3*time.Second {
		t.Errorf("bal=%v waits=%v", bal, waits)
	}

	// A server that never recovers gives up with Coinbase's message.
	f.on("/v2/accounts", func(*http.Request) (int, any) {
		return http.StatusServiceUnavailable, map[string]any{"errors": []any{map[string]any{"id": "internal_server_error", "message": "busy"}}}
	})
	if _, err := s.Balances(context.Background()); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("err = %v", err)
	}
}

func TestTest(t *testing.T) {
	f := newFake(t, ecKey(t), t0)
	perms := map[string]any{"can_view": true, "can_trade": false, "can_transfer": false, "portfolio_uuid": "p"}
	f.on("/api/v3/brokerage/key_permissions", func(*http.Request) (int, any) { return 200, perms })
	s := f.source(t, nil)
	if err := s.Test(context.Background()); err != nil {
		t.Fatal(err)
	}
	perms["can_trade"] = true
	if err := s.Test(context.Background()); err == nil || !strings.Contains(err.Error(), "View") {
		t.Errorf("trading key accepted: %v", err)
	}
	perms["can_trade"], perms["can_view"] = false, false
	if err := s.Test(context.Background()); err == nil {
		t.Error("key without view accepted")
	}

	// A key Coinbase does not know.
	f.on("/api/v3/brokerage/key_permissions", func(*http.Request) (int, any) {
		return http.StatusUnauthorized, map[string]any{"error": "unauthorized", "message": "Unauthorized"}
	})
	err := s.Test(context.Background())
	if err == nil || !strings.Contains(err.Error(), "refused the key") {
		t.Errorf("err = %v", err)
	}
	if strings.Contains(err.Error(), "Bearer") || strings.Contains(err.Error(), "eyJ") {
		t.Errorf("error leaks the token: %v", err)
	}
}

func TestKeyFormats(t *testing.T) {
	ec := ecKey(t)
	ed := edKey(t)
	edRaw, _ := base64.StdEncoding.DecodeString(ed.secret)
	for name, c := range map[string]struct {
		secret, alg string
	}{
		"pem":             {ec.secret, "ES256"},
		"pem literal \\n": {strings.ReplaceAll(strings.TrimSpace(ec.secret), "\n", `\n`), "ES256"},
		"pem one line":    {strings.ReplaceAll(ec.secret, "\n", ""), "ES256"},
		"pem quoted":      {`"` + strings.ReplaceAll(ec.secret, "\n", `\n`) + `"`, "ES256"},
		"ed25519":         {ed.secret, "EdDSA"},
		"ed25519 seed":    {base64.StdEncoding.EncodeToString(edRaw[:32]), "EdDSA"},
	} {
		sig, err := parseKey(c.secret)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		if sig.alg != c.alg {
			t.Errorf("%s: alg %s", name, sig.alg)
		}
	}
	for _, bad := range []string{"nope", "-----BEGIN EC PRIVATE KEY-----AAAA-----END EC PRIVATE KEY-----", base64.StdEncoding.EncodeToString(make([]byte, 64))} {
		if _, err := parseKey(bad); err == nil {
			t.Errorf("%q accepted", bad)
		} else if strings.Contains(err.Error(), bad) {
			t.Errorf("error quotes the key: %v", err)
		}
	}

	// The JSON file CDP downloads works as the secret, name included.
	f := newFake(t, ec, t0)
	f.on("/api/v3/brokerage/key_permissions", func(*http.Request) (int, any) { return 200, map[string]any{"can_view": true} })
	file, _ := json.Marshal(map[string]string{"name": testKeyName, "privateKey": ec.secret})
	s, err := New(source.Config{Values: map[string]string{"api_secret": string(file)}, HTTP: f.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	src := s.(*Source)
	src.setBase(f.srv.URL)
	src.c.now = func() time.Time { return t0 }
	if err := src.Test(context.Background()); err != nil {
		t.Error(err)
	}
}

func TestDefinition(t *testing.T) {
	d, err := source.Get("coinbase")
	if err != nil {
		t.Fatal(err)
	}
	if len(strings.Fields(d.Description)) > 10 {
		t.Errorf("description too long: %q", d.Description)
	}
	if _, err := d.New(source.Config{Values: map[string]string{"api_key": "k"}}); err == nil {
		t.Error("missing private key accepted")
	}
}
