// Package cardano reads a Cardano wallet from its stake address: every
// address of a Shelley wallet shares the stake credential, so the account's
// history, staking rewards and balance come from it, through a Koios API
// (api.koios.rest by default, free, an API key is optional).
package cardano

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

const (
	defaultKoios = "https://api.koios.rest/api/v1"
	network      = "cardano"
	lovelace     = 6  // ADA decimals
	batch        = 50 // tx hashes per tx_info call (Koios caps the body at 5 KB)
)

var ada = model.CryptoOrFiat("ADA")

// Shelley epochs last 5 days; epoch 208, the first one, started then.
var (
	shelleyStart = time.Date(2020, 7, 29, 21, 44, 51, 0, time.UTC)
	epochLength  = 5 * 24 * time.Hour
)

// epochStart is when an epoch (208 or later) began.
func epochStart(e int) time.Time {
	return shelleyStart.Add(time.Duration(e-208) * epochLength)
}

// token is a native asset worth following.
type token struct {
	Symbol   string
	Decimals int32
}

// known native tokens, by policy id + "." + asset name (hex), checked
// against the Cardano token registry. Anyone can mint a token, so anything
// else is treated as spam (airdropped NFTs, fake tickers).
var known = map[string]token{
	"8db269c3ec630e06ae29f74bc39edd1f87c819f1056206e879a1cd61.446a65644d6963726f555344": {"DJED", 6},
	"c48cbb3d5e57ed56e276bc45f99ab39abe94e6cd7ac39fb402da47ad.0014df105553444d":         {"USDM", 6},
	"f66d78b4a3cb3d37afa0ec36461e51ecbde00f26c8f0a68f94b69880.69555344":                 {"IUSD", 6},
}

// knownPolicies lists the policy ids of known, for filtering balances.
func knownPolicies() []string {
	seen := map[string]bool{}
	var out []string
	for k := range known {
		p, _, _ := strings.Cut(k, ".")
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// New builds the connector from its settings.
func New(cfg source.Config) (source.Source, error) {
	stake, err := stakeAddress(cfg.Get("address"))
	if err != nil {
		return nil, err
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.Get("base_url")), "/")
	if base == "" {
		base = defaultKoios
	}
	s := &Source{
		stake: stake,
		api: &client{
			base:  base,
			key:   strings.TrimSpace(cfg.Get("api_key")),
			http:  hc,
			pause: 250 * time.Millisecond,
			sleep: sleepCtx,
		},
		now: time.Now,
	}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &s.st); err != nil {
			return nil, fmt.Errorf("cardano: bad saved state: %w", err)
		}
	}
	return s, nil
}

// Source is a configured Cardano account.
type Source struct {
	stake string
	api   *client
	st    state
	now   func() time.Time
}

type state struct {
	// Height: last block height read; the next run starts there again
	// (the engine deduplicates).
	Height int64 `json:"height"`
	// RewardEpoch: last earned epoch emitted as a reward.
	RewardEpoch int `json:"rewardEpoch"`
}

func (s *Source) State() []byte {
	b, _ := json.Marshal(s.st)
	return b
}

// StakeAddress is the stake address read (derived from a base address).
func (s *Source) StakeAddress() string { return s.stake }

// ---- Koios rows

type assetQty struct {
	PolicyID  string `json:"policy_id"`
	AssetName string `json:"asset_name"`
	Quantity  string `json:"quantity"`
}

func (a assetQty) key() string { return a.PolicyID + "." + a.AssetName }

type utxo struct {
	Value       string `json:"value"`
	StakeAddr   string `json:"stake_addr"`
	PaymentAddr struct {
		Bech32 string `json:"bech32"`
	} `json:"payment_addr"`
	AssetList []assetQty `json:"asset_list"`
}

type txInfo struct {
	TxHash      string `json:"tx_hash"`
	BlockHeight int64  `json:"block_height"`
	Timestamp   int64  `json:"tx_timestamp"`
	Fee         string `json:"fee"`
	Deposit     string `json:"deposit"`
	Inputs      []utxo `json:"inputs"`
	Outputs     []utxo `json:"outputs"`
	Withdrawals []struct {
		Amount    string `json:"amount"`
		StakeAddr string `json:"stake_addr"`
	} `json:"withdrawals"`
	Certificates []struct {
		Type string `json:"type"`
		Info struct {
			StakeAddress string `json:"stake_address"`
		} `json:"info"`
	} `json:"certificates"`
}

