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
	"github.com/Alexis-Marcel/fundarr/internal/source/evm"
)

func init() {
	source.Register(source.Definition{
		Kind:        "wallet",
		Name:        "Wallet (Ledger, Trezor…)",
		Category:    "wallet",
		Description: "Every account of a hardware or software wallet in one source: Bitcoin, Ethereum and EVM networks. Read-only.",
		Fields: []source.Field{
			{Name: "accounts", Label: "Accounts", Secret: true, Lines: true,
				Help: "One per line. Bitcoin: the account's xpub, ypub or zpub (Ledger Live: account → ⋯ → Edit account → Advanced). " +
					"Ethereum and EVM: the 0x… address, read on every network (" + strings.Join(evm.Networks(), ", ") + ")."},
			{Name: "esplora_url", Label: "Bitcoin Esplora API", Optional: true,
				Help: "default https://mempool.space/api, then https://blockstream.info/api; point to your own mempool/electrs for privacy"},
			{Name: "etherscan_key", Label: "Etherscan API key", Secret: true, Optional: true,
				Help: "optional: reads Ethereum networks through Etherscan v2 instead of Blockscout"},
		},
		Setup: "Fundarr only reads public keys and addresses: it can list the history, never spend. Each blockchain has its own keys, " +
			"so add one line per account you see in Ledger Live (or your wallet app). Never type your recovery words anywhere but on " +
			"the device itself. Keys reveal the whole history of an account, so Fundarr stores them encrypted.",
		New: New,
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

// kind tells which blockchain a line belongs to.
func kind(line string) (string, error) {
	switch {
	case strings.HasPrefix(line, "0x") || strings.HasPrefix(line, "0X"):
		return "evm", nil
	case len(line) > 100 && (strings.HasPrefix(line, "xpub") || strings.HasPrefix(line, "ypub") || strings.HasPrefix(line, "zpub")):
		return "bitcoin", nil
	case strings.HasPrefix(line, "bc1") || strings.HasPrefix(line, "1") || strings.HasPrefix(line, "3"):
		return "", errors.New("a single Bitcoin address only shows part of an account: give the account's xpub, ypub or zpub instead")
	}
	return "", errors.New("not a supported key or address (Bitcoin xpub/ypub/zpub, or a 0x… address)")
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
	type parsed struct{ kind, line, id string }
	var ps []parsed
	count := map[string]int{}
	seen := map[string]bool{}
	for i, l := range lines {
		k, err := kind(l)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
		norm := l
		if k == "evm" {
			norm = strings.ToLower(l)
		}
		if seen[norm] {
			return nil, fmt.Errorf("line %d: this account is already in the list", i+1)
		}
		seen[norm] = true
		sum := sha256.Sum256([]byte(norm))
		ps = append(ps, parsed{k, l, k + "-" + hex.EncodeToString(sum[:4])})
		count[k]++
	}
	for i, p := range ps {
		sub := source.Config{HTTP: cfg.HTTP, State: w.st[p.id]}
		var label string
		var err error
		switch p.kind {
		case "bitcoin":
			sub.Values = map[string]string{"xpub": p.line, "esplora_url": cfg.Get("esplora_url")}
			label = "bitcoin"
			if count["bitcoin"] > 1 {
				label += " …" + p.line[len(p.line)-4:]
			}
			var s source.Source
			if s, err = bitcoin.New(sub); err == nil {
				w.accounts = append(w.accounts, account{p.id, label, s})
			}
		case "evm":
			sub.Values = map[string]string{"address": p.line, "chains": evm.All, "etherscan_key": cfg.Get("etherscan_key"),
				"base_url": cfg.Get("evm_base_url")}
			if count["evm"] > 1 {
				label = "…" + strings.ToLower(p.line[len(p.line)-4:])
			}
			var s source.Source
			if s, err = evm.New(sub); err == nil {
				w.accounts = append(w.accounts, account{p.id, label, s})
			}
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", i+1, err)
		}
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
		if in.Base.Qty.GreaterThan(o.Base.Qty) {
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
