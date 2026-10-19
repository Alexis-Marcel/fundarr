// Package wallet is one source for a whole self-custody wallet (a Ledger, a
// Trezor…): every account it holds, one key or address per line, each read
// on its own blockchain by the matching connector.
package wallet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/internal/source/bitcoin"
	"github.com/Alexis-Marcel/fundarr/internal/source/cardano"
	"github.com/Alexis-Marcel/fundarr/internal/source/evm"
	"github.com/Alexis-Marcel/fundarr/internal/source/solana"
	"github.com/Alexis-Marcel/fundarr/internal/source/tron"
	"github.com/Alexis-Marcel/fundarr/internal/source/xrp"
)

func init() {
	source.Register(source.Definition{
		Kind:        "wallet",
		Name:        "Wallet",
		Category:    "wallet",
		Description: "A hardware wallet (Ledger, Trezor…) or an app (MetaMask, Phantom, Trust Wallet, Exodus…), every account in one source: Bitcoin, Ethereum and EVM networks, Solana, XRP, Cardano, Tron, Litecoin, Dogecoin, Bitcoin Cash. Read-only.",
		Fields: []source.Field{
			{Name: "accounts", Label: "Accounts", Secret: true, Lines: true,
				Help: "Bitcoin, Litecoin, Dogecoin, Bitcoin Cash: the account's extended public key (Ledger Live: open the account, then ⋯, Edit account, Advanced). " +
					"A wallet app that shows a single Bitcoin address (Phantom…): that address.\n" +
					"Ethereum: the account's 0x… address. Fundarr reads it on every network it supports: Ethereum, Arbitrum, Base, Optimism, Polygon, Gnosis, Avalanche.\n" +
					"Tron: the account's T… address (TRX and tokens such as USDT).\n" +
					"Cardano: a receive address of the account (addr1…), or its stake address (stake1…); staking rewards included.\n" +
					"XRP: the account's r… address.\n" +
					"Solana: the account's address (SOL and well-known tokens)."},
			{Name: "esplora_url", Label: "Bitcoin server", Optional: true, Advanced: true,
				Help: "Your own mempool or electrs, for privacy. By default: mempool.space, then blockstream.info."},
			{Name: "solana_rpc", Label: "Solana RPC", Secret: true, Optional: true, Advanced: true,
				Help: "A private RPC URL (Helius, QuickNode…) reads a large Solana history much faster than the public one."},
			{Name: "etherscan_key", Label: "Etherscan API key", Secret: true, Optional: true, Advanced: true,
				Help: "Adds BNB Chain, which no free explorer covers, with an Etherscan plan that includes it."},
		},
		Setup: "Fundarr only reads public keys and addresses: it sees the history, it can never spend. Add each account of the wallet below; " +
			"Fundarr recognizes its blockchain. Never type your recovery phrase into Fundarr or any website: it is never needed to read.",
		New:    New,
		Detect: Detect,
	})
}

// account is one line of the list, read by its own connector.
type account struct {
	id    string // stable: derived from the key, so the list can be reordered
	label string // shown as the activity's account
	src   source.Source
}

type Wallet struct {
	accounts []account
	st       map[string]json.RawMessage
}

// chain is a blockchain the wallet can hold accounts on.
type chain struct {
	kind  string // account ids and labels
	name  string // for people
	label string // the account's name when it is alone of its kind
	// match recognizes a line of this chain (after any "kind:" prefix).
	match func(line string) bool
	// norm: lines that differ only in what norm erases are one account.
	norm  func(line string) string
	build func(line string, cfg source.Config) (source.Source, error)
}

func keyPrefix(ps ...string) func(string) bool {
	return func(l string) bool {
		for _, p := range ps {
			if len(l) > 100 && strings.HasPrefix(l, p) {
				return true
			}
		}
		return false
	}
}

func utxo(c bitcoin.Coin, esplora bool) func(string, source.Config) (source.Source, error) {
	return func(line string, cfg source.Config) (source.Source, error) {
		v := map[string]string{"xpub": line}
		if esplora {
			v["esplora_url"] = cfg.Get("esplora_url")
		}
		return bitcoin.NewCoin(c, source.Config{Values: v, HTTP: cfg.HTTP, State: cfg.State})
	}
}