type rewardRow struct {
	EarnedEpoch    int    `json:"earned_epoch"`
	SpendableEpoch int    `json:"spendable_epoch"`
	Amount         string `json:"amount"`
	Type           string `json:"type"`
}

func dec(s string) (decimal.Decimal, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, fmt.Errorf("bad amount %q", s)
	}
	return d, nil
}

// ---- history

func (s *Source) Activities(ctx context.Context, f source.Fetch) error {
	if f.Emit == nil {
		return errors.New("cardano: no Emit")
	}
	progress := f.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	emit := func(a model.Activity) error {
		a.Network = network
		return f.Emit(a)
	}

	// Transactions touching any address of the account, from the last block
	// read (included).
	progress("cardano: transactions")
	var hashes []string
	seen := map[string]bool{}
	height := s.st.Height
	q := url.Values{"_stake_address": {s.stake}, "_after_block_height": {strconv.FormatInt(s.st.Height, 10)},
		"order": {"block_height.asc,tx_hash.asc"}}
	err := s.api.rows(ctx, "account_txs", q, nil, func(raw json.RawMessage) error {
		var r struct {
			TxHash      string `json:"tx_hash"`
			BlockHeight int64  `json:"block_height"`
		}
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.TxHash != "" && !seen[r.TxHash] {
			seen[r.TxHash] = true
			hashes = append(hashes, r.TxHash)
		}
		height = max(height, r.BlockHeight)
		return nil
	})
	if err != nil {
		return fmt.Errorf("cardano: %w", err)
	}
	for i := 0; i < len(hashes); i += batch {
		chunk := hashes[i:min(i+batch, len(hashes))]
		progress("cardano: transaction details %d–%d of %d", i+1, i+len(chunk), len(hashes))
		var txs []txInfo
		body := map[string]any{"_tx_hashes": chunk, "_inputs": true, "_assets": true, "_withdrawals": true,
			"_certs": true, "_metadata": false, "_scripts": false, "_bytecode": false, "_governance": false}
		if err := s.api.call(ctx, "tx_info", nil, body, &txs); err != nil {
			return fmt.Errorf("cardano: %w", err)
		}
		sort.SliceStable(txs, func(a, b int) bool { return txs[a].BlockHeight < txs[b].BlockHeight })
		for _, t := range txs {
			acts, err := classify(t, s.stake)
			if err != nil {
				return fmt.Errorf("cardano: %s: %w", t.TxHash, err)
			}
			for _, a := range acts {
				if err := emit(a); err != nil {
					return err
				}
			}
		}
	}

	// Staking rewards, once they can be spent.
	progress("cardano: staking rewards")
	last, err := s.rewards(ctx, emit)
	if err != nil {
		return fmt.Errorf("cardano: %w", err)
	}
	s.st.Height, s.st.RewardEpoch = height, last
	return nil
}

// owns reports whether a stake address of a row is the account's.
func owns(stake, addr string) bool { return addr != "" && addr == stake }

