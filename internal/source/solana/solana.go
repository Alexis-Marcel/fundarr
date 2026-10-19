// Package solana reads a Solana wallet (Phantom, Solflare, Ledger…) from its
// public address: SOL and SPL token history and balances. Nothing is signed
// and no secret is needed.
//
// Data comes from the public JSON-RPC (api.mainnet-beta.solana.com):
// getSignaturesForAddress lists the transactions of the address and of each
// of its token accounts (an incoming token transfer often does not mention
// the owner, only its token account), getTransaction (jsonParsed) gives the
// balances before and after each one, and the owner's movements are their
// difference. Unknown token mints are named by Jupiter's token API.
//
// Limits of this first version:
//   - a swap shows as its legs (a Withdrawal of what was sent and a Deposit
//     of what was received, same TxID), not as a Trade;
//   - tokens Jupiter does not list as verified are skipped as spam, and
//     well-known symbols (SOL, USDC, USDT…) only count from their canonical
//     mint, so a counterfeit "USDC" airdrop is ignored;
//   - staking rewards (credited to stake accounts at epoch boundaries, not
//     by a transaction) and SOL held in stake accounts are not read;
//   - SOL locked as rent in token accounts is not part of the balance: its
//     payment shows as a Withdrawal, its refund on close as a Deposit.
package solana

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
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

const (
	network   = "solana"
	rpcURL    = "https://api.mainnet-beta.solana.com"
	tokensURL = "https://lite-api.jup.ag/tokens/v2/search"

	tokenProgram  = "TokenkegQfeZyiNwAJbNbGKPFXCWuBvf9Ss623VQ5DA"
	token2022     = "TokenzQdBNbLqP5VEhdkAS6EPFLC1PHnBqCXEpPxuEb"
	systemProgram = "11111111111111111111111111111111"
	wrappedSOL    = "So11111111111111111111111111111111111111112"

	native  = "SOL" // key of the native coin among mints
	lamport = 9     // SOL decimals
)

// known mints (checked against Jupiter's verified list): their symbol is
// taken as is, and these symbols from any other mint are counterfeit.
var known = map[string]string{
	"EPjFWdd5AufqSSqeM2qN1xzybapC8G4wEGGkZwyTDt1v": "USDC",
	"Es9vMFrzaCERmJfrF4H2FYD4KCoNkY11McCe8BenwNYB": "USDT",
	"2b1kV6DkPAnxd5ixfnxCpjxmKwqjjaYmCZfHsFu24GXo": "PYUSD",
	"HzwqbKZw8HxMN6bF2yFZNrht3c2iXXzpKcFu7uBEDKtr": "EURC",
	"JUPyiwrYJFskUPiHa7hkeR8VUtAeFoSYbKedZNsDvCN":  "JUP",
	"DezXAZ8z7PnrnRJjz3wXBoRgixCa6xjnB7YaB1pPB263": "BONK",
	"EKpQGSJtjMFqKZ9KQanSqYXRcF8fBopzLHYxdM65zcjm": "WIF",
	"jtojtomepa8beP8AuQc6eXt5FriJwfFMwQx2v2f9mCL":  "JTO",
	"HZ1JovNiVvGrGNiiYvEozEVgZ58xaU3RKwX8eACQBCt3": "PYTH",
	"4k3Dyjzvzp8eMZWUXbBCjEvwSkkk59S5iCNLY3QrkX6R": "RAY",
	"mSoLzYCxHdYgdzU16g5QSh3i5K3z3KZK7ytfqcJm7So":  "MSOL",
	"J1toso1uCk3RLmjorhTtrVwY9HJ7X8V9yYac6Y7kGCPn": "JITOSOL",
	"bSo13r4TkiE4KumL71LsHTPpL2euBYLFx6h9HP3piy1":  "BSOL",
	wrappedSOL: native, // wrapped SOL counts as SOL
}

// protected symbols: only their mint in known may carry them.
var protected = func() map[string]bool {
	p := map[string]bool{"WSOL": true, "ETH": true, "WETH": true, "BTC": true, "WBTC": true, "DAI": true}
	for _, s := range known {
		p[s] = true
	}
	return p
}()

