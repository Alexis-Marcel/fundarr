// Package bitcoin reads a Bitcoin wallet from its extended public key (xpub,
// ypub or zpub): addresses are derived locally, history and balances come
// from an Esplora API (mempool.space by default, or your own instance).
// Litecoin, Dogecoin and Bitcoin Cash are read the same way (see Coin).
package bitcoin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/btcsuite/btcd/btcutil/base58"
	"github.com/btcsuite/btcd/btcutil/hdkeychain"
	"github.com/shopspring/decimal"
)

const (
	defaultEsplora = "https://mempool.space/api"
	// GapLimit is how many unused addresses in a row end a chain (BIP44).
	GapLimit = 20
	pageSize = 25 // confirmed txs per Esplora page
)

func init() {
	source.Register(source.Definition{
		Kind:        "bitcoin",
		Name:        "Bitcoin wallet",
		Category:    "wallet",
		Hidden:      true, // the Wallet source covers it
		Description: "Bitcoin from an extended public key (Ledger, Trezor, Sparrow…): read-only, no private key.",
		Fields: []source.Field{
			{Name: "xpub", Label: "Extended public key", Secret: true,
				Help: "xpub, ypub or zpub of the account. Ledger Live: account → ⋯ → Edit account → Advanced"},
			{Name: "esplora_url", Label: "Esplora API", Optional: true,
				Help: "default https://mempool.space/api, then https://blockstream.info/api if it does not answer; point to your own mempool/electrs for privacy"},
		},
		Setup: "Fundarr only reads the account's extended public key: it can list addresses and transactions, never spend. " +
			"Never type your 24 words anywhere but on the device itself. An extended public key reveals the whole history " +
			"of the account, so Fundarr stores it encrypted; for full privacy, point it to your own Esplora (mempool or electrs).",
		New: New,
	})
}

// scriptType is an address format derived from the same key.
type scriptType string

const (
	p2pkh      scriptType = "p2pkh"       // legacy, 1…
	p2shP2wpkh scriptType = "p2sh-p2wpkh" // nested segwit, 3…
	p2wpkh     scriptType = "p2wpkh"      // native segwit, bc1q…
	p2tr       scriptType = "p2tr"        // taproot, bc1p…
)

// parseKey decodes an xpub/ypub/zpub and says which address formats to scan.
// A plain xpub does not say its format (Ledger Live shows xpub for every
// account type), so all four are scanned and the ones with activity kept.
func parseKey(s string) (*hdkeychain.ExtendedKey, []scriptType, error) { return Bitcoin.parseKey(s) }

func (c Coin) parseKey(s string) (*hdkeychain.ExtendedKey, []scriptType, error) {
	s = strings.TrimSpace(s)
	raw, ver, err := base58.CheckDecode(s)
	if err != nil || len(raw) != 77 {
		return nil, nil, fmt.Errorf("not a %s extended public key", c.Name)
	}
	var v [4]byte
	v[0] = ver
	copy(v[1:], raw[:3])
	types, ok := c.keys[v]
	if !ok {
		return nil, nil, fmt.Errorf("not a %s extended public key (private keys and testnet keys are refused)", c.Name)
	}
	// Re-encode with xpub version bytes so hdkeychain accepts it.
	buf := append([]byte{verXpub[1], verXpub[2], verXpub[3]}, raw[3:]...)
	k, err := hdkeychain.NewKeyFromString(base58.CheckEncode(buf, verXpub[0]))
	if err != nil {
		return nil, nil, fmt.Errorf("extended public key: %w", err)
	}
	if k.IsPrivate() {
		return nil, nil, errors.New("this is a private key: give the public one (xpub/ypub/zpub)")
	}
	return k, types, nil
}

// Wallet is a configured account of a Coin.
type Wallet struct {
	coin Coin
	// addr: a wallet that uses one address only (Phantom, some mobile
	// wallets) is read from it; key is nil then.
	addr  string
	key   *hdkeychain.ExtendedKey
	types []scriptType
	c     *client
	st    state

	scanned *scan // filled by the first scan of a run
	now     func() time.Time
}

type state struct {
	// TxCounts: confirmed tx count of each used address when its history
	// was last fetched; unchanged addresses are skipped next time.
	TxCounts map[string]int `json:"txCounts,omitempty"`
}

// New builds a Bitcoin wallet from its settings.
func New(cfg source.Config) (source.Source, error) { return NewCoin(Bitcoin, cfg) }