// classify turns a transaction into what it did to the account: the net
// ADA moved (minus the fee when the account paid it), the stake key deposit
// paid or refunded, and the known tokens moved. Rewards withdrawn into the
// wallet are not new money (they were counted as Rewards), so they are
// taken out of the ADA received.
func classify(t txInfo, stake string) ([]model.Activity, error) {
	ins, outs := map[string]decimal.Decimal{}, map[string]decimal.Decimal{}
	paid := false
	ownAddr, foreign := "", ""
	add := func(m map[string]decimal.Decimal, u utxo) error {
		v, err := dec(u.Value)
		if err != nil {
			return err
		}
		m[""] = m[""].Add(v)
		for _, a := range u.AssetList {
			q, err := dec(a.Quantity)
			if err != nil {
				return err
			}
			m[a.key()] = m[a.key()].Add(q)
		}
		return nil
	}
	for _, u := range t.Inputs {
		if owns(stake, u.StakeAddr) {
			paid = true
			if err := add(ins, u); err != nil {
				return nil, err
			}
		}
	}
	for _, u := range t.Outputs {
		if owns(stake, u.StakeAddr) {
			if err := add(outs, u); err != nil {
				return nil, err
			}
			if ownAddr == "" {
				ownAddr = u.PaymentAddr.Bech32
			}
		} else if foreign == "" {
			foreign = u.PaymentAddr.Bech32
		}
	}
	withdrawn := decimal.Zero
	for _, w := range t.Withdrawals {
		if owns(stake, w.StakeAddr) {
			v, err := dec(w.Amount)
			if err != nil {
				return nil, err
			}
			withdrawn = withdrawn.Add(v)
		}
	}
	// The deposit (positive when paid, negative when refunded) is the
	// account's when one of its certificates is in the transaction.
	deposit, certified := decimal.Zero, false
	for _, c := range t.Certificates {
		if owns(stake, c.Info.StakeAddress) {
			certified = true
		}
	}
	if certified {
		d, err := dec(t.Deposit)
		if err != nil {
			return nil, err
		}
		deposit = d
	}
	fee, err := dec(t.Fee)
	if err != nil {
		return nil, err
	}

	at := time.Unix(t.Timestamp, 0).UTC()
	base := model.Activity{Time: at, TxID: t.TxHash}
	amt := func(a model.Asset, units decimal.Decimal, d int32) model.Amount {
		return model.Amount{Asset: a, Qty: units.Shift(-d)}
	}
	var acts []model.Activity

	switch {
	case deposit.IsPositive():
		a := base
		a.NativeID, a.Type, a.Note = t.TxHash+":deposit", model.Withdrawal, "stake key deposit"
		a.Base = amt(ada, deposit, lovelace)
		acts = append(acts, a)
	case deposit.IsNegative():
		a := base
		a.NativeID, a.Type, a.Note = t.TxHash+":deposit", model.Deposit, "stake key deposit refund"
		a.Base = amt(ada, deposit.Neg(), lovelace)
		acts = append(acts, a)
	}

	// Net ADA, the deposit apart: received − sent − fee.
	delta := outs[""].Sub(ins[""]).Sub(withdrawn).Add(deposit)
	a := base
	a.NativeID = t.TxHash + ":ada"
	switch {
	case delta.IsPositive():
		// Received (or, for a joint transaction, received more than paid).
		a.Type, a.Base, a.Address = model.Deposit, amt(ada, delta, lovelace), ownAddr
		acts = append(acts, a)
	case delta.IsNegative():
		lost := delta.Neg()
		f := decimal.Zero
		if paid {
			f = decimal.Min(fee, lost)
		}
		sent := lost.Sub(f)
		if sent.IsZero() {
			a.Type, a.Base = model.Fee, amt(ada, f, lovelace)
			switch {
			case certified:
				a.Note = "fee of a staking certificate (registration, delegation, vote)"
			case withdrawn.IsPositive():
				a.Note = "fee of a staking reward withdrawal"
			default:
				a.Note = "transaction fee (transfer between own addresses, token move…)"
			}
		} else {
			a.Type, a.Base, a.Address = model.Withdrawal, amt(ada, sent, lovelace), foreign
			if f.IsPositive() {
				fa := amt(ada, f, lovelace)
				a.Fee = &fa
			}
		}
		acts = append(acts, a)
	}

	// Known native tokens.
	keys := map[string]bool{}
	for k := range ins {
		keys[k] = true
	}
	for k := range outs {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		if _, ok := known[k]; ok {
			sorted = append(sorted, k)
		}
	}
	sort.Strings(sorted)
	for _, k := range sorted {
		tk := known[k]
		d := outs[k].Sub(ins[k])
		if d.IsZero() {
			continue
		}
		a := base
		a.NativeID = t.TxHash + ":" + k
		asset := model.Asset{Kind: model.Crypto, Symbol: tk.Symbol}
		if d.IsPositive() {
			a.Type, a.Base, a.Address = model.Deposit, amt(asset, d, tk.Decimals), ownAddr
		} else {
			a.Type, a.Base, a.Address = model.Withdrawal, amt(asset, d.Neg(), tk.Decimals), foreign
		}
		acts = append(acts, a)
	}
	return acts, nil
}

