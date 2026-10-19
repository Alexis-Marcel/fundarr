// Package tron reads a Tron wallet (Ledger, TronLink…) from its public
// address: TRX and TRC-20 history and balances. Nothing is signed and no
// secret is needed.
//
// Data comes from TronGrid's v1 API (/v1/accounts/{address},
// …/transactions, …/transactions/trc20), which answers without a key at a
// low rate; a free TronGrid key raises the limit.
//
// Limits of this first version:
//   - a swap on a DEX shows as its two legs (a Withdrawal of what was sent
//     and a Deposit of what was received), not as a Trade;
//   - TRC-10 tokens (TransferAssetContract) are ignored: nearly all are
//     airdropped junk;
//   - spam TRC-20 tokens are skipped by heuristics (see spam), and
//     well-known symbols (USDT, USDC, USDD…) only count from their canonical
//     contract, so a counterfeit "USDT" airdrop is ignored;
//   - TRX frozen for energy or bandwidth (Stake 1.0 and 2.0), delegated to
//     another account or waiting to be unfrozen still counts in the balance:
//     it is still the owner's;
//   - voting rewards are read when claimed (WithdrawBalanceContract), with
//     one extra call per claim for the amount; unclaimed rewards are not
//     counted.
package tron

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

// Network is the chain name set on activities.
const Network = "tron"

const trxDecimals = 6 // 1 TRX = 1,000,000 sun

var trx = model.CryptoOrFiat("TRX")

// New builds the connector from its settings.
func New(cfg source.Config) (source.Source, error) {
	addr := strings.TrimSpace(cfg.Get("address"))
	if !Valid(addr) {
		return nil, errors.New("tron: the address must be a Tron address (34 characters starting with T)")
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	key := strings.TrimSpace(cfg.Get("api_key"))
	pause := 350 * time.Millisecond // keyless TronGrid allows a few calls a second
	if key != "" {
		pause = 100 * time.Millisecond
	}
	base := "https://api.trongrid.io"
	if b := strings.TrimRight(strings.TrimSpace(cfg.Get("base_url")), "/"); b != "" {
		base = b // tests
	}
	s := &Source{
		addr: addr,
		api:  &client{http: hc, key: key, base: base, pause: pause, sleep: sleepCtx},
		st:   state{Since: map[string]int64{}, Tokens: map[string]token{}},
	}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &s.st); err != nil {
			return nil, fmt.Errorf("tron: bad saved state: %w", err)
		}
		if s.st.Since == nil {
			s.st.Since = map[string]int64{}
		}
		if s.st.Tokens == nil {
			s.st.Tokens = map[string]token{}
		}
	}
	return s, nil
}

// ---- addresses

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// Valid reports whether s is a Tron address: base58check of 21 bytes whose
// first is 0x41 (so it starts with T), with a good checksum.
func Valid(s string) bool {
	_, err := decode(s)
	return err == nil
}

// decode returns the 21 bytes of a base58check address.
func decode(s string) ([]byte, error) {
	if len(s) != 34 || s[0] != 'T' {
		return nil, errors.New("not a Tron address")
	}
	n := new(big.Int)
	for i := 0; i < len(s); i++ {
		d := strings.IndexByte(alphabet, s[i])
		if d < 0 {
			return nil, errors.New("not base58")
		}
		n.Mul(n, big.NewInt(58))
		n.Add(n, big.NewInt(int64(d)))
	}
	b := n.Bytes()
	if len(b) != 25 {
		return nil, errors.New("wrong length")
	}
	payload, sum := b[:21], b[21:]
	if payload[0] != 0x41 || !bytes.Equal(check(payload), sum) {
		return nil, errors.New("bad checksum")
	}
	return payload, nil
}

// check is the 4-byte base58check checksum (double sha256).
func check(b []byte) []byte {
	h := sha256.Sum256(b)
	h = sha256.Sum256(h[:])
	return h[:4]
}

