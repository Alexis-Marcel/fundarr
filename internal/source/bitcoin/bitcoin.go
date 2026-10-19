// Package bitcoin reads a Bitcoin wallet from its extended public key (xpub,
// ypub or zpub): addresses are derived locally, history and balances come
// from an Esplora API (mempool.space by default, or your own instance).
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

// fallbackEsplora: public servers tried when the default does not answer.
var fallbackEsplora = []string{"https://blockstream.info/api"}

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

// Version bytes of the serialized extended public keys (SLIP-132).
var (
	verXpub = [4]byte{0x04, 0x88, 0xb2, 0x1e}
	verYpub = [4]byte{0x04, 0x9d, 0x7c, 0xb2}
	verZpub = [4]byte{0x04, 0xb2, 0x47, 0x46}
)

// parseKey decodes an xpub/ypub/zpub and says which address formats to scan.
// A plain xpub does not say its format (Ledger Live shows xpub for every
// account type), so all four are scanned and the ones with activity kept.
func parseKey(s string) (*hdkeychain.ExtendedKey, []scriptType, error) {
	s = strings.TrimSpace(s)
	raw, ver, err := base58.CheckDecode(s)
	if err != nil || len(raw) != 77 {
		return nil, nil, errors.New("not an extended public key (expected xpub…, ypub… or zpub…)")
	}
	var v [4]byte
	v[0] = ver
	copy(v[1:], raw[:3])
	var types []scriptType
	switch v {
	case verXpub:
		types = []scriptType{p2wpkh, p2tr, p2shP2wpkh, p2pkh}
	case verYpub:
		types = []scriptType{p2shP2wpkh}
	case verZpub:
		types = []scriptType{p2wpkh}
	default:
		return nil, nil, errors.New("only mainnet xpub, ypub and zpub keys are supported (private keys and testnet keys are refused)")
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

// Bitcoin is a configured wallet.
type Bitcoin struct {
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

// New builds the source from its settings.
func New(cfg source.Config) (source.Source, error) {
	key, types, err := parseKey(cfg.Get("xpub"))
	if err != nil {
		return nil, err
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.Get("esplora_url")), "/")
	var fallback []string
	if base == "" {
		base, fallback = defaultEsplora, append([]string(nil), fallbackEsplora...)
	} else if !strings.HasPrefix(base, "http") {
		base = "https://" + base
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	b := &Bitcoin{key: key, types: types, now: time.Now,
		c: &client{base: base, fallback: fallback, http: hc, pause: 120 * time.Millisecond, sleep: sleepCtx}}
	if len(cfg.State) > 0 {
		_ = json.Unmarshal(cfg.State, &b.st)
	}
	if b.st.TxCounts == nil {
		b.st.TxCounts = map[string]int{}
	}
	return b, nil
}

func (b *Bitcoin) State() []byte {
	raw, _ := json.Marshal(b.st)
	return raw
}

// Test checks the key and that the Esplora API answers.
func (b *Bitcoin) Test(ctx context.Context) error {
	addr, err := deriveAddress(b.key, b.types[0], 0, 0)
	if err != nil {
		return err
	}
	_, err = b.c.address(ctx, addr)
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

func (b *Bitcoin) scan(ctx context.Context, progress func(string, ...any)) (*scan, error) {
	if b.scanned != nil {
		return b.scanned, nil
	}
	s := &scan{owned: map[string]bool{}}
	total := 0
	for _, t := range b.types {
		for chain := uint32(0); chain <= 1; chain++ {
			gap := 0
			for i := uint32(0); gap < GapLimit; i++ {
				addr, err := deriveAddress(b.key, t, chain, i)
				if err != nil {
					return nil, err
				}
				st, err := b.c.address(ctx, addr)
				if err != nil {
					return nil, err
				}
				total++
				s.owned[addr] = true
				if st.Chain.TxCount > 0 || st.Mempool.TxCount > 0 {
					s.used = append(s.used, used{addr: addr, stats: st})
					gap = 0
				} else {
					gap++
				}
			}
		}
		if progress != nil {
			progress("Bitcoin: %s scanned, %d addresses checked, %d used", t, total, len(s.used))
		}
	}
	b.scanned = s
	return s, nil
}

// ---- activities

func sats(v int64) decimal.Decimal { return decimal.New(v, -8) }

var btc = model.Asset{Kind: model.Crypto, Symbol: "BTC"}

func (b *Bitcoin) Activities(ctx context.Context, f source.Fetch) error {
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
	progress("Bitcoin: %d transactions read", fetched)

	ids := make([]string, 0, len(txs))
	for id := range txs {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, a := range classify(txs[id], s.owned) {
			if err := f.Emit(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// classify turns one transaction into what it means for the wallet.
func classify(t tx, owned map[string]bool) []model.Activity {
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
	base := model.Activity{NativeID: t.TxID, Time: at, Network: "BTC", TxID: t.TxID}
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
func (b *Bitcoin) Balances(ctx context.Context) ([]model.Balance, error) {
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
	return []model.Balance{{Amount: model.Amount{Asset: btc, Qty: sats(total)}}}, nil
}