// rewards emits one Reward per earned epoch, at the start of the epoch it
// becomes spendable (rewards not spendable yet are not in the balance
// either). It returns the last earned epoch emitted.
func (s *Source) rewards(ctx context.Context, emit func(model.Activity) error) (int, error) {
	type epoch struct {
		units     decimal.Decimal
		spendable int
		types     map[string]bool
	}
	byEpoch := map[int]*epoch{}
	q := url.Values{"order": {"earned_epoch.asc,type.asc"}}
	if s.st.RewardEpoch > 0 {
		q.Set("earned_epoch", "gte."+strconv.Itoa(s.st.RewardEpoch))
	}
	body := map[string]any{"_stake_addresses": []string{s.stake}}
	err := s.api.rows(ctx, "account_reward_history", q, body, func(raw json.RawMessage) error {
		var r rewardRow
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		v, err := dec(r.Amount)
		if err != nil {
			return err
		}
		e := byEpoch[r.EarnedEpoch]
		if e == nil {
			e = &epoch{types: map[string]bool{}}
			byEpoch[r.EarnedEpoch] = e
		}
		e.units = e.units.Add(v)
		e.spendable = max(e.spendable, r.SpendableEpoch)
		e.types[r.Type] = true
		return nil
	})
	if err != nil {
		return s.st.RewardEpoch, err
	}
	epochs := make([]int, 0, len(byEpoch))
	for n := range byEpoch {
		epochs = append(epochs, n)
	}
	sort.Ints(epochs)
	last := s.st.RewardEpoch
	now := s.now()
	for _, n := range epochs {
		e := byEpoch[n]
		at := epochStart(e.spendable)
		if e.spendable < 208 || at.After(now) {
			break // not spendable yet: comes back next run
		}
		last = n
		if !e.units.IsPositive() {
			continue
		}
		note := "staking reward, epoch " + strconv.Itoa(n)
		if !(len(e.types) == 1 && e.types["member"]) {
			var ts []string
			for t := range e.types {
				ts = append(ts, t)
			}
			sort.Strings(ts)
			note += " (" + strings.Join(ts, ", ") + ")"
		}
		a := model.Activity{NativeID: "reward:" + strconv.Itoa(n), Time: at, Type: model.Reward,
			Base: model.Amount{Asset: ada, Qty: e.units.Shift(-lovelace)}, Note: note}
		if err := emit(a); err != nil {
			return last, err
		}
	}
	return last, nil
}

// ---- balances

// Balances is the account's total: ADA on its addresses plus rewards not
// withdrawn yet (counted as Rewards already), and the known tokens.
func (s *Source) Balances(ctx context.Context) ([]model.Balance, error) {
	var info []struct {
		TotalBalance string `json:"total_balance"`
	}
	body := map[string]any{"_stake_addresses": []string{s.stake}}
	if err := s.api.call(ctx, "account_info", nil, body, &info); err != nil {
		return nil, fmt.Errorf("cardano: %w", err)
	}
	var out []model.Balance
	if len(info) > 0 {
		v, err := dec(info[0].TotalBalance)
		if err != nil {
			return nil, fmt.Errorf("cardano: %w", err)
		}
		if v.IsPositive() {
			out = append(out, model.Balance{Amount: model.Amount{Asset: ada, Qty: v.Shift(-lovelace)}})
		}
	}
	held := map[string]decimal.Decimal{}
	q := url.Values{"policy_id": {"in.(" + strings.Join(knownPolicies(), ",") + ")"}}
	err := s.api.rows(ctx, "account_assets", q, body, func(raw json.RawMessage) error {
		var a assetQty
		if err := json.Unmarshal(raw, &a); err != nil {
			return err
		}
		if _, ok := known[a.key()]; !ok {
			return nil
		}
		v, err := dec(a.Quantity)
		if err != nil {
			return err
		}
		held[a.key()] = held[a.key()].Add(v)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("cardano: %w", err)
	}
	keys := make([]string, 0, len(held))
	for k := range held {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if tk := known[k]; held[k].IsPositive() {
			out = append(out, model.Balance{Amount: model.Amount{Asset: model.Asset{Kind: model.Crypto, Symbol: tk.Symbol},
				Qty: held[k].Shift(-tk.Decimals)}})
		}
	}
	return out, nil
}

// Test checks Koios answers for the account (and accepts the API key).
func (s *Source) Test(ctx context.Context) error {
	var info []json.RawMessage
	body := map[string]any{"_stake_addresses": []string{s.stake}}
	if err := s.api.call(ctx, "account_info", nil, body, &info); err != nil {
		return fmt.Errorf("cardano: %w", err)
	}
	return nil
}