// New builds the connector from its settings.
func New(cfg source.Config) (source.Source, error) {
	addr := strings.TrimSpace(cfg.Get("address"))
	if !Valid(addr) {
		return nil, errors.New("solana: the address must be a base58 public key (32 to 44 characters, as Phantom or Solflare show it)")
	}
	// The public RPC allows 40 calls of one method per 10 s per IP: a
	// pause of 300 ms keeps getTransaction under it.
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	s := &Source{
		addr: addr,
		api:  &client{http: hc, rpc: rpcURL, tokens: tokensURL, pause: 300 * time.Millisecond, sleep: sleepCtx},
		st:   state{Last: map[string]string{}, Tokens: map[string]token{}},
	}
	if b := strings.TrimRight(strings.TrimSpace(cfg.Get("base_url")), "/"); b != "" {
		// Another RPC; Jupiter answers there too unless tokens_url says
		// otherwise (tests serve both).
		s.api.rpc, s.api.tokens = b, b+"/tokens/v2/search"
	}
	if t := strings.TrimSpace(cfg.Get("tokens_url")); t != "" {
		s.api.tokens = t
	}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &s.st); err != nil {
			return nil, fmt.Errorf("solana: bad saved state: %w", err)
		}
		if s.st.Last == nil {
			s.st.Last = map[string]string{}
		}
		if s.st.Tokens == nil {
			s.st.Tokens = map[string]token{}
		}
	}
	return s, nil
}

const alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

// Valid reports whether s is a Solana address: base58 for exactly 32 bytes
// (Bitcoin and Tron base58check addresses decode to 25).
func Valid(s string) bool {
	if len(s) < 32 || len(s) > 44 {
		return false
	}
	b, ok := decode58(s)
	return ok && len(b) == 32
}

func decode58(s string) ([]byte, bool) {
	var out []byte // big-endian
	zeros := 0
	for zeros < len(s) && s[zeros] == '1' {
		zeros++
	}
	for i := 0; i < len(s); i++ {
		v := strings.IndexByte(alphabet, s[i])
		if v < 0 {
			return nil, false
		}
		carry := v
		for j := len(out) - 1; j >= 0; j-- {
			carry += 58 * int(out[j])
			out[j] = byte(carry)
			carry >>= 8
		}
		for carry > 0 {
			out = append([]byte{byte(carry)}, out...)
			carry >>= 8
		}
	}
	return append(make([]byte, zeros), out...), true
}

// Source is a configured wallet.
type Source struct {
	addr string
	api  *client
	st   state
}

type state struct {
	// Newest signature processed per address walked: the owner and each of
	// its token accounts (closed ones included).
	Last map[string]string `json:"last"`
	// Tokens resolved through Jupiter (mint → symbol, spam).
	Tokens map[string]token `json:"tokens"`
}

type token struct {
	Symbol string `json:"symbol"`
	Spam   bool   `json:"spam,omitempty"`
}

func (s *Source) State() []byte {
	b, _ := json.Marshal(s.st)
	return b
}

// ---- HTTP

type client struct {
	http   *http.Client
	rpc    string
	tokens string
	pause  time.Duration
	sleep  func(context.Context, time.Duration) error

	mu      sync.Mutex
	last    time.Time
	id      int
	version int // transaction version an RPC asked for (see txVersion)
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

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// retryable JSON-RPC errors: rate limits (429, Helius' -32429) and a node
// behind or missing the block (-32005, -32004).
func (e *rpcError) retryable() bool {
	m := strings.ToLower(e.Message)
	switch e.Code {
	case 429, -32429, -32005, -32004:
		return true
	}
	return strings.Contains(m, "rate limit") || strings.Contains(m, "too many")
}

// do sends a request, pausing between calls and backing off on rate limits,
// server errors and timeouts. what names the call in errors: the URL may
// carry an API key, so it never goes into a message.
func (c *client) do(ctx context.Context, what string, build func() (*http.Request, error), check func([]byte) (retry bool, err error)) error {
	backoff := 2 * time.Second
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		wait := c.pause - time.Since(c.last)
		c.mu.Unlock()
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
		req, err := build()
		if err != nil {
			return err
		}
		resp, err := c.http.Do(req)
		c.mu.Lock()
		c.last = time.Now()
		c.mu.Unlock()
		var body []byte
		if err == nil {
			body, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20))
			resp.Body.Close()
		}
		retry := err != nil || resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests
		var cerr error
		if !retry && resp.StatusCode == http.StatusOK {
			retry, cerr = check(body)
		}
		if retry {
			if ctx.Err() == nil && attempt < 5 {
				if err := c.sleep(ctx, backoff); err != nil {
					return err
				}
				backoff *= 2
				continue
			}
			if err != nil {
				var ue *url.Error
				if errors.As(err, &ue) {
					err = ue.Err
				}
				return fmt.Errorf("%s: %v", what, err)
			}
			if cerr != nil {
				return fmt.Errorf("%s: %w", what, cerr)
			}
			if resp.StatusCode == http.StatusTooManyRequests {
				return fmt.Errorf("%s: rate limited", what)
			}
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s: HTTP %d", what, resp.StatusCode)
		}
		if cerr != nil {
			return fmt.Errorf("%s: %w", what, cerr)
		}
		return nil
	}
}