func utxoAddress(c bitcoin.Coin, esplora bool) func(string, source.Config) (source.Source, error) {
	return func(line string, cfg source.Config) (source.Source, error) {
		v := map[string]string{"address": line}
		if esplora {
			v["esplora_url"] = cfg.Get("esplora_url")
		}
		return bitcoin.NewCoin(c, source.Config{Values: v, HTTP: cfg.HTTP, State: cfg.State})
	}
}

// chains, in the order lines are recognized. A prefix ("ltc:", "bch:"…)
// picks the chain when the line alone cannot tell: Ledger Live shows the
// Bitcoin Cash key as an xpub, like Bitcoin's.
var chains = []chain{
	{kind: "bitcoin", name: "Bitcoin", label: "bitcoin", match: keyPrefix("xpub", "ypub", "zpub"), build: utxo(bitcoin.Bitcoin, true)},
	{kind: "litecoin", name: "Litecoin", label: "litecoin", match: keyPrefix("Ltub", "Mtub"), build: utxo(bitcoin.Litecoin, false)},
	{kind: "dogecoin", name: "Dogecoin", label: "dogecoin", match: keyPrefix("dgub"), build: utxo(bitcoin.Dogecoin, false)},
	{kind: "bitcoincash", name: "Bitcoin Cash", label: "bitcoin cash", match: func(string) bool { return false }, build: utxo(bitcoin.BitcoinCash, false)},
	{kind: "evm", name: "Ethereum & EVM networks", match: func(l string) bool { return strings.HasPrefix(l, "0x") || strings.HasPrefix(l, "0X") },
		norm: strings.ToLower,
		build: func(line string, cfg source.Config) (source.Source, error) {
			return evm.New(source.Config{HTTP: cfg.HTTP, State: cfg.State, Values: map[string]string{"address": line,
				"chains": evm.All, "etherscan_key": cfg.Get("etherscan_key"), "base_url": cfg.Get("evm_base_url")}})
		}},
	{kind: "tron", name: "Tron", label: "tron", match: tron.Valid, build: address(tron.New)},
	{kind: "cardano", name: "Cardano", label: "cardano", match: cardano.Valid, build: address(cardano.New)},
	{kind: "xrp", name: "XRP", label: "xrp", match: xrp.Valid, build: address(xrp.New)},
	// One address: wallets that use a single one (Phantom, mobile wallets).
	{kind: "bitcoin", name: "Bitcoin", label: "bitcoin", match: bitcoin.Bitcoin.ValidAddress, build: utxoAddress(bitcoin.Bitcoin, true)},
	{kind: "litecoin", name: "Litecoin", label: "litecoin", match: bitcoin.Litecoin.ValidAddress, build: utxoAddress(bitcoin.Litecoin, false)},
	{kind: "dogecoin", name: "Dogecoin", label: "dogecoin", match: bitcoin.Dogecoin.ValidAddress, build: utxoAddress(bitcoin.Dogecoin, false)},
	// Last: any 32 bytes in base58 look like a Solana address.
	{kind: "solana", name: "Solana", match: solana.Valid,
		build: func(line string, cfg source.Config) (source.Source, error) {
			return solana.New(source.Config{HTTP: cfg.HTTP, State: cfg.State,
				Values: map[string]string{"address": line, "base_url": cfg.Get("solana_rpc")}})
		}},
}

// address builds an account read from one address, by the chain's package.
func address(build func(source.Config) (source.Source, error)) func(string, source.Config) (source.Source, error) {
	return func(line string, cfg source.Config) (source.Source, error) {
		return build(source.Config{HTTP: cfg.HTTP, State: cfg.State, Values: map[string]string{"address": line}})
	}
}

// prefixes name a chain explicitly, before the key: "bch:xpub…".
var prefixes = map[string]string{
	"btc": "bitcoin", "bitcoin": "bitcoin",
	"ltc": "litecoin", "litecoin": "litecoin",
	"doge": "dogecoin", "dogecoin": "dogecoin",
	"bch": "bitcoincash", "bitcoincash": "bitcoincash",
	"eth": "evm", "evm": "evm",
	"trx": "tron", "tron": "tron",
	"ada": "cardano", "cardano": "cardano",
	"xrp": "xrp",
	"sol": "solana", "solana": "solana",
}