// NewCoin builds a wallet of any Coin: cfg "xpub" is the account key (or
// "address" its only address), and "esplora_url" (Bitcoin and Litecoin)
// replaces the public servers.
func NewCoin(coin Coin, cfg source.Config) (source.Source, error) {
	var key *hdkeychain.ExtendedKey
	var types []scriptType
	addr := strings.TrimSpace(cfg.Get("address"))
	if addr != "" {
		if !coin.ValidAddress(addr) {
			return nil, fmt.Errorf("not a %s address", coin.Name)
		}
	} else {
		var err error
		if key, types, err = coin.parseKey(cfg.Get("xpub")); err != nil {
			return nil, err
		}
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.Get("esplora_url")), "/")
	var first server
	var fallback []server
	if base == "" {
		first, fallback = coin.servers[0], append([]server(nil), coin.servers[1:]...)
	} else {
		if !strings.HasPrefix(base, "http") {
			base = "https://" + base
		}
		first = server{url: base}
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	b := &Wallet{coin: coin, addr: addr, key: key, types: types, now: time.Now,
		c: &client{base: first.url, blockbook: first.blockbook, fallback: fallback, http: hc, pause: 60 * time.Millisecond, sleep: sleepCtx}}
	b.c.skipDown()
	if len(cfg.State) > 0 {
		_ = json.Unmarshal(cfg.State, &b.st)
	}
	if b.st.TxCounts == nil {
		b.st.TxCounts = map[string]int{}
	}
	return b, nil
}

func (b *Wallet) State() []byte {
	raw, _ := json.Marshal(b.st)
	return raw
}

// Test checks the key and that the Esplora API answers.
func (b *Wallet) Test(ctx context.Context) error {
	addr := b.addr
	if b.key != nil {
		var err error
		if addr, err = b.coin.derive(b.key, b.types[0], 0, 0); err != nil {
			return err
		}
	}
	_, err := b.c.address(ctx, addr)
	return err
}

// ---- scanning

type used struct {
	addr  string
	stats addrStats
}

type scan struct {
	owned map[string]bool // every derived address (used or within the gap)
	used  []used
}

func (b *Wallet) scan(ctx context.Context, progress func(string, ...any)) (*scan, error) {
	if b.scanned != nil {
		return b.scanned, nil
	}
	s := &scan{owned: map[string]bool{}}
	if b.key == nil {
		st, err := b.c.address(ctx, b.addr)
		if err != nil {
			return nil, err
		}
		s.owned[b.addr] = true
		if st.Chain.TxCount > 0 || st.Mempool.TxCount > 0 {
			s.used = append(s.used, used{addr: b.addr, stats: st})
		}
		b.scanned = s
		return s, nil
	}
	types, err := b.activeTypes(ctx)
	if err != nil {
		return nil, err
	}
	total := 0
	for _, t := range types {
		for chain := uint32(0); chain <= 1; chain++ {
			gap := 0
			// Addresses are asked GapLimit at a time, in parallel; the gap
			// is then counted in order.
			for start := uint32(0); gap < GapLimit; start += GapLimit {
				addrs := make([]string, GapLimit)
				for i := range addrs {
					a, err := b.coin.derive(b.key, t, chain, start+uint32(i))
					if err != nil {
						return nil, err
					}
					addrs[i] = a
				}
				stats, err := b.c.addresses(ctx, addrs)
				if err != nil {
					return nil, err
				}
				for i, addr := range addrs {
					if gap >= GapLimit {
						break
					}
					total++
					s.owned[addr] = true
					if st := stats[i]; st.Chain.TxCount > 0 || st.Mempool.TxCount > 0 {
						s.used = append(s.used, used{addr: addr, stats: st})
						gap = 0
					} else {
						gap++
					}
				}
			}
		}
		if progress != nil {
			progress("%s: %s scanned, %d addresses checked, %d used", b.coin.Name, t, total, len(s.used))
		}
	}
	b.scanned = s
	return s, nil
}