// encode returns the base58check form of 21 address bytes.
func encode(payload []byte) string {
	full := append(append([]byte{}, payload...), check(payload)...)
	n := new(big.Int).SetBytes(full)
	var out []byte
	mod := new(big.Int)
	for n.Sign() > 0 {
		n.DivMod(n, big.NewInt(58), mod)
		out = append(out, alphabet[mod.Int64()])
	}
	for _, c := range full {
		if c != 0 {
			break
		}
		out = append(out, alphabet[0])
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return string(out)
}

// FromHex converts the hex form raw transactions use (41 + 20 bytes, or the
// 20 bytes alone as in EVM-style logs) to base58check; anything else comes
// back unchanged, so a base58 address passes through.
func FromHex(h string) string {
	h = strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(h), "0x"), "0X")
	b, err := hex.DecodeString(h)
	if err != nil {
		return h
	}
	switch {
	case len(b) == 20:
		b = append([]byte{0x41}, b...)
	case len(b) != 21 || b[0] != 0x41:
		return h
	}
	return encode(b)
}

// ToHex is the inverse of FromHex: "41" + 40 hex digits ("" when invalid).
func ToHex(addr string) string {
	b, err := decode(addr)
	if err != nil {
		return ""
	}
	return hex.EncodeToString(b)
}

// ---- source

// Source is a configured wallet.
type Source struct {
	addr string // base58
	api  *client
	st   state
}

type state struct {
	// Since: block_timestamp (ms) of the last row processed per list
	// (transactions, trc20); the next run asks from it again.
	Since map[string]int64 `json:"since"`
	// Tokens seen in history (contract → symbol, decimals), for balances.
	Tokens map[string]token `json:"tokens"`
}

type token struct {
	Symbol   string `json:"symbol"`
	Decimals int32  `json:"decimals"`
	Spam     bool   `json:"spam,omitempty"`
}

func (s *Source) State() []byte {
	b, _ := json.Marshal(s.st)
	return b
}

// ---- HTTP

type client struct {
	http  *http.Client
	key   string
	base  string
	pause time.Duration
	sleep func(context.Context, time.Duration) error

	mu   sync.Mutex
	last time.Time
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// envelope is TronGrid's v1 answer.
type envelope struct {
	Data    json.RawMessage `json:"data"`
	Success *bool           `json:"success"`
	Error   string          `json:"error"`
	Meta    struct {
		Fingerprint string `json:"fingerprint"`
	} `json:"meta"`
}

func isRateLimit(status int, body []byte) bool {
	if status == http.StatusTooManyRequests {
		return true
	}
	m := strings.ToLower(string(body))
	return status == http.StatusForbidden && (strings.Contains(m, "frequency") || strings.Contains(m, "rate limit"))
}

// do sends one request (body nil: GET) and returns the raw answer, pausing
// between calls and backing off on rate limits, server errors and timeouts.
func (c *client) do(ctx context.Context, path string, q url.Values, body []byte) ([]byte, error) {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	backoff := 2 * time.Second
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		wait := c.pause - time.Since(c.last)
		c.mu.Unlock()
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
		method := http.MethodGet
		var rd io.Reader
		if body != nil {
			method, rd = http.MethodPost, bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rd)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if c.key != "" {
			req.Header.Set("TRON-PRO-API-KEY", c.key)
		}
		resp, err := c.http.Do(req)
		c.mu.Lock()
		c.last = time.Now()
		c.mu.Unlock()
		var raw []byte
		if err == nil {
			raw, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
		}
		retry := err != nil || resp.StatusCode >= 500 || isRateLimit(resp.StatusCode, raw)
		if retry && ctx.Err() == nil && attempt < 5 {
			if err := c.sleep(ctx, backoff); err != nil {
				return nil, err
			}
			backoff *= 2
			continue
		}
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return nil, fmt.Errorf("%s: %v", path, err)
		}
		if isRateLimit(resp.StatusCode, raw) {
			return nil, fmt.Errorf("%s: rate limited", path)
		}
		if resp.StatusCode != http.StatusOK {
			var env envelope
			if json.Unmarshal(raw, &env) == nil && env.Error != "" {
				return nil, fmt.Errorf("%s: HTTP %d: %s", path, resp.StatusCode, env.Error)
			}
			return nil, fmt.Errorf("%s: HTTP %d", path, resp.StatusCode)
		}
		return raw, nil
	}
}