// detect tells which chain a line belongs to, and the key without prefix.
func detect(line string) (*chain, string, error) {
	if i := strings.Index(line, ":"); i > 0 {
		if k, ok := prefixes[strings.ToLower(strings.TrimSpace(line[:i]))]; ok {
			rest := strings.TrimSpace(line[i+1:])
			// The entry of that chain that recognizes the value, else its
			// first (an xpub read as another chain).
			var first *chain
			for j := range chains {
				if chains[j].kind != k {
					continue
				}
				if chains[j].match(rest) {
					return &chains[j], rest, nil
				}
				if first == nil {
					first = &chains[j]
				}
			}
			return first, rest, nil
		}
	}
	for j := range chains {
		if chains[j].match(line) {
			return &chains[j], line, nil
		}
	}
	return nil, "", errors.New("Fundarr does not recognize this key or address")
}

// short prefixes, by chain kind, used to store a chain picked by the user.
var short = map[string]string{"bitcoin": "btc", "litecoin": "ltc", "dogecoin": "doge", "bitcoincash": "bch", "evm": "evm", "tron": "tron", "cardano": "cardano", "xrp": "xrp", "solana": "solana"}

// Detect recognizes one account and checks it can be read.
func Detect(line string) (source.Detected, error) {
	line = strings.TrimSpace(line)
	c, key, err := detect(line)
	if err != nil {
		return source.Detected{}, err
	}
	if _, err := c.build(key, source.Config{Values: map[string]string{}}); err != nil {
		return source.Detected{}, err
	}
	d := source.Detected{Chain: c.name, Value: line}
	// A plain xpub can be any of the Bitcoin-like chains.
	if strings.HasPrefix(key, "xpub") {
		for _, k := range []string{"bitcoin", "bitcoincash", "litecoin", "dogecoin"} {
			for j := range chains {
				if chains[j].kind != k {
					continue
				}
				if _, err := chains[j].build(key, source.Config{Values: map[string]string{}}); err == nil {
					v := short[k] + ":" + key
					if k == "bitcoin" {
						v = key
					}
					d.Choices = append(d.Choices, source.Choice{Chain: chains[j].name, Value: v})
				}
			}
		}
	}
	return d, nil
}

// Lines splits the accounts field: one per line, blank lines and # comments
// skipped.
func Lines(raw string) []string {
	var out []string
	for _, l := range strings.Split(raw, "\n") {
		l = strings.TrimSpace(l)
		if l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

func New(cfg source.Config) (source.Source, error) {
	lines := Lines(cfg.Get("accounts"))
	if len(lines) == 0 {
		return nil, errors.New("add at least one account")
	}
	w := &Wallet{st: map[string]json.RawMessage{}}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &w.st); err != nil {
			return nil, fmt.Errorf("wallet: bad saved state: %w", err)
		}
	}
	type parsed struct {
		c        *chain
		line, id string
	}
	var ps []parsed
	count := map[string]int{}
	seen := map[string]bool{}
	for i, l := range lines {
		c, key, err := detect(l)
		if err != nil {
			return nil, fmt.Errorf("account %d: %w", i+1, err)
		}
		norm := key
		if c.norm != nil {
			norm = c.norm(key)
		}
		if seen[c.kind+norm] {
			return nil, fmt.Errorf("account %d: this account is already in the list", i+1)
		}
		seen[c.kind+norm] = true
		sum := sha256.Sum256([]byte(norm))
		ps = append(ps, parsed{c, key, c.kind + "-" + hex.EncodeToString(sum[:4])})
		count[c.kind]++
	}
	for i, p := range ps {
		label := p.c.label
		if count[p.c.kind] > 1 {
			// Several accounts of a chain: the end of the key tells them apart.
			tail := "…" + p.line[len(p.line)-4:]
			if p.c.norm != nil {
				tail = p.c.norm(tail)
			}
			label = strings.TrimSpace(label + " " + tail)
		}
		s, err := p.c.build(p.line, source.Config{Values: cfg.Values, HTTP: cfg.HTTP, State: w.st[p.id]})
		if err != nil {
			return nil, fmt.Errorf("account %d: %w", i+1, err)
		}
		w.accounts = append(w.accounts, account{p.id, label, s})
	}
	return w, nil
}

