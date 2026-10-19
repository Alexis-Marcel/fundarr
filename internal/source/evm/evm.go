// Package evm reads an Ethereum-compatible wallet (Ledger, MetaMask…) from
// its public address: native coin and ERC-20 history and balances, on one or
// more networks. Nothing is signed and no secret is needed.
//
// Data comes from each network's Blockscout instance through its
// Etherscan-compatible API (module=account: txlist, txlistinternal, tokentx,
// balance, tokenbalance), or from Etherscan's v2 API (one host, chainid
// parameter) when an Etherscan key is given.
//
// Limits of this first version:
//   - a swap on a DEX shows as its two legs (a Withdrawal of what was sent
//     and a Deposit of what was received), not as a Trade;
//   - spam tokens are skipped by heuristics (see spam), and well-known
//     symbols (USDC, USDT, DAI, WETH, WBTC) only count from their canonical
//     contract, so a counterfeit "USDT" airdrop is ignored;
//   - the L1 data fee of rollups is added when the API returns it (l1Fee,
//     L1FeesPaid); Blockscout's txlist does not, so ETH on L2s can show a tiny
//     reconciliation gap.
package evm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"golang.org/x/crypto/sha3"
)

// Chain is a supported network.
type Chain struct {
	Name    string // as typed in the settings
	ID      int    // EIP-155 chain id (Etherscan v2, Blockscout PRO)
	Native  string // native coin symbol
	Decimal int32
	// Free: an Etherscan-compatible explorer that answers without a key
	// ("" when none does any more).
	Free string
	// Pro: indexed by Blockscout's PRO API, free with a key.
	Pro bool
}

// Public explorers keep closing their free APIs behind bot checks; what
// still answers without a key is listed in Free, the rest needs a free
// Blockscout key (or an Etherscan one).
var chains = map[string]Chain{
	"ethereum":  {"ethereum", 1, "ETH", 18, "https://eth.blockscout.com/api", true},
	"arbitrum":  {"arbitrum", 42161, "ETH", 18, "", true},
	"base":      {"base", 8453, "ETH", 18, "", true},
	"optimism":  {"optimism", 10, "ETH", 18, "https://explorer.optimism.io/api", true},
	"polygon":   {"polygon", 137, "POL", 18, "", true},
	"gnosis":    {"gnosis", 100, "XDAI", 18, "", true},
	"avalanche": {"avalanche", 43114, "AVAX", 18, "https://api.routescan.io/v2/network/mainnet/evm/43114/etherscan/api", false},
	"bsc":       {"bsc", 56, "BNB", 18, "", false},
}

// All is the "chains" value that reads every supported network.
const All = "all"

// Networks lists the supported networks by name.
func Networks() []string { return chainNames() }