// activeTypes keeps the address formats the account used. Wallets start
// at index 0, so a format whose first receive and change addresses never
// saw a transaction was never used: a plain xpub (the four formats) costs
// eight requests to sort out instead of four full scans.
func (b *Wallet) activeTypes(ctx context.Context) ([]scriptType, error) {
	if len(b.types) == 1 {
		return b.types, nil
	}
	var addrs []string
	for _, t := range b.types {
		for chain := uint32(0); chain <= 1; chain++ {
			a, err := b.coin.derive(b.key, t, chain, 0)
			if err != nil {
				return nil, err
			}
			addrs = append(addrs, a)
		}
	}
	stats, err := b.c.addresses(ctx, addrs)
	if err != nil {
		return nil, err
	}
	var out []scriptType
	for i, t := range b.types {
		r, c := stats[2*i], stats[2*i+1]
		if r.Chain.TxCount+r.Mempool.TxCount+c.Chain.TxCount+c.Mempool.TxCount > 0 {
			out = append(out, t)
		}
	}
	return out, nil
}

// ---- activities

func sats(v int64) decimal.Decimal { return decimal.New(v, -8) }

func (b *Wallet) Activities(ctx context.Context, f source.Fetch) error {
	progress := f.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	s, err := b.scan(ctx, progress)
	if err != nil {
		return err
	}
	txs := map[string]tx{}
	fetched := 0
	for _, u := range s.used {
		count := u.stats.Chain.TxCount
		if count == 0 {
			continue
		}
		if !f.Since.IsZero() && b.st.TxCounts[u.addr] == count {
			continue // nothing new on this address
		}
		list, err := b.c.chainTxs(ctx, u.addr)
		if err != nil {
			return err
		}
		fetched += len(list)
		for _, t := range list {
			txs[t.TxID] = t
		}
		b.st.TxCounts[u.addr] = count
	}
	progress("%s: %d transactions read", b.coin.Name, fetched)

	ids := make([]string, 0, len(txs))
	for id := range txs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, a := range b.coin.classify(txs[id], s.owned) {
			if err := f.Emit(a); err != nil {
				return err
			}
		}
	}
	return nil
}

func classify(t tx, owned map[string]bool) []model.Activity { return Bitcoin.classify(t, owned) }

// classify turns one transaction into what it means for the wallet.
func (c Coin) classify(t tx, owned map[string]bool) []model.Activity {
	btc := c.asset()
	if !t.Status.Confirmed || t.Status.BlockTime == 0 {
		return nil
	}
	var ins, outs int64
	firstOwnOut, firstForeignOut := "", ""
	for _, in := range t.Vin {
		if in.Prevout != nil && owned[in.Prevout.Address] {
			ins += in.Prevout.Value
		}
	}
	for _, o := range t.Vout {
		if owned[o.Address] {
			outs += o.Value
			if firstOwnOut == "" {
				firstOwnOut = o.Address
			}
		} else if firstForeignOut == "" && o.Address != "" {
			firstForeignOut = o.Address
		}
	}
	at := time.Unix(t.Status.BlockTime, 0).UTC()
	base := model.Activity{NativeID: t.TxID, Time: at, Network: c.Symbol, TxID: t.TxID}
	if ins == 0 && outs == 0 {
		return nil
	}
	delta := outs - ins
	if delta >= 0 {
		// Received (or, for a joint transaction, received more than paid).
		if delta == 0 {
			return nil
		}
		a := base
		a.Type = model.Deposit
		a.Base = model.Amount{Asset: btc, Qty: sats(delta)}
		a.Address = firstOwnOut
		return []model.Activity{a}
	}
	lost := -delta
	fee := t.Fee
	if fee > lost {
		fee = lost
	}
	sent := lost - fee
	if sent == 0 {
		a := base
		a.Type = model.Fee
		a.Base = model.Amount{Asset: btc, Qty: sats(fee)}
		a.Note = "consolidation or transfer between own addresses"
		return []model.Activity{a}
	}
	a := base
	a.Type = model.Withdrawal
	a.Base = model.Amount{Asset: btc, Qty: sats(sent)}
	a.Address = firstForeignOut
	if fee > 0 {
		a.Fee = &model.Amount{Asset: btc, Qty: sats(fee)}
	}
	return []model.Activity{a}
}

// ---- balances

// Balances sums the confirmed funds of every used address.
func (b *Wallet) Balances(ctx context.Context) ([]model.Balance, error) {
	s, err := b.scan(ctx, nil)
	if err != nil {
		return nil, err
	}
	var total int64
	for _, u := range s.used {
		total += u.stats.Chain.Funded - u.stats.Chain.Spent
	}
	if total == 0 {
		return nil, nil
	}
	return []model.Balance{{Amount: model.Amount{Asset: b.coin.asset(), Qty: sats(total)}}}, nil
}