func (w *Wallet) State() []byte {
	for _, a := range w.accounts {
		w.st[a.id] = a.src.State()
	}
	// Accounts removed from the list leave nothing behind.
	keep := map[string]bool{}
	for _, a := range w.accounts {
		keep[a.id] = true
	}
	for id := range w.st {
		if !keep[id] {
			delete(w.st, id)
		}
	}
	b, _ := json.Marshal(w.st)
	return b
}

// account names an activity or balance: the chain for EVM networks, with
// the account's label when there are several.
func named(label, sub string) string {
	switch {
	case label == "":
		return sub
	case sub == "" || sub == label:
		return label
	}
	return sub + " " + label
}

func (w *Wallet) Activities(ctx context.Context, f source.Fetch) error {
	var all []model.Activity
	var errs []error
	for _, a := range w.accounts {
		a := a
		err := a.src.Activities(ctx, source.Fetch{Since: f.Since, Progress: f.Progress, Emit: func(x model.Activity) error {
			x.NativeID = a.id + ":" + x.NativeID
			x.Account = named(a.label, x.Account)
			all = append(all, x)
			return nil
		}})
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err)
		}
	}
	for _, x := range internal(all) {
		if err := f.Emit(x); err != nil {
			return err
		}
	}
	return errors.Join(errs...)
}

// internal merges a send from one account of the wallet to another (same
// transaction, same asset) into what it cost: the fee. The coins stayed in
// the wallet, so it is neither a withdrawal nor a deposit.
func internal(acts []model.Activity) []model.Activity {
	type key struct{ tx, asset string }
	outs, ins := map[key][]int{}, map[key][]int{}
	for i, a := range acts {
		if a.TxID == "" {
			continue
		}
		k := key{strings.ToLower(a.TxID), a.Base.Asset.Key()}
		switch a.Type {
		case model.Withdrawal:
			outs[k] = append(outs[k], i)
		case model.Deposit:
			ins[k] = append(ins[k], i)
		}
	}
	drop := map[int]bool{}
	var fees []model.Activity
	for k, os := range outs {
		is := ins[k]
		if len(os) != 1 || len(is) != 1 {
			continue
		}
		o, in := acts[os[0]], acts[is[0]]
		// Only between two accounts: a contract refunding part of what one
		// account sent, in the same transaction, is not a transfer.
		if owner(o) == owner(in) || in.Base.Qty.GreaterThan(o.Base.Qty) {
			continue
		}
		drop[os[0]], drop[is[0]] = true, true
		fee := o
		fee.Type, fee.Address, fee.Fee = model.Fee, "", nil
		fee.Note = "sent to " + nameOr(in.Account, "another account") + " of the same wallet"
		fee.Base = model.Amount{Asset: o.Base.Asset, Qty: o.Base.Qty.Sub(in.Base.Qty)}
		if o.Fee != nil && o.Fee.Asset.Key() == o.Base.Asset.Key() {
			fee.Base.Qty = fee.Base.Qty.Add(o.Fee.Qty)
		} else if o.Fee != nil && o.Fee.Qty.IsPositive() {
			gas := fee
			gas.NativeID += ":fee"
			gas.Base = *o.Fee
			fees = append(fees, gas)
		}
		if fee.Base.Qty.IsPositive() {
			fees = append(fees, fee)
		}
	}
	out := make([]model.Activity, 0, len(acts))
	for i, a := range acts {
		if !drop[i] {
			out = append(out, a)
		}
	}
	out = append(out, fees...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out
}

// owner is the account an activity came from (its id prefixes NativeID).
func owner(a model.Activity) string {
	id, _, _ := strings.Cut(a.NativeID, ":")
	return id
}

func nameOr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (w *Wallet) Balances(ctx context.Context) ([]model.Balance, error) {
	var out []model.Balance
	for _, a := range w.accounts {
		bs, err := a.src.Balances(ctx)
		if err != nil {
			return nil, err
		}
		for _, b := range bs {
			b.Account = named(a.label, b.Account)
			out = append(out, b)
		}
	}
	return out, nil
}

// Test checks every account answers.
func (w *Wallet) Test(ctx context.Context) error {
	for _, a := range w.accounts {
		if t, ok := a.src.(source.Tester); ok {
			if err := t.Test(ctx); err != nil {
				return fmt.Errorf("%s: %w", nameOr(a.label, a.id), err)
			}
		}
	}
	return nil
}