// get calls a v1 endpoint and returns its envelope.
func (c *client) get(ctx context.Context, path string, q url.Values) (envelope, error) {
	var env envelope
	raw, err := c.do(ctx, path, q, nil)
	if err != nil {
		return env, err
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return env, fmt.Errorf("%s: decoding response: %w", path, err)
	}
	if env.Success != nil && !*env.Success {
		msg := env.Error
		if msg == "" {
			msg = "request refused"
		}
		return env, fmt.Errorf("%s: %s", path, msg)
	}
	return env, nil
}

// pageSize is the rows asked per call (TronGrid's maximum).
var pageSize = 200

// walk lists an account's rows (kind: "transactions" or "trc20") from a
// block timestamp, ascending, following meta.fingerprint, calling each on
// every row. It returns the last block timestamp seen; the next run starts
// at it again (rows of that millisecond come back and are deduplicated).
func (c *client) walk(ctx context.Context, addr, kind string, from int64, each func(json.RawMessage) error) (int64, error) {
	path := "/v1/accounts/" + addr + "/transactions"
	if kind == "trc20" {
		path += "/trc20"
	}
	q := url.Values{"only_confirmed": {"true"}, "limit": {strconv.Itoa(pageSize)},
		"order_by": {"block_timestamp,asc"}, "min_timestamp": {strconv.FormatInt(from, 10)}}
	last := from
	for {
		env, err := c.get(ctx, path, q)
		if err != nil {
			return last, err
		}
		var rows []json.RawMessage
		if len(env.Data) > 0 && string(env.Data) != "null" {
			if err := json.Unmarshal(env.Data, &rows); err != nil {
				return last, fmt.Errorf("%s: decoding data: %w", path, err)
			}
		}
		for _, r := range rows {
			if err := each(r); err != nil {
				return last, err
			}
			var t struct {
				BlockTimestamp int64 `json:"block_timestamp"`
			}
			if json.Unmarshal(r, &t) == nil && t.BlockTimestamp > last {
				last = t.BlockTimestamp
			}
		}
		if env.Meta.Fingerprint == "" || len(rows) == 0 {
			return last, nil
		}
		q.Set("fingerprint", env.Meta.Fingerprint)
	}
}

// ---- amounts

func units(raw json.Number, dec int32) (decimal.Decimal, error) {
	s := strings.TrimSpace(raw.String())
	if s == "" {
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(s)
	if err != nil {
		return decimal.Zero, fmt.Errorf("bad amount %q", s)
	}
	return d.Shift(-dec), nil
}

func msTime(ms int64) time.Time { return time.UnixMilli(ms).UTC() }

// ---- spam

// canonical contracts of well-known TRC-20 tokens: a token with one of
// these symbols from another contract is counterfeit. The symbol here wins
// over the token's own (explorers rename some, USDC became "USDCOLD"), and
// decimals let a balance be read before the token shows in the history.
var canonical = map[string]token{
	"TR7NHqjeKQxGTCi8q8ZY4pL8otSzgjLj6t": {"USDT", 6, false},
	"TEkxiTehnzSmSe2XqrBj4w32RUN966rdz8": {"USDC", 6, false}, // no longer issued by Circle on Tron
	"TN3W4H6rK2ce4vX9YnFQHwKENnHjoxb3m9": {"BTC", 8, false},
	"TXpw8XeWYeTUd4quDskoUqeQPowRh4jY65": {"WBTC", 8, false},
	"THb4CqiFdwNHsWsQCs4JhzwjMWys4aqCbF": {"ETH", 18, false},
	"TXWkP3jLBqRGojUih1ShzNyDaN5Csnebok": {"WETH", 18, false},
	"TXDk8mbtRbXeYuMNS83CfKPaYYT8XWv9Hz": {"USDD", 18, false},
	"TUpMhErZL2fhh4sVNULAbNKLokS4GjC1F4": {"TUSD", 18, false},
	"TNUC9Qb1rRpS5CbWLmNMxXBjyFoydXjWFR": {"WTRX", 6, false},
	"TAFjULxiVgT4qWk6UZwjqwZXTSaGaqnVp4": {"BTT", 18, false},
	"TCFLL5dx5ZJdKnWuesXxi1VPwjLVmWZZy9": {"JST", 18, false},
	"TSSMHYeV2uE9qYH95DqyoCuNCzEL1NvU3S": {"SUN", 18, false},
}

// protected symbols only count from their canonical contract.
var protected = map[string]bool{"USDT": true, "USDC": true, "USDD": true, "TUSD": true, "TRX": true,
	"WTRX": true, "BTT": true, "JST": true, "SUN": true, "BTC": true, "WBTC": true, "ETH": true, "WETH": true, "USD1": true, "USDE": true}

var spamWords = []string{"http", "www", ".com", ".io", ".net", ".org", ".xyz", ".app", ".top", "visit", "claim",
	"voucher", "airdrop", "reward", "gift"}

// spam guesses whether a token is unsolicited junk.
func spam(contract, symbol, name string) bool {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return true
	}
	if _, ok := canonical[contract]; ok {
		return false
	}
	if protected[sym] {
		return true
	}
	// Tickers are letters and digits; "@ZAPZEE" or "USDT GIFT" are airdrops.
	if strings.ContainsAny(sym, " @#!?:/=+*%&|<>()[]{}\"'") {
		return true
	}
	text := strings.ToLower(symbol + " " + name)
	for _, w := range spamWords {
		if strings.Contains(text, w) {
			return true
		}
	}
	return len(sym) > 24
}