// call runs one JSON-RPC method and decodes its result into out.
func (c *client) call(ctx context.Context, method string, params []any, out any) error {
	c.mu.Lock()
	c.id++
	id := c.id
	c.mu.Unlock()
	payload, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return err
	}
	return c.do(ctx, method, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.rpc, bytes.NewReader(payload))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, err
	}, func(body []byte) (bool, error) {
		var env struct {
			Result json.RawMessage `json:"result"`
			Error  *rpcError       `json:"error"`
		}
		if err := json.Unmarshal(body, &env); err != nil {
			return false, fmt.Errorf("decoding response: %w", err)
		}
		if env.Error != nil {
			return env.Error.retryable(), errors.New(env.Error.Message)
		}
		if err := json.Unmarshal(env.Result, out); err != nil {
			return false, fmt.Errorf("decoding result: %w", err)
		}
		return false, nil
	})
}

// ---- token accounts

type tokenAmount struct {
	Amount   string `json:"amount"`
	Decimals int32  `json:"decimals"`
}

type tokenAccount struct {
	Pubkey  string
	Mint    string
	Balance decimal.Decimal
}

// tokenAccounts lists the owner's token accounts under both token programs.
func (s *Source) tokenAccounts(ctx context.Context) ([]tokenAccount, error) {
	var out []tokenAccount
	for _, prog := range []string{tokenProgram, token2022} {
		var res struct {
			Value []struct {
				Pubkey  string `json:"pubkey"`
				Account struct {
					Data struct {
						Parsed struct {
							Info struct {
								Mint        string      `json:"mint"`
								Owner       string      `json:"owner"`
								TokenAmount tokenAmount `json:"tokenAmount"`
							} `json:"info"`
						} `json:"parsed"`
					} `json:"data"`
				} `json:"account"`
			} `json:"value"`
		}
		err := s.api.call(ctx, "getTokenAccountsByOwner",
			[]any{s.addr, map[string]string{"programId": prog}, map[string]string{"encoding": "jsonParsed"}}, &res)
		if err != nil {
			return nil, err
		}
		for _, v := range res.Value {
			info := v.Account.Data.Parsed.Info
			q, err := units(info.TokenAmount.Amount, info.TokenAmount.Decimals)
			if err != nil {
				return nil, err
			}
			out = append(out, tokenAccount{Pubkey: v.Pubkey, Mint: info.Mint, Balance: q})
		}
	}
	return out, nil
}

// ---- amounts

func units(raw string, dec int32) (decimal.Decimal, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return decimal.Zero, nil
	}
	d, err := decimal.NewFromString(raw)
	if err != nil {
		return decimal.Zero, fmt.Errorf("bad amount %q", raw)
	}
	return d.Shift(-dec), nil
}

func sol(lamports int64) decimal.Decimal { return decimal.NewFromInt(lamports).Shift(-lamport) }

// ---- tokens