func chainNames() []string {
	var n []string
	for k := range chains {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

func init() {
	source.Register(source.Definition{
		Kind:        "evm",
		Name:        "Ethereum & EVM wallet",
		Category:    "wallet",
		Hidden:      true, // the Wallet source covers it
		Description: "ETH and tokens on Ethereum and compatible networks from a public address (Ledger, MetaMask…): read-only.",
		Fields: []source.Field{
			{Name: "address", Label: "Address", Help: "0x… public address"},
			{Name: "chains", Label: "Networks", Optional: true,
				Help: "comma-separated, or all; default ethereum. Available: " + strings.Join(chainNames(), ", ")},
			{Name: "blockscout_key", Label: "Blockscout API key", Secret: true, Optional: true,
				Help: "free at dev.blockscout.com: reads Arbitrum, Base, Polygon and Gnosis, which no longer answer without a key"},
			{Name: "etherscan_key", Label: "Etherscan API key", Secret: true, Optional: true,
				Help: "optional: uses Etherscan v2 where the key's plan allows (free key at etherscan.io); required for bsc"},
		},
		Setup: "Fundarr only reads the public history of the address: nothing to sign, no permission to give. " +
			"In Ledger Live, open the Ethereum account and copy its receive address (0x…). " +
			"Never type your 24 recovery words anywhere but on the Ledger itself.",
		New: New,
	})
}

// New builds the connector from its settings.
func New(cfg source.Config) (source.Source, error) {
	addr := strings.TrimSpace(cfg.Get("address"))
	if err := validAddress(addr); err != nil {
		return nil, err
	}
	var cs []Chain
	seen := map[string]bool{}
	raw := cfg.Get("chains")
	if strings.TrimSpace(raw) == "" {
		raw = "ethereum"
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 60 * time.Second}
	}
	api := &client{
		http:      hc,
		key:       strings.TrimSpace(cfg.Get("etherscan_key")),
		pro:       strings.TrimSpace(cfg.Get("blockscout_key")),
		etherscan: "https://api.etherscan.io/v2/api",
		proURL:    "https://api.blockscout.com/v2/api",
		free:      func(c Chain) string { return c.Free },
		pause:     250 * time.Millisecond,
		sleep:     sleepCtx,
	}
	if b := strings.TrimRight(strings.TrimSpace(cfg.Get("base_url")), "/"); b != "" {
		// Tests: every network, Etherscan and Blockscout PRO answer at this URL.
		api.free = func(c Chain) string { return b + "/" + c.Name + "/api" }
		api.etherscan, api.proURL = b+"/v2/api", b+"/pro/api"
	}
	if strings.EqualFold(strings.TrimSpace(raw), All) {
		// Every network a key or a free explorer lets Fundarr read.
		var ns []string
		for _, n := range chainNames() {
			if api.readable(chains[n]) {
				ns = append(ns, n)
			}
		}
		raw = strings.Join(ns, ",")
	}
	for _, n := range strings.Split(raw, ",") {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" || seen[n] {
			continue
		}
		c, ok := chains[n]
		if !ok {
			return nil, fmt.Errorf("evm: unknown network %q (available: %s)", n, strings.Join(chainNames(), ", "))
		}
		if !api.readable(c) {
			if c.Pro {
				return nil, fmt.Errorf("evm: %s needs an API key: a free Blockscout key (dev.blockscout.com) or an Etherscan one", n)
			}
			return nil, fmt.Errorf("evm: %s has no free explorer: add an Etherscan API key whose plan covers it", n)
		}
		seen[n] = true
		cs = append(cs, c)
	}
	if len(cs) == 0 {
		return nil, errors.New("evm: choose at least one network")
	}
	s := &Source{addr: strings.ToLower(addr), chains: cs, api: api, st: state{Chains: map[string]*chainState{}}}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &s.st); err != nil {
			return nil, fmt.Errorf("evm: bad saved state: %w", err)
		}
		if s.st.Chains == nil {
			s.st.Chains = map[string]*chainState{}
		}
	}
	return s, nil
}

// validAddress checks 0x + 40 hex digits, and the EIP-55 checksum when the
// address mixes upper and lower case.
func validAddress(a string) error {
	if len(a) != 42 || !strings.HasPrefix(a, "0x") {
		return errors.New("evm: the address must look like 0x followed by 40 hexadecimal characters")
	}
	hex := a[2:]
	for _, r := range hex {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return errors.New("evm: the address must look like 0x followed by 40 hexadecimal characters")
		}
	}
	if hex == strings.ToLower(hex) || hex == strings.ToUpper(hex) {
		return nil
	}
	if checksum(hex) != hex {
		return errors.New("evm: the address checksum is wrong (a character was mistyped?)")
	}
	return nil
}

// checksum returns the EIP-55 mixed-case form of a 40-hex address.
func checksum(hex string) string {
	lower := strings.ToLower(hex)
	h := sha3.NewLegacyKeccak256()
	h.Write([]byte(lower))
	sum := h.Sum(nil)
	out := []byte(lower)
	for i := range out {
		if out[i] >= 'a' && out[i] <= 'f' {
			nibble := sum[i/2]
			if i%2 == 0 {
				nibble >>= 4
			}
			if nibble&0xf >= 8 {
				out[i] -= 'a' - 'A'
			}
		}
	}
	return string(out)
}

// Source is a configured wallet.
type Source struct {
	addr   string // lower case
	chains []Chain
	api    *client
	st     state
}

type state struct {
	Chains map[string]*chainState `json:"chains"`
}

type chainState struct {
	// Last block fully processed per action (txlist, internal, tokentx).
	Blocks map[string]int64 `json:"blocks"`
	// Tokens seen in history (contract → symbol, decimals), for balances.
	Tokens map[string]token `json:"tokens"`
}