func ascii(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// ---- history

type txRow struct {
	TxID string `json:"txID"`
	Ret  []struct {
		ContractRet string      `json:"contractRet"`
		Fee         json.Number `json:"fee"`
	} `json:"ret"`
	NetFee         json.Number `json:"net_fee"`
	EnergyFee      json.Number `json:"energy_fee"`
	BlockTimestamp int64       `json:"block_timestamp"`
	RawData        struct {
		Contract []struct {
			Type      string `json:"type"`
			Parameter struct {
				Value struct {
					Owner     string      `json:"owner_address"`
					To        string      `json:"to_address"`
					Amount    json.Number `json:"amount"`
					Contract  string      `json:"contract_address"`
					CallValue json.Number `json:"call_value"`
				} `json:"value"`
			} `json:"parameter"`
		} `json:"contract"`
	} `json:"raw_data"`
	Internal []struct {
		ID   string `json:"internal_tx_id"`
		From string `json:"from_address"`
		To   string `json:"to_address"`
		Data struct {
			Rejected  bool                   `json:"rejected"`
			CallValue map[string]json.Number `json:"call_value"` // "_" is TRX
		} `json:"data"`
	} `json:"internal_transactions"`
}

type trc20Row struct {
	TxID      string `json:"transaction_id"`
	TokenInfo struct {
		Symbol   string `json:"symbol"`
		Address  string `json:"address"`
		Decimals int32  `json:"decimals"`
		Name     string `json:"name"`
	} `json:"token_info"`
	BlockTimestamp int64       `json:"block_timestamp"`
	From           string      `json:"from"`
	To             string      `json:"to"`
	Type           string      `json:"type"`
	Value          json.Number `json:"value"`
}

func (s *Source) Activities(ctx context.Context, f source.Fetch) error {
	if f.Emit == nil {
		return errors.New("tron: no Emit")
	}
	progress := f.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	seen := map[string]bool{}
	out := func(a model.Activity) error {
		if seen[a.NativeID] {
			return nil
		}
		seen[a.NativeID] = true
		a.Account, a.Network = Network, Network
		return f.Emit(a)
	}

	// Transactions the account took part in: TRX moved and fees paid.
	progress("tron: transactions")
	last, err := s.api.walk(ctx, s.addr, "transactions", s.st.Since["transactions"], func(raw json.RawMessage) error {
		var t txRow
		if err := json.Unmarshal(raw, &t); err != nil {
			return fmt.Errorf("tron: decoding transaction: %w", err)
		}
		return s.tx(ctx, t, out)
	})
	if err != nil {
		return fmt.Errorf("tron: %w", err)
	}
	s.st.Since["transactions"] = last

	// TRC-20 transfers. Rows carry no log index: those of one millisecond
	// are numbered in a fixed order once all of them are in.
	progress("tron: token transfers")
	var group []trc20Row
	flush := func() error {
		sort.SliceStable(group, func(i, j int) bool {
			a, b := group[i], group[j]
			if a.TxID != b.TxID {
				return a.TxID < b.TxID
			}
			if a.TokenInfo.Address != b.TokenInfo.Address {
				return a.TokenInfo.Address < b.TokenInfo.Address
			}
			if a.From != b.From {
				return a.From < b.From
			}
			if a.To != b.To {
				return a.To < b.To
			}
			return a.Value.String() < b.Value.String()
		})
		n := map[string]int{}
		for _, r := range group {
			k := r.TxID + ":" + r.TokenInfo.Address
			if err := s.token(r, n[k], out); err != nil {
				return err
			}
			n[k]++
		}
		group = group[:0]
		return nil
	}
	last, err = s.api.walk(ctx, s.addr, "trc20", s.st.Since["trc20"], func(raw json.RawMessage) error {
		var r trc20Row
		if err := json.Unmarshal(raw, &r); err != nil {
			return fmt.Errorf("tron: decoding token transfer: %w", err)
		}
		if len(group) > 0 && group[0].BlockTimestamp != r.BlockTimestamp {
			if err := flush(); err != nil {
				return err
			}
		}
		group = append(group, r)
		return nil
	})
	if err == nil {
		err = flush()
	}
	if err != nil {
		return fmt.Errorf("tron: %w", err)
	}
	s.st.Since["trc20"] = last
	return nil
}

// tx maps one transaction: TRX sent or received, a claimed reward, and the
// fee the account burnt (attached to its own TRX send, alone otherwise).
func (s *Source) tx(ctx context.Context, t txRow, out func(model.Activity) error) error {
	if len(t.RawData.Contract) == 0 {
		return nil
	}
	c := t.RawData.Contract[0]
	v := c.Parameter.Value
	owner, to := FromHex(v.Owner), FromHex(v.To)
	status := ""
	if len(t.Ret) > 0 {
		status = t.Ret[0].ContractRet
	}
	ok := status == "" || status == "SUCCESS"
	when := msTime(t.BlockTimestamp)
	base := model.Activity{Time: when, TxID: t.TxID}

	// ret.fee is everything burnt (bandwidth, energy, account creation…);
	// net_fee + energy_fee is a floor in case it is missing.
	var fee decimal.Decimal
	if owner == s.addr {
		var burnt decimal.Decimal
		if len(t.Ret) > 0 {
			burnt, _ = units(t.Ret[0].Fee, trxDecimals)
		}
		n, _ := units(t.NetFee, trxDecimals)
		e, _ := units(t.EnergyFee, trxDecimals)
		fee = decimal.Max(burnt, n.Add(e))
	}
	feeAmt := model.Amount{Asset: trx, Qty: fee}
	feeDone := false

	switch c.Type {
	case "TransferContract":
		qty, err := units(v.Amount, trxDecimals)
		if err != nil {
			return fmt.Errorf("%s: %w", t.TxID, err)
		}
		switch {
		case !ok || !qty.IsPositive() || owner == to:
		case owner == s.addr:
			a := base
			a.NativeID, a.Type, a.Base, a.Address = t.TxID+":trx", model.Withdrawal, model.Amount{Asset: trx, Qty: qty}, to
			if fee.IsPositive() {
				a.Fee, feeDone = &feeAmt, true
			}
			if err := out(a); err != nil {
				return err
			}
		case to == s.addr:
			// Received: only the amount counts, the sender paid the fee.
			a := base
			a.NativeID, a.Type, a.Base, a.Address = t.TxID+":trx", model.Deposit, model.Amount{Asset: trx, Qty: qty}, owner
			if err := out(a); err != nil {
				return err
			}
		}
	case "TriggerSmartContract":
		// TRX sent along with a contract call (a DEX swap from TRX…).
		qty, err := units(v.CallValue, trxDecimals)
		if err != nil {
			return fmt.Errorf("%s: %w", t.TxID, err)
		}
		if ok && owner == s.addr && qty.IsPositive() {
			a := base
			a.NativeID, a.Type, a.Base = t.TxID+":trx", model.Withdrawal, model.Amount{Asset: trx, Qty: qty}
			a.Address, a.Note = FromHex(v.Contract), "sent to a contract"
			if fee.IsPositive() {
				a.Fee, feeDone = &feeAmt, true
			}
			if err := out(a); err != nil {
				return err
			}
		}
	case "WithdrawBalanceContract":
		// Voting rewards claimed: the amount is only in the receipt.
		if ok && owner == s.addr {
			qty, err := s.claimed(ctx, t.TxID)
			if err != nil {
				return err
			}
			if qty.IsPositive() {
				a := base
				a.NativeID, a.Type, a.Base, a.Note = t.TxID+":reward", model.Reward, model.Amount{Asset: trx, Qty: qty}, "voting rewards"
				if err := out(a); err != nil {
					return err
				}
			}
		}
	}

	// TRX moved by contracts inside the transaction (a DEX paying out TRX…).
	if ok {
		for _, in := range t.Internal {
			if in.Data.Rejected {
				continue
			}
			qty, err := units(in.Data.CallValue["_"], trxDecimals)
			if err != nil || !qty.IsPositive() {
				continue
			}
			from, dst := FromHex(in.From), FromHex(in.To)
			a := base
			a.NativeID, a.Base, a.Note = t.TxID+":internal:"+in.ID, model.Amount{Asset: trx, Qty: qty}, "internal transfer"
			switch {
			case from == dst:
				continue
			case dst == s.addr:
				a.Type, a.Address = model.Deposit, from
			case from == s.addr:
				a.Type, a.Address = model.Withdrawal, dst
			default:
				continue
			}
			if err := out(a); err != nil {
				return err
			}
		}
	}

	// Contract call (a USDT send…), staking, vote, failed tx: only the fee.
	if feeDone || !fee.IsPositive() {
		return nil
	}
	a := base
	a.NativeID, a.Type, a.Base, a.Note = t.TxID+":fee", model.Fee, feeAmt, feeNote(c.Type)
	if !ok {
		a.Note = "fee of a failed transaction (" + strings.ToLower(status) + ")"
	}
	return out(a)
}

func feeNote(typ string) string {
	switch typ {
	case "TriggerSmartContract":
		return "fee of a contract call"
	case "TransferContract":
		return "fee"
	}
	return "fee of " + strings.TrimSuffix(typ, "Contract")
}

// claimed reads the TRX a WithdrawBalanceContract paid out.
func (s *Source) claimed(ctx context.Context, txID string) (decimal.Decimal, error) {
	body, _ := json.Marshal(map[string]string{"value": txID})
	raw, err := s.api.do(ctx, "/wallet/gettransactioninfobyid", nil, body)
	if err != nil {
		return decimal.Zero, err
	}
	var info struct {
		WithdrawAmount json.Number `json:"withdraw_amount"`
	}
	if err := json.Unmarshal(raw, &info); err != nil {
		return decimal.Zero, fmt.Errorf("transaction info %s: %w", txID, err)
	}
	return units(info.WithdrawAmount, trxDecimals)
}

// token maps one TRC-20 transfer; n numbers the transfers of the same
// contract within the transaction.
func (s *Source) token(r trc20Row, n int, out func(model.Activity) error) error {
	contract := FromHex(r.TokenInfo.Address)
	from, to := FromHex(r.From), FromHex(r.To)
	isSpam := spam(contract, r.TokenInfo.Symbol, r.TokenInfo.Name)
	sym := strings.ToUpper(strings.TrimSpace(r.TokenInfo.Symbol))
	if c, ok := canonical[contract]; ok {
		sym = c.Symbol
	}
	s.st.Tokens[contract] = token{Symbol: sym, Decimals: r.TokenInfo.Decimals, Spam: isSpam}
	if isSpam || from == to || (r.Type != "" && r.Type != "Transfer") {
		return nil
	}
	qty, err := units(r.Value, r.TokenInfo.Decimals)
	if err != nil {
		return fmt.Errorf("%s: %w", r.TxID, err)
	}
	if !qty.IsPositive() {
		return nil // zero-value transfers: address poisoning
	}
	a := model.Activity{NativeID: r.TxID + ":trc20:" + contract + ":" + strconv.Itoa(n), Time: msTime(r.BlockTimestamp),
		TxID: r.TxID, Base: model.Amount{Asset: model.CryptoOrFiat(sym), Qty: qty}}
	if _, ok := canonical[contract]; !ok {
		a.Note = "token " + contract
		if !ascii(r.TokenInfo.Symbol) {
			a.Note += " (" + r.TokenInfo.Symbol + ")"
		}
	}
	switch s.addr {
	case to:
		a.Type, a.Address = model.Deposit, from
	case from:
		a.Type, a.Address = model.Withdrawal, to
	default:
		return nil
	}
	return out(a)
}

// ---- balances

type account struct {
	Balance json.Number `json:"balance"`
	// Stake 2.0: frozen, delegated to others, waiting to be withdrawn.
	FrozenV2 []struct {
		Amount json.Number `json:"amount"`
	} `json:"frozenV2"`
	UnfrozenV2 []struct {
		Amount json.Number `json:"unfreeze_amount"`
	} `json:"unfrozenV2"`
	DelegatedBandwidthV2 json.Number `json:"delegated_frozenV2_balance_for_bandwidth"`
	// Stake 1.0.
	Frozen []struct {
		Amount json.Number `json:"frozen_balance"`
	} `json:"frozen"`
	DelegatedBandwidth json.Number `json:"delegated_frozen_balance_for_bandwidth"`
	Resource           struct {
		DelegatedEnergyV2 json.Number `json:"delegated_frozenV2_balance_for_energy"`
		DelegatedEnergy   json.Number `json:"delegated_frozen_balance_for_energy"`
		FrozenEnergy      struct {
			Amount json.Number `json:"frozen_balance"`
		} `json:"frozen_balance_for_energy"`
	} `json:"account_resource"`
	TRC20 []map[string]json.Number `json:"trc20"`
}

// sun is everything the account owns in TRX, frozen or not.
func (a account) sun() decimal.Decimal {
	all := []json.Number{a.Balance, a.DelegatedBandwidthV2, a.DelegatedBandwidth,
		a.Resource.DelegatedEnergyV2, a.Resource.DelegatedEnergy, a.Resource.FrozenEnergy.Amount}
	for _, f := range a.FrozenV2 {
		all = append(all, f.Amount)
	}
	for _, f := range a.UnfrozenV2 {
		all = append(all, f.Amount)
	}
	for _, f := range a.Frozen {
		all = append(all, f.Amount)
	}
	total := decimal.Zero
	for _, n := range all {
		if d, err := units(n, 0); err == nil {
			total = total.Add(d)
		}
	}
	return total
}

// account returns the account, nil when it was never activated.
func (s *Source) account(ctx context.Context) (*account, error) {
	env, err := s.api.get(ctx, "/v1/accounts/"+s.addr, nil)
	if err != nil {
		return nil, fmt.Errorf("tron: %w", err)
	}
	var accs []account
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, &accs); err != nil {
			return nil, fmt.Errorf("tron: decoding account: %w", err)
		}
	}
	if len(accs) == 0 {
		return nil, nil
	}
	return &accs[0], nil
}