func ascii(s string) bool {
	for _, r := range s {
		if r > unicode.MaxASCII || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// asset names a mint, or reports it as spam. Unknown mints must have been
// resolved first.
func (s *Source) asset(mint string) (model.Asset, bool) {
	if sym, ok := known[mint]; ok {
		return model.CryptoOrFiat(sym), true
	}
	t, ok := s.st.Tokens[mint]
	if !ok || t.Spam {
		return model.Asset{}, false
	}
	return model.CryptoOrFiat(t.Symbol), true
}

// resolve asks Jupiter for the mints not named yet; a mint it does not know
// or does not mark verified is spam.
func (s *Source) resolve(ctx context.Context, mints []string) error {
	var todo []string
	for _, m := range mints {
		if _, ok := known[m]; ok {
			continue
		}
		if _, ok := s.st.Tokens[m]; ok {
			continue
		}
		todo = append(todo, m)
	}
	sort.Strings(todo)
	for len(todo) > 0 {
		n := min(len(todo), 100) // the most a search takes
		batch := todo[:n]
		todo = todo[n:]
		var res []struct {
			ID         string   `json:"id"`
			Symbol     string   `json:"symbol"`
			IsVerified bool     `json:"isVerified"`
			Tags       []string `json:"tags"`
		}
		u := s.api.tokens + "?query=" + url.QueryEscape(strings.Join(batch, ","))
		err := s.api.do(ctx, "jupiter tokens", func() (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		}, func(body []byte) (bool, error) {
			if err := json.Unmarshal(body, &res); err != nil {
				return false, fmt.Errorf("decoding response: %w", err)
			}
			return false, nil
		})
		if err != nil {
			return err
		}
		got := map[string]token{}
		for _, r := range res {
			sym := strings.ToUpper(strings.TrimSpace(r.Symbol))
			verified := r.IsVerified
			for _, t := range r.Tags {
				verified = verified || t == "verified" || t == "strict"
			}
			spam := !verified || sym == "" || len(sym) > 24 || !ascii(sym) || protected[sym]
			got[r.ID] = token{Symbol: sym, Spam: spam}
		}
		for _, m := range batch {
			t, ok := got[m]
			if !ok {
				t = token{Spam: true}
			}
			s.st.Tokens[m] = t
		}
	}
	return nil
}

// ---- history

type sigInfo struct {
	Signature string `json:"signature"`
	Slot      int64  `json:"slot"`
}

// pageSize is the signatures asked per call (the RPC's maximum).
var pageSize = 1000

// signatures lists an address' signatures newer than until, newest first.
func (s *Source) signatures(ctx context.Context, addr, until string) ([]sigInfo, error) {
	var all []sigInfo
	before := ""
	for {
		opts := map[string]any{"limit": pageSize}
		if before != "" {
			opts["before"] = before
		}
		if until != "" {
			opts["until"] = until
		}
		var page []sigInfo
		if err := s.api.call(ctx, "getSignaturesForAddress", []any{addr, opts}, &page); err != nil {
			return nil, err
		}
		for _, p := range page {
			if p.Signature == until {
				return all, nil
			}
			all = append(all, p)
		}
		if len(page) < pageSize {
			return all, nil
		}
		before = page[len(page)-1].Signature
	}
}

type tokenBalance struct {
	AccountIndex  int         `json:"accountIndex"`
	Mint          string      `json:"mint"`
	Owner         string      `json:"owner"`
	UITokenAmount tokenAmount `json:"uiTokenAmount"`
}

type instruction struct {
	ProgramID string          `json:"programId"`
	Parsed    json.RawMessage `json:"parsed"` // an object, or a string for some programs
}

type txResult struct {
	Slot      int64  `json:"slot"`
	BlockTime *int64 `json:"blockTime"`
	Meta      *struct {
		Err               json.RawMessage `json:"err"`
		Fee               int64           `json:"fee"`
		PreBalances       []int64         `json:"preBalances"`
		PostBalances      []int64         `json:"postBalances"`
		PreTokenBalances  []tokenBalance  `json:"preTokenBalances"`
		PostTokenBalances []tokenBalance  `json:"postTokenBalances"`
		InnerInstructions []struct {
			Instructions []instruction `json:"instructions"`
		} `json:"innerInstructions"`
	} `json:"meta"`
	Transaction struct {
		Message struct {
			AccountKeys []struct {
				Pubkey string `json:"pubkey"`
			} `json:"accountKeys"`
			Instructions []instruction `json:"instructions"`
		} `json:"message"`
	} `json:"transaction"`
}

// txVersion is the newest transaction format asked for; an RPC that meets a
// newer one names it in its error, and the call is made again with it.
var txVersion = 1

var versionRe = regexp.MustCompile(`[Tt]ransaction version \((\d+)\) is not supported`)

func (s *Source) transaction(ctx context.Context, sig string) (*txResult, error) {
	for range 3 {
		s.api.mu.Lock()
		v := max(s.api.version, txVersion)
		s.api.mu.Unlock()
		var t *txResult
		err := s.api.call(ctx, "getTransaction",
			[]any{sig, map[string]any{"encoding": "jsonParsed", "maxSupportedTransactionVersion": v}}, &t)
		if err == nil {
			return t, nil
		}
		m := versionRe.FindStringSubmatch(err.Error())
		n := 0
		if m != nil {
			n, _ = strconv.Atoi(m[1])
		}
		if n <= v {
			return nil, err
		}
		s.api.mu.Lock()
		s.api.version = n
		s.api.mu.Unlock()
	}
	return nil, fmt.Errorf("getTransaction %s: unsupported transaction version", sig)
}

// moves is what one transaction did to the owner.
type moves struct {
	sig    string
	slot   int64
	time   time.Time
	failed bool
	fee    decimal.Decimal            // paid by the owner (zero when someone else paid)
	delta  map[string]decimal.Decimal // mint (native for SOL) → change, fee excluded
	peer   map[string]string          // mint → counterparty, when there is exactly one
}

// parse computes the owner's changes in a transaction, and the token
// accounts of the owner it mentions (to walk too: closed ones are not
// listed by getTokenAccountsByOwner).
func (s *Source) parse(sig string, t *txResult) (*moves, []string, error) {
	m := t.Meta
	if m == nil {
		return nil, nil, fmt.Errorf("transaction %s has no meta", sig)
	}
	mv := &moves{sig: sig, slot: t.Slot, delta: map[string]decimal.Decimal{}, peer: map[string]string{}}
	if t.BlockTime != nil {
		mv.time = time.Unix(*t.BlockTime, 0).UTC()
	}
	mv.failed = len(m.Err) > 0 && string(m.Err) != "null"
	keys := t.Transaction.Message.AccountKeys
	if len(keys) > 0 && keys[0].Pubkey == s.addr {
		mv.fee = sol(m.Fee)
	}

	// SOL: the owner's lamports, fee given back.
	for i, k := range keys {
		if k.Pubkey != s.addr || i >= len(m.PreBalances) || i >= len(m.PostBalances) {
			continue
		}
		d := sol(m.PostBalances[i] - m.PreBalances[i])
		if i == 0 {
			d = d.Add(sol(m.Fee))
		}
		mv.delta[native] = mv.delta[native].Add(d)
	}

	// Tokens: per account, before and after (a missing side is zero).
	type acct struct {
		mint, owner string
		pre, post   decimal.Decimal
	}
	accts := map[int]*acct{}
	read := func(bs []tokenBalance, post bool) error {
		for _, b := range bs {
			q, err := units(b.UITokenAmount.Amount, b.UITokenAmount.Decimals)
			if err != nil {
				return err
			}
			a := accts[b.AccountIndex]
			if a == nil {
				a = &acct{mint: b.Mint}
				accts[b.AccountIndex] = a
			}
			if b.Owner != "" {
				a.owner = b.Owner
			}
			if post {
				a.post = q
			} else {
				a.pre = q
			}
		}
		return nil
	}
	if err := read(m.PreTokenBalances, false); err != nil {
		return nil, nil, err
	}
	if err := read(m.PostTokenBalances, true); err != nil {
		return nil, nil, err
	}
	var mine []string
	owners := map[string]map[string]decimal.Decimal{} // mint → owner → change
	idx := make([]int, 0, len(accts))
	for i := range accts {
		idx = append(idx, i)
	}
	sort.Ints(idx)
	for _, i := range idx {
		a := accts[i]
		mint := a.mint
		if mint == wrappedSOL {
			mint = native
		}
		d := a.post.Sub(a.pre)
		if owners[mint] == nil {
			owners[mint] = map[string]decimal.Decimal{}
		}
		owners[mint][a.owner] = owners[mint][a.owner].Add(d)
		if a.owner != s.addr {
			continue
		}
		mv.delta[mint] = mv.delta[mint].Add(d)
		if i < len(keys) {
			mine = append(mine, keys[i].Pubkey)
		}
	}

	// Counterparties: tokens by owners moving the other way, SOL by system
	// transfers to or from the owner.
	for mint, d := range mv.delta {
		if d.IsZero() || mint == native {
			continue
		}
		var peers []string
		for o, od := range owners[mint] {
			if o != s.addr && o != "" && od.Sign() == -d.Sign() {
				peers = append(peers, o)
			}
		}
		if len(peers) == 1 {
			mv.peer[mint] = peers[0]
		}
	}
	if d := mv.delta[native]; !d.IsZero() {
		peers := map[string]bool{}
		all := append([]instruction(nil), t.Transaction.Message.Instructions...)
		for _, in := range m.InnerInstructions {
			all = append(all, in.Instructions...)
		}
		for _, in := range all {
			if in.ProgramID != systemProgram || len(in.Parsed) == 0 || in.Parsed[0] != '{' {
				continue
			}
			var p struct {
				Type string `json:"type"`
				Info struct {
					Source      string `json:"source"`
					Destination string `json:"destination"`
				} `json:"info"`
			}
			if json.Unmarshal(in.Parsed, &p) != nil || !strings.HasPrefix(p.Type, "transfer") {
				continue
			}
			switch {
			case d.IsNegative() && p.Info.Source == s.addr && p.Info.Destination != s.addr:
				peers[p.Info.Destination] = true
			case d.IsPositive() && p.Info.Destination == s.addr && p.Info.Source != s.addr:
				peers[p.Info.Source] = true
			}
		}
		if len(peers) == 1 {
			for p := range peers {
				mv.peer[native] = p
			}
		}
	}
	return mv, mine, nil
}

func (s *Source) Activities(ctx context.Context, f source.Fetch) error {
	if f.Emit == nil {
		return errors.New("solana: no Emit")
	}
	progress := f.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	accts, err := s.tokenAccounts(ctx)
	if err != nil {
		return fmt.Errorf("solana: %w", err)
	}
	// The owner, its token accounts, and those seen before (closed since).
	queue := []string{s.addr}
	for _, a := range accts {
		queue = append(queue, a.Pubkey)
	}
	var old []string
	for a := range s.st.Last {
		old = append(old, a)
	}
	sort.Strings(old)
	queue = append(queue, old...)

	walked := map[string]bool{}
	newest := map[string]string{}
	got := map[string]*moves{}
	for len(queue) > 0 {
		a := queue[0]
		queue = queue[1:]
		if walked[a] {
			continue
		}
		walked[a] = true
		if a == s.addr {
			progress("solana: transactions")
		} else {
			progress("solana: token account %s", a)
		}
		sigs, err := s.signatures(ctx, a, s.st.Last[a])
		if err != nil {
			return fmt.Errorf("solana: %w", err)
		}
		newest[a] = s.st.Last[a]
		if len(sigs) > 0 {
			newest[a] = sigs[0].Signature
		}
		for _, sg := range sigs {
			if _, done := got[sg.Signature]; done {
				continue
			}
			t, err := s.transaction(ctx, sg.Signature)
			if err != nil {
				return fmt.Errorf("solana: %w", err)
			}
			if t == nil {
				progress("solana: transaction %s not found, skipped", sg.Signature)
				got[sg.Signature] = nil
				continue
			}
			mv, mine, err := s.parse(sg.Signature, t)
			if err != nil {
				// One odd transaction must not stop every later sync.
				progress("solana: %v, skipped", err)
				got[sg.Signature] = nil
				continue
			}
			got[sg.Signature] = mv
			for _, k := range mine {
				if !walked[k] {
					queue = append(queue, k)
				}
			}
		}
	}

	var list []*moves
	mints := map[string]bool{}
	for _, mv := range got {
		if mv == nil {
			continue
		}
		list = append(list, mv)
		for m := range mv.delta {
			if m != native {
				mints[m] = true
			}
		}
	}
	var ms []string
	for m := range mints {
		ms = append(ms, m)
	}
	if err := s.resolve(ctx, ms); err != nil {
		return fmt.Errorf("solana: %w", err)
	}
	sort.Slice(list, func(i, j int) bool {
		if list[i].slot != list[j].slot {
			return list[i].slot < list[j].slot
		}
		return list[i].sig < list[j].sig
	})
	for _, mv := range list {
		if err := s.emit(mv, f.Emit); err != nil {
			return err
		}
	}
	// Only now: a run that failed half-way is done again next time.
	for a, sig := range newest {
		s.st.Last[a] = sig
	}
	return nil
}

// emit turns a transaction's changes into activities: what came in is a
// Deposit, what went out a Withdrawal, the fee rides on the SOL withdrawal
// or stands alone.
func (s *Source) emit(mv *moves, emit func(model.Activity) error) error {
	base := model.Activity{Account: network, Network: network, Time: mv.time, TxID: mv.sig}
	solAsset := model.CryptoOrFiat(native)
	fee := model.Amount{Asset: solAsset, Qty: mv.fee}
	feeDone := !mv.fee.IsPositive()
	if !mv.failed {
		keys := make([]string, 0, len(mv.delta))
		for k := range mv.delta {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool { // SOL first
			if (keys[i] == native) != (keys[j] == native) {
				return keys[i] == native
			}
			return keys[i] < keys[j]
		})
		for _, k := range keys {
			d := mv.delta[k]
			if d.IsZero() {
				continue
			}
			asset, ok := solAsset, true
			if k != native {
				asset, ok = s.asset(k)
			}
			if !ok {
				continue // spam
			}
			a := base
			a.NativeID = mv.sig + ":" + k
			a.Base = model.Amount{Asset: asset, Qty: d.Abs()}
			a.Address = mv.peer[k]
			if d.IsPositive() {
				a.Type = model.Deposit
			} else {
				a.Type = model.Withdrawal
				if k == native && !feeDone {
					a.Fee, feeDone = &fee, true
				}
			}
			if k != native && known[k] == "" {
				a.Note = "token " + k
			}
			if err := emit(a); err != nil {
				return err
			}
		}
	}
	if feeDone {
		return nil
	}
	a := base
	a.NativeID, a.Type, a.Base, a.Note = mv.sig+":fee", model.Fee, fee, "transaction fee"
	if mv.failed {
		a.Note = "fee of a failed transaction"
	}
	return emit(a)
}

// ---- balances

func (s *Source) balance(ctx context.Context) (decimal.Decimal, error) {
	var res struct {
		Value int64 `json:"value"`
	}
	if err := s.api.call(ctx, "getBalance", []any{s.addr}, &res); err != nil {
		return decimal.Zero, fmt.Errorf("solana: %w", err)
	}
	return sol(res.Value), nil
}

func (s *Source) Balances(ctx context.Context) ([]model.Balance, error) {
	q, err := s.balance(ctx)
	if err != nil {
		return nil, err
	}
	accts, err := s.tokenAccounts(ctx)
	if err != nil {
		return nil, fmt.Errorf("solana: %w", err)
	}
	var mints []string
	for _, a := range accts {
		if a.Balance.IsPositive() {
			mints = append(mints, a.Mint)
		}
	}
	if err := s.resolve(ctx, mints); err != nil {
		return nil, fmt.Errorf("solana: %w", err)
	}
	sum := map[string]model.Amount{}
	add := func(a model.Asset, q decimal.Decimal) {
		x := sum[a.Key()]
		x.Asset, x.Qty = a, x.Qty.Add(q)
		sum[a.Key()] = x
	}
	add(model.CryptoOrFiat(native), q)
	for _, a := range accts {
		if !a.Balance.IsPositive() {
			continue
		}
		if asset, ok := s.asset(a.Mint); ok {
			add(asset, a.Balance)
		}
	}
	ks := make([]string, 0, len(sum))
	for k := range sum {
		ks = append(ks, k)
	}
	sort.Strings(ks)
	var out []model.Balance
	for _, k := range ks {
		if sum[k].Qty.IsPositive() {
			out = append(out, model.Balance{Account: network, Amount: sum[k]})
		}
	}
	return out, nil
}

// Test checks the RPC answers for the address.
func (s *Source) Test(ctx context.Context) error {
	_, err := s.balance(ctx)
	return err
}