type token struct {
	Symbol   string `json:"symbol"`
	Decimals int32  `json:"decimals"`
	Spam     bool   `json:"spam,omitempty"`
}

func (s *Source) chainState(name string) *chainState {
	cs := s.st.Chains[name]
	if cs == nil {
		cs = &chainState{}
		s.st.Chains[name] = cs
	}
	if cs.Blocks == nil {
		cs.Blocks = map[string]int64{}
	}
	if cs.Tokens == nil {
		cs.Tokens = map[string]token{}
	}
	return cs
}

func (s *Source) State() []byte {
	b, _ := json.Marshal(s.st)
	return b
}

// ---- HTTP

type client struct {
	http      *http.Client
	key       string // Etherscan
	pro       string // Blockscout PRO
	etherscan string
	proURL    string
	free      func(Chain) string
	pause     time.Duration
	sleep     func(context.Context, time.Duration) error

	mu   sync.Mutex
	last time.Time
	// unplanned: networks the Etherscan key's plan does not cover, read
	// another way instead.
	unplanned map[string]bool
}

// route says where a network is read: Etherscan when its key's plan covers
// it, then Blockscout PRO with its key, then a free explorer.
func (c *client) route(ch Chain, q url.Values) (base string, ok bool) {
	q.Del("chainid")
	q.Del("chain_id")
	q.Del("apikey")
	c.mu.Lock()
	etherscan := c.key != "" && !c.unplanned[ch.Name]
	c.mu.Unlock()
	switch {
	case etherscan:
		q.Set("chainid", strconv.Itoa(ch.ID))
		q.Set("apikey", c.key)
		return c.etherscan, true
	case c.pro != "" && ch.Pro:
		q.Set("chain_id", strconv.Itoa(ch.ID))
		q.Set("apikey", c.pro)
		return c.proURL, true
	case c.free(ch) != "":
		return c.free(ch), true
	}
	return "", false
}

// readable: some way to read the network is configured.
func (c *client) readable(ch Chain) bool {
	_, ok := c.route(ch, url.Values{})
	return ok
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

type envelope struct {
	Status  string          `json:"status"`
	Message string          `json:"message"`
	Result  json.RawMessage `json:"result"`
}

// errEmpty is "no transactions found" and the like.
var errEmpty = errors.New("empty")

func isEmptyMessage(m string) bool {
	m = strings.ToLower(m)
	return strings.HasPrefix(m, "no ") && strings.Contains(m, "found")
}

func isRateLimit(m string) bool {
	m = strings.ToLower(m)
	return strings.Contains(m, "rate limit") || strings.Contains(m, "too many")
}

// get calls module=account&action=… for a chain and decodes result into out.
func (c *client) get(ctx context.Context, ch Chain, q url.Values, out any) error {
	base, ok := c.route(ch, q)
	if !ok {
		return fmt.Errorf("%s: no way to read it (a free Blockscout key reads it)", ch.Name)
	}
	viaKey := base == c.etherscan
	q.Set("module", "account")
	u := base + "?" + q.Encode()
	backoff := 2 * time.Second
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		wait := c.pause - time.Since(c.last)
		c.mu.Unlock()
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
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
		if err != nil || resp.StatusCode >= 500 {
			// Timeouts, dropped connections and server errors: retry. The
			// URL may carry the API key, so it never goes into the message.
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
				return fmt.Errorf("%s %s: %v", ch.Name, q.Get("action"), err)
			}
		}
		var env envelope
		jsonErr := json.Unmarshal(body, &env)
		limited := resp.StatusCode == http.StatusTooManyRequests
		if jsonErr == nil && env.Status == "0" {
			var msg string
			_ = json.Unmarshal(env.Result, &msg)
			if isRateLimit(env.Message) || isRateLimit(msg) {
				limited = true
			}
		}
		if limited {
			if attempt >= 5 {
				return fmt.Errorf("%s %s: rate limited", ch.Name, q.Get("action"))
			}
			if err := c.sleep(ctx, backoff); err != nil {
				return err
			}
			backoff *= 2
			continue
		}
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("%s %s: HTTP %d", ch.Name, q.Get("action"), resp.StatusCode)
		}
		if jsonErr != nil {
			return fmt.Errorf("%s %s: decoding response: %w", ch.Name, q.Get("action"), jsonErr)
		}
		if env.Status == "0" {
			if isEmptyMessage(env.Message) {
				return errEmpty
			}
			var msg string
			_ = json.Unmarshal(env.Result, &msg)
			if msg == "" {
				msg = env.Message
			}
			if viaKey && strings.Contains(strings.ToLower(msg), "upgrade") {
				// "Free API access is not supported for this chain": read
				// the network another way, if there is one.
				c.mu.Lock()
				if c.unplanned == nil {
					c.unplanned = map[string]bool{}
				}
				c.unplanned[ch.Name] = true
				c.mu.Unlock()
				if c.readable(ch) {
					return c.get(ctx, ch, q, out)
				}
				return fmt.Errorf("%s: your Etherscan plan does not cover it", ch.Name)
			}
			return fmt.Errorf("%s %s: %s", ch.Name, q.Get("action"), msg)
		}
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("%s %s: decoding result: %w", ch.Name, q.Get("action"), err)
		}
		return nil
	}
}