func (s *Source) Balances(ctx context.Context) ([]model.Balance, error) {
	acc, err := s.account(ctx)
	if err != nil || acc == nil {
		return nil, err
	}
	var out []model.Balance
	if q := acc.sun().Shift(-trxDecimals); q.IsPositive() {
		out = append(out, model.Balance{Account: Network, Amount: model.Amount{Asset: trx, Qty: q}})
	}
	held := map[string]json.Number{}
	for _, m := range acc.TRC20 {
		for c, v := range m {
			held[c] = v
		}
	}
	contracts := make([]string, 0, len(held))
	for c := range held {
		contracts = append(contracts, c)
	}
	sort.Strings(contracts)
	for _, c := range contracts {
		tk, ok := s.st.Tokens[c]
		if !ok {
			// Not in the history (yet): only canonical tokens are known.
			if tk, ok = canonical[c]; !ok {
				continue
			}
		}
		if tk.Spam {
			continue
		}
		if q, err := units(held[c], tk.Decimals); err == nil && q.IsPositive() {
			out = append(out, model.Balance{Account: Network, Amount: model.Amount{Asset: model.CryptoOrFiat(tk.Symbol), Qty: q}})
		}
	}
	return out, nil
}

// Test checks TronGrid answers for the address (and accepts the key).
func (s *Source) Test(ctx context.Context) error {
	_, err := s.account(ctx)
	return err
}