// pageSize is the rows asked per call; history is walked by startblock.
var pageSize = 1000

// walk lists an action's rows from a block, ascending, calling each on every
// row. The next call starts at the last block seen (rows of that block come
// again and are deduplicated by the caller); a block holding more rows than
// a page goes by page numbers.
func (c *client) walk(ctx context.Context, ch Chain, action, addr string, from int64, each func(json.RawMessage) error) (int64, error) {
	last := from
	start, page := from, 1
	for {
		q := url.Values{"action": {action}, "address": {addr}, "sort": {"asc"},
			"startblock": {strconv.FormatInt(start, 10)}, "page": {strconv.Itoa(page)}, "offset": {strconv.Itoa(pageSize)}}
		var rows []json.RawMessage
		err := c.get(ctx, ch, q, &rows)
		if errors.Is(err, errEmpty) {
			return last, nil
		}
		if err != nil {
			return last, err
		}
		for _, r := range rows {
			if err := each(r); err != nil {
				return last, err
			}
			var b struct {
				BlockNumber string `json:"blockNumber"`
			}
			if json.Unmarshal(r, &b) == nil {
				if n, err := strconv.ParseInt(b.BlockNumber, 10, 64); err == nil && n > last {
					last = n
				}
			}
		}
		if len(rows) < pageSize {
			return last, nil
		}
		if last == start {
			page++
		} else {
			start, page = last, 1
		}
	}
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

func parseTime(ts string) time.Time {
	n, _ := strconv.ParseInt(strings.TrimSpace(ts), 10, 64)
	return time.Unix(n, 0).UTC()
}

// ---- spam

// canonical contracts of well-known symbols per network: a token with one of
// these symbols elsewhere is counterfeit (airdropped to look like a balance).
var canonical = map[string]map[string]string{
	"ethereum": {"USDC": "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", "USDT": "0xdac17f958d2ee523a2206206994597c13d831ec7",
		"DAI": "0x6b175474e89094c44da98b954eedeac495271d0f", "WETH": "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2",
		"WBTC": "0x2260fac5e5542a773aa44fbcfedf7c193bc2c599"},
	"arbitrum": {"USDC": "0xaf88d065e77c8cc2239327c5edb3a432268e5831", "USDT": "0xfd086bc7cd5c481dcc9c85ebe478a1c0b69fcbb9",
		"DAI": "0xda10009cbd5d07dd0cecc66161fc93d7c9000da1", "WETH": "0x82af49447d8a07e3bd95bd0d56f35241523fbab1",
		"WBTC": "0x2f2a2543b76a4166549f7aab2e75bef0aefc5b0f"},
	"base": {"USDC": "0x833589fcd6edb6e08f4c7c32d4f71b54bda02913", "DAI": "0x50c5725949a6f0c72e6c4a641f24049a917db0cb",
		"WETH": "0x4200000000000000000000000000000000000006"},
	"optimism": {"USDC": "0x0b2c639c533813f4aa9d7837caf62653d097ff85", "USDT": "0x94b008aa00579c1307b0ef2c499ad98a8ce58e58",
		"DAI": "0xda10009cbd5d07dd0cecc66161fc93d7c9000da1", "WETH": "0x4200000000000000000000000000000000000006",
		"WBTC": "0x68f180fcce6836688e9084f035309e29bf0a2095"},
	"polygon": {"USDC": "0x3c499c542cef5e3811e1192ce70d8cc03d5c3359", "USDT": "0xc2132d05d31c914a87c6611c10748aeb04b58e8f",
		"DAI": "0x8f3cf7ad23cd3cadbd9735aff958023239c6a063", "WETH": "0x7ceb23fd6bc0add59e62ac25578270cff1b9f619",
		"WBTC": "0x1bfd67037b42cf73acf2047067bd4f2c47d9bfd6"},
	"gnosis": {"USDC": "0xddafbb505ad214d7b80b1f830fccc89b60fb7a83", "USDT": "0x4ecaba5870353805a9f068101a40e0f32ed605c6",
		"WETH": "0x6a023ccd1ff6f2045c3309768ead9e68f978f6e1"},
	"avalanche": {"USDC": "0xb97ef9ef8734c71904d8002f8b6bc66dd9c48a6e", "USDT": "0x9702230a8ea53601f5cd2dc00fdbc13d4df4a8c7"},
	"bsc": {"USDC": "0x8ac76a51cc950d9822d68b83fe1ad97b32cd580d", "USDT": "0x55d398326f99059ff775485246999027b3197955",
		"ETH": "0x2170ed0880ac9a755fd29b2688956bd959f933f8"},
}

var protected = map[string]bool{"USDC": true, "USDT": true, "DAI": true, "WETH": true, "WBTC": true, "ETH": true}

var spamWords = []string{"http", "www", ".com", ".io", ".net", ".org", ".xyz", ".app", "visit", "claim", "voucher", "airdrop", "reward"}

// spam guesses whether a token is unsolicited junk.
func spam(chain, contract, symbol, name string) bool {
	sym := strings.ToUpper(strings.TrimSpace(symbol))
	if sym == "" {
		return true
	}
	if protected[sym] {
		return canonical[chain][sym] != strings.ToLower(contract)
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
	BlockNumber string `json:"blockNumber"`
	TimeStamp   string `json:"timeStamp"`
	Hash        string `json:"hash"`
	From        string `json:"from"`
	To          string `json:"to"`
	Value       string `json:"value"`
	GasUsed     string `json:"gasUsed"`
	GasPrice    string `json:"gasPrice"`
	Effective   string `json:"effectiveGasPrice"`
	IsError     string `json:"isError"`
	L1Fee       string `json:"l1Fee"`
	L1FeesPaid  string `json:"L1FeesPaid"`
}

type internalRow struct {
	BlockNumber     string `json:"blockNumber"`
	TimeStamp       string `json:"timeStamp"`
	TransactionHash string `json:"transactionHash"`
	Hash            string `json:"hash"`
	From            string `json:"from"`
	To              string `json:"to"`
	Value           string `json:"value"`
	IsError         string `json:"isError"`
	Index           string `json:"index"`
	TraceID         string `json:"traceId"`
}

type tokenRow struct {
	BlockNumber     string `json:"blockNumber"`
	TimeStamp       string `json:"timeStamp"`
	Hash            string `json:"hash"`
	From            string `json:"from"`
	To              string `json:"to"`
	Value           string `json:"value"`
	ContractAddress string `json:"contractAddress"`
	TokenName       string `json:"tokenName"`
	TokenSymbol     string `json:"tokenSymbol"`
	TokenDecimal    string `json:"tokenDecimal"`
	LogIndex        string `json:"logIndex"`
}

func (s *Source) Activities(ctx context.Context, f source.Fetch) error {
	if f.Emit == nil {
		return errors.New("evm: no Emit")
	}
	progress := f.Progress
	if progress == nil {
		progress = func(string, ...any) {}
	}
	var errs []error
	for _, ch := range s.chains {
		if err := s.chain(ctx, ch, f.Emit, progress); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			progress("%s: %v", ch.Name, err)
			errs = append(errs, fmt.Errorf("evm %s: %w", ch.Name, err))
		}
	}
	return errors.Join(errs...)
}

func (s *Source) chain(ctx context.Context, ch Chain, emit func(model.Activity) error, progress func(string, ...any)) error {
	cs := s.chainState(ch.Name)
	native := model.CryptoOrFiat(ch.Native)
	seen := map[string]bool{}
	out := func(a model.Activity) error {
		if seen[a.NativeID] {
			return nil
		}
		seen[a.NativeID] = true
		a.Account, a.Network = ch.Name, ch.Name
		return emit(a)
	}

	// Native transactions: value moved and gas paid.
	progress("%s: transactions", ch.Name)
	last, err := s.api.walk(ctx, ch, "txlist", s.addr, cs.Blocks["txlist"], func(raw json.RawMessage) error {
		var t txRow
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		return s.tx(ch, native, t, out)
	})
	if err != nil {
		return err
	}
	cs.Blocks["txlist"] = last

	// Native coin moved by contracts (withdrawals from a bridge, a DEX…).
	progress("%s: internal transactions", ch.Name)
	n := map[string]int{}
	last, err = s.api.walk(ctx, ch, "txlistinternal", s.addr, cs.Blocks["internal"], func(raw json.RawMessage) error {
		var t internalRow
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		if t.TransactionHash == "" {
			t.TransactionHash = t.Hash
		}
		key := t.TraceID
		if key == "" {
			key = t.Index
		}
		if key == "" {
			n[t.TransactionHash]++
			key = strconv.Itoa(n[t.TransactionHash])
		}
		return s.internal(ch, native, t, key, out)
	})
	if err != nil {
		return err
	}
	cs.Blocks["internal"] = last

	// ERC-20 transfers.
	progress("%s: token transfers", ch.Name)
	n = map[string]int{}
	last, err = s.api.walk(ctx, ch, "tokentx", s.addr, cs.Blocks["tokentx"], func(raw json.RawMessage) error {
		var t tokenRow
		if err := json.Unmarshal(raw, &t); err != nil {
			return err
		}
		key := t.LogIndex
		if key == "" {
			k := t.Hash + t.ContractAddress + t.From + t.To + t.Value
			n[k]++
			key = strings.ToLower(t.ContractAddress+":"+t.From+":"+t.To+":"+t.Value) + ":" + strconv.Itoa(n[k])
		}
		return s.token(ch, cs, t, key, out)
	})
	if err != nil {
		return err
	}
	cs.Blocks["tokentx"] = last
	return nil
}

func (s *Source) tx(ch Chain, native model.Asset, t txRow, out func(model.Activity) error) error {
	from, to := strings.ToLower(t.From), strings.ToLower(t.To)
	value, err := units(t.Value, ch.Decimal)
	if err != nil {
		return err
	}
	failed := t.IsError == "1"
	when := parseTime(t.TimeStamp)
	id := ch.Name + ":" + t.Hash + ":tx"
	base := model.Activity{NativeID: id, Time: when, TxID: t.Hash}
	if from != s.addr {
		// Received: only the value counts, the sender paid the gas.
		if to != s.addr || failed || !value.IsPositive() {
			return nil
		}
		a := base
		a.Type, a.Base, a.Address = model.Deposit, model.Amount{Asset: native, Qty: value}, from
		return out(a)
	}
	price := t.Effective
	if price == "" {
		price = t.GasPrice
	}
	gu, err1 := decimal.NewFromString(orZero(t.GasUsed))
	gp, err2 := decimal.NewFromString(orZero(price))
	if err1 != nil || err2 != nil {
		return fmt.Errorf("bad gas in %s", t.Hash)
	}
	gas := gu.Mul(gp)
	for _, l1 := range []string{t.L1Fee, t.L1FeesPaid} {
		if v, err := decimal.NewFromString(strings.TrimSpace(l1)); err == nil {
			gas = gas.Add(v)
			break
		}
	}
	gas = gas.Shift(-ch.Decimal)
	fee := model.Amount{Asset: native, Qty: gas}
	if !failed && value.IsPositive() && to != s.addr {
		a := base
		a.Type, a.Base, a.Address = model.Withdrawal, model.Amount{Asset: native, Qty: value}, to
		if gas.IsPositive() {
			a.Fee = &fee
		}
		return out(a)
	}
	// Contract call, approval, failed tx or a send to oneself: only gas.
	if !gas.IsPositive() {
		return nil
	}
	a := base
	a.Type, a.Base, a.Note = model.Fee, fee, "gas"
	if failed {
		a.Note = "gas of a failed transaction"
	}
	return out(a)
}

func orZero(s string) string {
	if strings.TrimSpace(s) == "" {
		return "0"
	}
	return strings.TrimSpace(s)
}

func (s *Source) internal(ch Chain, native model.Asset, t internalRow, key string, out func(model.Activity) error) error {
	if t.IsError == "1" {
		return nil
	}
	from, to := strings.ToLower(t.From), strings.ToLower(t.To)
	if from == to {
		return nil
	}
	value, err := units(t.Value, ch.Decimal)
	if err != nil {
		return err
	}
	if !value.IsPositive() {
		return nil
	}
	a := model.Activity{NativeID: ch.Name + ":" + t.TransactionHash + ":internal:" + key, Time: parseTime(t.TimeStamp),
		TxID: t.TransactionHash, Base: model.Amount{Asset: native, Qty: value}, Note: "internal transfer"}
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

func (s *Source) token(ch Chain, cs *chainState, t tokenRow, key string, out func(model.Activity) error) error {
	from, to := strings.ToLower(t.From), strings.ToLower(t.To)
	contract := strings.ToLower(t.ContractAddress)
	dec, err := strconv.ParseInt(orZero(t.TokenDecimal), 10, 32)
	if err != nil {
		return fmt.Errorf("bad decimals for %s", contract)
	}
	isSpam := spam(ch.Name, contract, t.TokenSymbol, t.TokenName)
	sym := strings.ToUpper(strings.TrimSpace(t.TokenSymbol))
	cs.Tokens[contract] = token{Symbol: sym, Decimals: int32(dec), Spam: isSpam}
	if isSpam || from == to {
		return nil
	}
	qty, err := units(t.Value, int32(dec))
	if err != nil {
		return err
	}
	if !qty.IsPositive() {
		return nil // zero-value transfers: address poisoning
	}
	a := model.Activity{NativeID: ch.Name + ":" + t.Hash + ":token:" + key, Time: parseTime(t.TimeStamp), TxID: t.Hash,
		Base: model.Amount{Asset: model.CryptoOrFiat(sym), Qty: qty}}
	if canonical[ch.Name][sym] != contract {
		a.Note = "token " + contract
		if !ascii(t.TokenSymbol) {
			a.Note += " (" + t.TokenSymbol + ")"
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

func (s *Source) Balances(ctx context.Context) ([]model.Balance, error) {
	var out []model.Balance
	for _, ch := range s.chains {
		var wei string
		err := s.api.get(ctx, ch, url.Values{"action": {"balance"}, "address": {s.addr}, "tag": {"latest"}}, &wei)
		if err != nil {
			return nil, err
		}
		if q, err := units(wei, ch.Decimal); err == nil && q.IsPositive() {
			out = append(out, model.Balance{Account: ch.Name, Amount: model.Amount{Asset: model.CryptoOrFiat(ch.Native), Qty: q}})
		}
		cs := s.chainState(ch.Name)
		contracts := make([]string, 0, len(cs.Tokens))
		for c := range cs.Tokens {
			contracts = append(contracts, c)
		}
		sort.Strings(contracts)
		for _, c := range contracts {
			tk := cs.Tokens[c]
			if tk.Spam {
				continue
			}
			var raw string
			err := s.api.get(ctx, ch, url.Values{"action": {"tokenbalance"}, "contractaddress": {c}, "address": {s.addr}, "tag": {"latest"}}, &raw)
			if errors.Is(err, errEmpty) {
				continue
			}
			if err != nil {
				return nil, err
			}
			if q, err := units(raw, tk.Decimals); err == nil && q.IsPositive() {
				out = append(out, model.Balance{Account: ch.Name, Amount: model.Amount{Asset: model.CryptoOrFiat(tk.Symbol), Qty: q}})
			}
		}
	}
	return out, nil
}

// Test checks the address answers on the first network.
func (s *Source) Test(ctx context.Context) error {
	var wei string
	return s.api.get(ctx, s.chains[0], url.Values{"action": {"balance"}, "address": {s.addr}, "tag": {"latest"}}, &wei)
}
