// Package binance reads a Binance account's history and balances with a
// read-only API key: deposits and withdrawals, spot trades (delisted pairs
// included), converts, card and bank purchases, dust conversions,
// distributions (airdrops, token swaps), Auto-Invest, Simple Earn rewards,
// ETH/SOL liquid staking, Binance Pay and P2P. Binance is treated as one account: moves between its
// internal wallets (spot, funding, earn) are not activities.
package binance

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

const defaultBase = "https://api.binance.com"

// Binance opened in July 2017: a first sync walks back to here.
var genesis = time.Date(2017, 7, 1, 0, 0, 0, 0, time.UTC)

// overlap is how far before the last run incremental syncs start again;
// the engine deduplicates by native id.
const overlap = 7 * 24 * time.Hour

// Documented maximum ranges between startTime and endTime.
const (
	capitalWindow  = 89 * 24 * time.Hour  // deposit/withdraw history: < 90 days
	convertWindow  = 29 * 24 * time.Hour  // convert/tradeFlow: at most 30 days
	dividendWindow = 179 * 24 * time.Hour // asset/assetDividend: 180 days
	fiatWindow     = 89 * 24 * time.Hour  // fiat history: no documented max, stay conservative
)

// Quote assets tried when looking for trading pairs of an asset we have seen.
var commonQuotes = []string{"USDT", "USDC", "FDUSD", "BUSD", "TUSD", "EUR", "BTC", "ETH", "BNB", "TRY", "BRL", "GBP"}

// Quotes of pairs that were delisted (BUSD was wound down in 2023, many EUR
// and GBP pairs closed): their pairs are gone from exchangeInfo, but trades
// made on them still count. They are tried for every asset seen.
var historicalQuotes = []string{"BUSD", "USDC", "TUSD", "USDP", "PAX", "UST", "EUR", "GBP", "BTC", "ETH", "BNB", "USDT", "FDUSD"}

// errInvalidSymbol is Binance's answer for a pair it does not know.
const errInvalidSymbol = -1121

func init() {
	source.Register(source.Definition{
		Kind:        "binance",
		Name:        "Binance",
		Category:    "exchange",
		Description: "Spot trades, converts, Auto-Invest, card and P2P purchases, Pay, deposits, withdrawals, staking and Earn rewards.",
		Fields: []source.Field{
			{Name: "api_key", Label: "API key"},
			{Name: "api_secret", Label: "API secret", Secret: true},
			{Name: "base_url", Label: "API URL", Optional: true, Help: "leave empty; for tests"},
		},
		Setup: "In Binance, open Account → API Management and create a system-generated API key. " +
			"Leave ONLY \"Enable Reading\" checked: no spot/margin trading, no withdrawals, no transfers. " +
			"Restrict it to your server's IP address if you can. Fundarr refuses keys that can trade or withdraw.",
		New: New,
	})
}

// New builds the connector.
func New(cfg source.Config) (source.Source, error) {
	key, secret := strings.TrimSpace(cfg.Get("api_key")), strings.TrimSpace(cfg.Get("api_secret"))
	if key == "" || secret == "" {
		return nil, errors.New("binance: API key and secret are required")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.Get("base_url")), "/")
	if base == "" {
		base = defaultBase
	}
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	s := &Binance{
		c: &client{base: base, key: key, secret: []byte(secret), http: hc, pause: 150 * time.Millisecond, sleep: sleepCtx,
			slow: map[string]time.Duration{"/sapi/v1/fiat/orders": 16 * time.Second, "/sapi/v1/fiat/payments": 16 * time.Second}},
		now: time.Now,
	}
	for path, gap := range productSlow {
		s.c.slow[path] = gap
	}
	if len(cfg.State) > 0 {
		if err := json.Unmarshal(cfg.State, &s.state); err != nil {
			return nil, fmt.Errorf("binance: bad saved state: %w", err)
		}
	}
	if s.state.Trades == nil {
		s.state.Trades = map[string]int64{}
	}
	if s.state.Dead == nil {
		s.state.Dead = map[string]bool{}
	}
	return s, nil
}

// Binance is a configured connector.
type Binance struct {
	c     *client
	now   func() time.Time
	state state
	syms  []symbolInfo // exchangeInfo, fetched once per connector
}

// state survives between runs: the last trade id seen per symbol, so spot
// trades are fetched incrementally and pairs once traded are always checked.
type state struct {
	Trades map[string]int64 `json:"trades"`
	// Dead are candidate pairs Binance does not know (never listed, or
	// delisted with no history kept): not asked again.
	Dead map[string]bool `json:"dead,omitempty"`
}

func (b *Binance) State() []byte {
	raw, _ := json.Marshal(b.state)
	return raw
}

// ---- Test

func (b *Binance) Test(ctx context.Context) error {
	var r struct {
		EnableReading                bool `json:"enableReading"`
		EnableWithdrawals            bool `json:"enableWithdrawals"`
		EnableInternalTransfer       bool `json:"enableInternalTransfer"`
		EnableMargin                 bool `json:"enableMargin"`
		EnableFutures                bool `json:"enableFutures"`
		PermitsUniversalTransfer     bool `json:"permitsUniversalTransfer"`
		EnableVanillaOptions         bool `json:"enableVanillaOptions"`
		EnableFixApiTrade            bool `json:"enableFixApiTrade"`
		EnableSpotAndMarginTrading   bool `json:"enableSpotAndMarginTrading"`
		EnablePortfolioMarginTrading bool `json:"enablePortfolioMarginTrading"`
	}
	if err := b.c.get(ctx, "/sapi/v1/account/apiRestrictions", nil, &r); err != nil {
		if isAuthError(err) {
			return fmt.Errorf("binance rejected the API key: check the key, the secret and the IP restriction (%w)", err)
		}
		return err
	}
	var too []string
	if r.EnableSpotAndMarginTrading || r.EnableMargin || r.EnableFutures || r.EnableVanillaOptions || r.EnableFixApiTrade || r.EnablePortfolioMarginTrading {
		too = append(too, "trading")
	}
	if r.EnableWithdrawals {
		too = append(too, "withdrawals")
	}
	if r.EnableInternalTransfer || r.PermitsUniversalTransfer {
		too = append(too, "transfers")
	}
	if len(too) > 0 {
		return fmt.Errorf("this API key allows %s: create a read-only key (only \"Enable Reading\" checked)", strings.Join(too, ", "))
	}
	if !r.EnableReading {
		return errors.New(`this API key cannot read: check "Enable Reading"`)
	}
	return nil
}

// ---- Balances

func dec(s string) decimal.Decimal {
	d, err := decimal.NewFromString(strings.TrimSpace(s))
	if err != nil {
		return decimal.Zero
	}
	return d
}

// earnAsset maps the "LD" receipts of Simple Earn flexible products that
// can show up in spot balances (LDBTC) back to the underlying asset.
func earnAsset(sym string, known map[string]bool) string {
	if strings.HasPrefix(sym, "LD") && len(sym) > 2 && !known[sym] {
		return sym[2:]
	}
	return sym
}

func (b *Binance) Balances(ctx context.Context) ([]model.Balance, error) {
	totals := map[string]decimal.Decimal{}
	add := func(sym string, qty decimal.Decimal) {
		sym = strings.ToUpper(strings.TrimSpace(sym))
		if sym == "" || qty.IsZero() {
			return
		}
		totals[sym] = totals[sym].Add(qty)
	}

	var acc struct {
		Balances []struct {
			Asset  string `json:"asset"`
			Free   string `json:"free"`
			Locked string `json:"locked"`
		} `json:"balances"`
	}
	if err := b.c.get(ctx, "/api/v3/account", url.Values{"omitZeroBalances": {"true"}}, &acc); err != nil {
		return nil, fmt.Errorf("spot balances: %w", err)
	}
	// LDxxx is only an earn receipt when no real asset carries that ticker
	// (LDO is a real token). Receipts are counted only for assets the Simple
	// Earn positions below do not report, so nothing is counted twice.
	symbols, _ := b.symbols(ctx)
	known := map[string]bool{}
	for _, s := range symbols {
		known[s.Base], known[s.Quote] = true, true
	}
	receipts := map[string]decimal.Decimal{}
	for _, x := range acc.Balances {
		q := dec(x.Free).Add(dec(x.Locked))
		if u := earnAsset(x.Asset, known); u != x.Asset {
			receipts[u] = receipts[u].Add(q)
			continue
		}
		add(x.Asset, q)
	}
	inEarn := map[string]bool{}

	var funding []struct {
		Asset       string `json:"asset"`
		Free        string `json:"free"`
		Locked      string `json:"locked"`
		Freeze      string `json:"freeze"`
		Withdrawing string `json:"withdrawing"`
	}
	if err := b.c.post(ctx, "/sapi/v1/asset/get-funding-asset", nil, &funding); err != nil {
		return nil, fmt.Errorf("funding balances: %w", err)
	}
	for _, x := range funding {
		add(x.Asset, dec(x.Free).Add(dec(x.Locked)).Add(dec(x.Freeze)).Add(dec(x.Withdrawing)))
	}

	type earnPage struct {
		Rows []struct {
			Asset       string `json:"asset"`
			TotalAmount string `json:"totalAmount"` // flexible
			Amount      string `json:"amount"`      // locked
		} `json:"rows"`
		Total int `json:"total"`
	}
	for _, path := range []string{"/sapi/v1/simple-earn/flexible/position", "/sapi/v1/simple-earn/locked/position"} {
		for page, seen := 1, 0; ; page++ {
			var p earnPage
			if err := b.c.get(ctx, path, url.Values{"current": {strconv.Itoa(page)}, "size": {"100"}}, &p); err != nil {
				return nil, fmt.Errorf("simple earn positions: %w", err)
			}
			for _, r := range p.Rows {
				q := r.TotalAmount
				if q == "" {
					q = r.Amount
				}
				add(r.Asset, dec(q))
				inEarn[strings.ToUpper(r.Asset)] = true
			}
			seen += len(p.Rows)
			if len(p.Rows) == 0 || seen >= p.Total {
				break
			}
		}
	}

	for sym, q := range receipts {
		if !inEarn[sym] {
			add(sym, q)
		}
	}

	out := make([]model.Balance, 0, len(totals))
	for sym, q := range totals {
		if q.IsZero() {
			continue
		}
		out = append(out, model.Balance{Amount: model.Amount{Asset: model.CryptoOrFiat(sym), Qty: q}})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Asset.Symbol < out[j].Asset.Symbol })
	return out, nil
}

// ---- Activities

type run struct {
	b    *Binance
	f    source.Fetch
	from time.Time
	to   time.Time
	seen map[string]bool // assets encountered, to find trading pairs
}

func (r *run) progress(format string, args ...any) {
	if r.f.Progress != nil {
		r.f.Progress(format, args...)
	}
}

func (r *run) emit(a model.Activity) error {
	a.Time = a.Time.UTC()
	r.see(a.Base.Asset.Symbol)
	if a.Quote != nil {
		r.see(a.Quote.Asset.Symbol)
	}
	if a.Fee != nil {
		r.see(a.Fee.Asset.Symbol)
	}
	return r.f.Emit(a)
}

func (r *run) see(sym string) {
	if sym != "" {
		r.seen[sym] = true
	}
}

func amt(sym, qty string) model.Amount {
	return model.Amount{Asset: model.CryptoOrFiat(sym), Qty: dec(qty)}
}

func ptr(a model.Amount) *model.Amount { return &a }

func feeOf(sym, qty string) *model.Amount {
	f := amt(sym, qty)
	if f.Qty.IsZero() || f.Asset.Symbol == "" {
		return nil
	}
	f.Qty = f.Qty.Abs()
	return &f
}

func ms(t time.Time) string { return strconv.FormatInt(t.UnixMilli(), 10) }

// windows splits [from, to] into consecutive inclusive ranges no longer than
// size that do not share a millisecond (Binance includes both ends, so
// touching ranges would return the same record twice).
func windows(from, to time.Time, size time.Duration) [][2]time.Time {
	var out [][2]time.Time
	for s := from; !s.After(to); s = s.Add(size) {
		e := s.Add(size - time.Millisecond)
		if e.After(to) {
			e = to
		}
		out = append(out, [2]time.Time{s, e})
	}
	return out
}

// halves splits an inclusive range in two inclusive, non-touching ranges.
func halves(from, to time.Time) (time.Time, time.Time) {
	mid := from.Add(to.Sub(from) / 2).Truncate(time.Millisecond)
	return mid, mid.Add(time.Millisecond)
}

func (b *Binance) Activities(ctx context.Context, f source.Fetch) error {
	if f.Emit == nil {
		return errors.New("binance: no Emit")
	}
	r := &run{b: b, f: f, from: genesis, to: b.now().UTC(), seen: map[string]bool{}}
	b.c.onWait = func(path string, d time.Duration) {
		r.progress("Binance: rate limited on %s, waiting %s", path, d.Round(time.Second))
	}
	if !f.Since.IsZero() {
		r.from = f.Since.UTC().Add(-overlap)
		if r.from.Before(genesis) {
			r.from = genesis
		}
	}
	steps := []struct {
		name string
		fn   func(context.Context) error
	}{
		{"deposits", r.deposits},
		{"withdrawals", r.withdrawals},
		{"fiat orders", r.fiatOrders},
		{"card purchases", r.fiatPayments},
		{"converts", r.converts},
		{"distributions", r.dividends},
		{"dust conversions", r.dust},
		{"Auto-Invest", r.autoInvest},
		{"Simple Earn rewards", r.earnRewards},
		{"staking", r.staking},
		{"Binance Pay", r.pay},
		{"P2P", r.p2p},
		{"spot trades", r.trades},
	}
	// A section that fails does not stop the others: what they fetch is
	// kept, and the run reports every failure at the end.
	var errs []error
	for _, s := range steps {
		r.progress("Binance: %s", s.name)
		if err := s.fn(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			r.progress("Binance: %s failed: %v", s.name, err)
			errs = append(errs, fmt.Errorf("binance %s: %w", s.name, err))
		}
	}
	return errors.Join(errs...)
}

func (r *run) deposits(ctx context.Context) error {
	for _, w := range windows(r.from, r.to, capitalWindow) {
		for offset := 0; ; {
			var rows []struct {
				ID           string `json:"id"`
				Amount       string `json:"amount"`
				Coin         string `json:"coin"`
				Network      string `json:"network"`
				Status       int    `json:"status"`
				Address      string `json:"address"`
				TxID         string `json:"txId"`
				InsertTime   int64  `json:"insertTime"`
				CompleteTime int64  `json:"completeTime"`
			}
			q := url.Values{"startTime": {ms(w[0])}, "endTime": {ms(w[1])}, "limit": {"1000"}, "offset": {strconv.Itoa(offset)}}
			if err := r.b.c.get(ctx, "/sapi/v1/capital/deposit/hisrec", q, &rows); err != nil {
				return err
			}
			for _, d := range rows {
				// 1 = success, 6 = credited but cannot withdraw yet: both are held.
				if d.Status != 1 && d.Status != 6 {
					continue
				}
				id := d.ID
				if id == "" {
					id = d.Coin + ":" + d.TxID + ":" + strconv.FormatInt(d.InsertTime, 10)
				}
				if err := r.emit(model.Activity{
					NativeID: "deposit:" + id,
					Time:     time.UnixMilli(d.InsertTime),
					Type:     model.Deposit,
					Base:     amt(d.Coin, d.Amount),
					Network:  d.Network,
					TxID:     d.TxID,
					Address:  d.Address,
				}); err != nil {
					return err
				}
			}
			if len(rows) < 1000 {
				break
			}
			offset += len(rows)
		}
	}
	return nil
}

// Binance returns withdrawal times as "2019-10-12 11:12:02" (UTC).
func parseWhen(s string) time.Time {
	t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(s), time.UTC)
	if err != nil {
		return time.Time{}
	}
	return t
}

func (r *run) withdrawals(ctx context.Context) error {
	for _, w := range windows(r.from, r.to, capitalWindow) {
		for offset := 0; ; {
			var rows []struct {
				ID             string `json:"id"`
				Amount         string `json:"amount"`
				TransactionFee string `json:"transactionFee"`
				Coin           string `json:"coin"`
				Status         int    `json:"status"`
				Address        string `json:"address"`
				TxID           string `json:"txId"`
				ApplyTime      string `json:"applyTime"`
				CompleteTime   string `json:"completeTime"`
				Network        string `json:"network"`
				Info           string `json:"info"`
			}
			q := url.Values{"startTime": {ms(w[0])}, "endTime": {ms(w[1])}, "limit": {"1000"}, "offset": {strconv.Itoa(offset)}}
			if err := r.b.c.get(ctx, "/sapi/v1/capital/withdraw/history", q, &rows); err != nil {
				return err
			}
			for _, x := range rows {
				if x.Status != 6 { // 6 = completed
					continue
				}
				when := parseWhen(x.ApplyTime)
				if c := parseWhen(x.CompleteTime); !c.IsZero() {
					when = c
				}
				// amount is what was sent; the fee is taken on top of it.
				if err := r.emit(model.Activity{
					NativeID: "withdrawal:" + x.ID,
					Time:     when,
					Type:     model.Withdrawal,
					Base:     amt(x.Coin, x.Amount),
					Fee:      feeOf(x.Coin, x.TransactionFee),
					Network:  x.Network,
					TxID:     x.TxID,
					Address:  x.Address,
				}); err != nil {
					return err
				}
			}
			if len(rows) < 1000 {
				break
			}
			offset += len(rows)
		}
	}
	return nil
}

// fiatEnvelope is how /sapi/v1/fiat/* wrap their rows.
type fiatEnvelope[T any] struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Data    []T    `json:"data"`
	Total   int    `json:"total"`
	Success bool   `json:"success"`
}

func fiatDone(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "successful", "finished", "completed", "success":
		return true
	}
	return false
}

// fiatPages walks the time windows and pages of a fiat history endpoint.
func fiatPages[T any](ctx context.Context, r *run, path, txType string, each func(T) error) error {
	// One call per page over the whole range when Binance accepts it (it
	// documents no maximum), 89-day windows otherwise: each call is costly.
	ws := [][2]time.Time{{r.from, r.to}}
	err := fiatRange(ctx, r, path, txType, ws, each)
	var api *apiError
	if errors.As(err, &api) && api.Status == http.StatusBadRequest {
		r.progress("Binance: %s refused the whole range (%s), going by windows", path, api.Msg)
		return fiatRange(ctx, r, path, txType, windows(r.from, r.to, fiatWindow), each)
	}
	return err
}

func fiatRange[T any](ctx context.Context, r *run, path, txType string, ws [][2]time.Time, each func(T) error) error {
	for _, w := range ws {
		for page, seen := 1, 0; ; page++ {
			var env fiatEnvelope[T]
			q := url.Values{"transactionType": {txType}, "beginTime": {ms(w[0])}, "endTime": {ms(w[1])}, "page": {strconv.Itoa(page)}, "rows": {"500"}}
			if err := r.b.c.get(ctx, path, q, &env); err != nil {
				return err
			}
			for _, x := range env.Data {
				if err := each(x); err != nil {
					return err
				}
			}
			seen += len(env.Data)
			if len(env.Data) < 500 || seen >= env.Total {
				break
			}
		}
	}
	return nil
}

type fiatOrder struct {
	OrderNo      string `json:"orderNo"`
	FiatCurrency string `json:"fiatCurrency"`
	Amount       string `json:"amount"`
	TotalFee     string `json:"totalFee"`
	Method       string `json:"method"`
	Status       string `json:"status"`
	CreateTime   int64  `json:"createTime"`
	UpdateTime   int64  `json:"updateTime"`
}

// fiatOrders: bank deposits (0) and withdrawals (1) of fiat money.
func (r *run) fiatOrders(ctx context.Context) error {
	for _, kind := range []struct {
		txType string
		typ    model.Type
		prefix string
	}{{"0", model.Deposit, "fiat-deposit:"}, {"1", model.Withdrawal, "fiat-withdrawal:"}} {
		err := fiatPages(ctx, r, "/sapi/v1/fiat/orders", kind.txType, func(o fiatOrder) error {
			if !fiatDone(o.Status) {
				return nil
			}
			when := o.UpdateTime
			if when == 0 {
				when = o.CreateTime
			}
			return r.emit(model.Activity{
				NativeID: kind.prefix + o.OrderNo,
				Time:     time.UnixMilli(when),
				Type:     kind.typ,
				Base:     amt(o.FiatCurrency, o.Amount),
				Fee:      feeOf(o.FiatCurrency, o.TotalFee),
				Note:     o.Method,
			})
		})
		if err != nil {
			return err
		}
	}
	return nil
}

type fiatPayment struct {
	OrderNo        string `json:"orderNo"`
	SourceAmount   string `json:"sourceAmount"`
	FiatCurrency   string `json:"fiatCurrency"`
	ObtainAmount   string `json:"obtainAmount"`
	CryptoCurrency string `json:"cryptoCurrency"`
	TotalFee       string `json:"totalFee"`
	Status         string `json:"status"`
	PaymentMethod  string `json:"paymentMethod"`
	CreateTime     int64  `json:"createTime"`
	UpdateTime     int64  `json:"updateTime"`
}

// fiatPayments: crypto bought (0) or sold (1) for fiat by card or bank.
// sourceAmount includes the fee. When the money did not come from the
// Binance fiat balance ("Cash Balance"), it is first deposited, so the
// replayed fiat balance stays right.
func (r *run) fiatPayments(ctx context.Context) error {
	err := fiatPages(ctx, r, "/sapi/v1/fiat/payments", "0", func(p fiatPayment) error {
		if !fiatDone(p.Status) {
			return nil
		}
		when := time.UnixMilli(p.CreateTime)
		paid := dec(p.SourceAmount)
		fee := dec(p.TotalFee)
		if !strings.EqualFold(strings.TrimSpace(p.PaymentMethod), "Cash Balance") {
			if err := r.emit(model.Activity{
				NativeID: "fiat-payment:" + p.OrderNo + ":funding",
				Time:     when,
				Type:     model.Deposit,
				Base:     model.Amount{Asset: model.CryptoOrFiat(p.FiatCurrency), Qty: paid},
				Note:     p.PaymentMethod,
			}); err != nil {
				return err
			}
		}
		return r.emit(model.Activity{
			NativeID: "fiat-payment:" + p.OrderNo,
			Time:     when,
			Type:     model.Trade,
			Side:     model.Buy,
			Base:     amt(p.CryptoCurrency, p.ObtainAmount),
			Quote:    ptr(model.Amount{Asset: model.CryptoOrFiat(p.FiatCurrency), Qty: paid.Sub(fee)}),
			Fee:      feeOf(p.FiatCurrency, p.TotalFee),
			Note:     p.PaymentMethod,
		})
	})
	if err != nil {
		return err
	}
	return fiatPages(ctx, r, "/sapi/v1/fiat/payments", "1", func(p fiatPayment) error {
		if !fiatDone(p.Status) {
			return nil
		}
		// Selling: sourceAmount of crypto gives obtainAmount of fiat, fee in fiat.
		return r.emit(model.Activity{
			NativeID: "fiat-payment:" + p.OrderNo,
			Time:     time.UnixMilli(p.CreateTime),
			Type:     model.Trade,
			Side:     model.Sell,
			Base:     amt(p.CryptoCurrency, p.SourceAmount),
			Quote:    ptr(amt(p.FiatCurrency, p.ObtainAmount)),
			Fee:      feeOf(p.FiatCurrency, p.TotalFee),
			Note:     p.PaymentMethod,
		})
	})
}

// converts: Binance Convert, from one asset to another without an order book.
func (r *run) converts(ctx context.Context) error {
	var walk func(from, to time.Time) error
	walk = func(from, to time.Time) error {
		var res struct {
			List []struct {
				QuoteID     string `json:"quoteId"`
				OrderID     int64  `json:"orderId"`
				OrderStatus string `json:"orderStatus"`
				FromAsset   string `json:"fromAsset"`
				FromAmount  string `json:"fromAmount"`
				ToAsset     string `json:"toAsset"`
				ToAmount    string `json:"toAmount"`
				CreateTime  int64  `json:"createTime"`
			} `json:"list"`
			MoreData bool `json:"moreData"`
		}
		q := url.Values{"startTime": {ms(from)}, "endTime": {ms(to)}, "limit": {"1000"}}
		if err := r.b.c.get(ctx, "/sapi/v1/convert/tradeFlow", q, &res); err != nil {
			return err
		}
		// More than one page in this range: split it until each half fits.
		if (res.MoreData || len(res.List) >= 1000) && to.Sub(from) > time.Hour {
			end, next := halves(from, to)
			if err := walk(from, end); err != nil {
				return err
			}
			return walk(next, to)
		}
		for _, c := range res.List {
			if !strings.EqualFold(c.OrderStatus, "SUCCESS") {
				continue
			}
			if err := r.emit(model.Activity{
				NativeID: "convert:" + strconv.FormatInt(c.OrderID, 10),
				Time:     time.UnixMilli(c.CreateTime),
				Type:     model.Trade,
				Side:     model.Buy,
				Base:     amt(c.ToAsset, c.ToAmount),
				Quote:    ptr(amt(c.FromAsset, c.FromAmount)),
				Note:     "Convert",
			}); err != nil {
				return err
			}
		}
		return nil
	}
	for _, w := range windows(r.from, r.to, convertWindow) {
		if err := walk(w[0], w[1]); err != nil {
			return err
		}
	}
	return nil
}

// dividends: staking rewards, Simple Earn interest, airdrops, distributions.
func (r *run) dividends(ctx context.Context) error {
	var walk func(from, to time.Time) error
	walk = func(from, to time.Time) error {
		var res struct {
			Rows []struct {
				ID        int64  `json:"id"`
				Amount    string `json:"amount"`
				Asset     string `json:"asset"`
				DivTime   int64  `json:"divTime"`
				EnInfo    string `json:"enInfo"`
				TranID    int64  `json:"tranId"`
				Direction *int   `json:"direction"`
			} `json:"rows"`
			Total int `json:"total"`
		}
		q := url.Values{"startTime": {ms(from)}, "endTime": {ms(to)}, "limit": {"500"}}
		if err := r.b.c.get(ctx, "/sapi/v1/asset/assetDividend", q, &res); err != nil {
			return err
		}
		// Only the latest 500 rows of a range come back: split busy ranges.
		if (len(res.Rows) >= 500 || res.Total > len(res.Rows)) && to.Sub(from) > time.Hour {
			end, next := halves(from, to)
			if err := walk(from, end); err != nil {
				return err
			}
			return walk(next, to)
		}
		for _, d := range res.Rows {
			// Simple Earn interest comes from its own rewards history.
			if strings.Contains(strings.ToLower(d.EnInfo), "simple earn") {
				continue
			}
			a := amt(d.Asset, d.Amount)
			typ := model.Reward
			// A negative amount or direction takes the asset away.
			if a.Qty.IsNegative() || (d.Direction != nil && *d.Direction < 0) {
				typ = model.Fee
				a.Qty = a.Qty.Abs()
			}
			if a.Qty.IsZero() {
				continue
			}
			id := strconv.FormatInt(d.ID, 10)
			if d.ID == 0 {
				id = strconv.FormatInt(d.TranID, 10) + ":" + d.Asset
			}
			if err := r.emit(model.Activity{
				NativeID: "distribution:" + id,
				Time:     time.UnixMilli(d.DivTime),
				Type:     typ,
				Base:     a,
				Note:     d.EnInfo,
			}); err != nil {
				return err
			}
		}
		return nil
	}
	for _, w := range windows(r.from, r.to, dividendWindow) {
		if err := walk(w[0], w[1]); err != nil {
			return err
		}
	}
	return nil
}

// dust: small balances converted to BNB. Only the last 100 conversions
// (after 2020-12-01) are available. transferedAmount is the BNB credited,
// after the service charge.
func (r *run) dust(ctx context.Context) error {
	var res struct {
		UserAssetDribblets []struct {
			OperateTime int64 `json:"operateTime"`
			TransID     int64 `json:"transId"`
			Details     []struct {
				TransID             int64  `json:"transId"`
				ServiceChargeAmount string `json:"serviceChargeAmount"`
				Amount              string `json:"amount"`
				OperateTime         int64  `json:"operateTime"`
				TransferedAmount    string `json:"transferedAmount"`
				FromAsset           string `json:"fromAsset"`
			} `json:"userAssetDribbletDetails"`
		} `json:"userAssetDribblets"`
	}
	if err := r.b.c.get(ctx, "/sapi/v1/asset/dribblet", nil, &res); err != nil {
		return err
	}
	for _, d := range res.UserAssetDribblets {
		for _, x := range d.Details {
			when := x.OperateTime
			if when == 0 {
				when = d.OperateTime
			}
			if time.UnixMilli(when).Before(r.from) {
				continue
			}
			got := dec(x.TransferedAmount)
			charge := dec(x.ServiceChargeAmount)
			if err := r.emit(model.Activity{
				NativeID: fmt.Sprintf("dust:%d:%s", d.TransID, x.FromAsset),
				Time:     time.UnixMilli(when),
				Type:     model.Trade,
				Side:     model.Buy,
				Base:     model.Amount{Asset: model.CryptoOrFiat("BNB"), Qty: got.Add(charge)},
				Quote:    ptr(amt(x.FromAsset, x.Amount)),
				Fee:      feeOf("BNB", x.ServiceChargeAmount),
				Note:     "Dust conversion",
			}); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- spot trades

type symbolInfo struct {
	Symbol, Base, Quote string
}

func (b *Binance) symbols(ctx context.Context) ([]symbolInfo, error) {
	if b.syms != nil {
		return b.syms, nil
	}
	var info struct {
		Symbols []struct {
			Symbol     string `json:"symbol"`
			Status     string `json:"status"`
			BaseAsset  string `json:"baseAsset"`
			QuoteAsset string `json:"quoteAsset"`
		} `json:"symbols"`
	}
	if err := b.c.do(ctx, http.MethodGet, "/api/v3/exchangeInfo", url.Values{"showPermissionSets": {"false"}}, false, &info); err != nil {
		return nil, err
	}
	out := make([]symbolInfo, 0, len(info.Symbols))
	for _, s := range info.Symbols {
		out = append(out, symbolInfo{Symbol: s.Symbol, Base: s.BaseAsset, Quote: s.QuoteAsset})
	}
	b.syms = out
	return out, nil
}

// trades fetches spot trades for every pair that could have been traded:
// pairs already traded before, and pairs between assets seen in balances or
// in the rest of the history (or one such asset and a common quote).
// myTrades requires a symbol; there is no "all my trades" endpoint.
func (r *run) trades(ctx context.Context) error {
	syms, err := r.b.symbols(ctx)
	if err != nil {
		return err
	}
	bal, err := r.b.Balances(ctx)
	if err != nil {
		return err
	}
	for _, x := range bal {
		r.see(x.Asset.Symbol)
	}
	quotes := map[string]bool{}
	for _, q := range commonQuotes {
		quotes[q] = true
	}
	var todo []symbolInfo
	for _, s := range syms {
		_, before := r.b.state.Trades[s.Symbol]
		if before || (r.seen[s.Base] && (r.seen[s.Quote] || quotes[s.Quote])) || (r.seen[s.Quote] && quotes[s.Base]) {
			todo = append(todo, s)
		}
	}
	listed := map[string]bool{}
	for _, s := range syms {
		listed[s.Symbol] = true
	}
	for base := range r.seen {
		if model.CryptoOrFiat(base).Kind == model.Fiat {
			continue
		}
		for _, q := range historicalQuotes {
			sym := base + q
			if base == q || listed[sym] || r.b.state.Dead[sym] {
				continue
			}
			listed[sym] = true
			todo = append(todo, symbolInfo{Symbol: sym, Base: base, Quote: q})
		}
	}
	sort.Slice(todo, func(i, j int) bool { return todo[i].Symbol < todo[j].Symbol })
	r.progress("Binance: checking %d trading pairs", len(todo))
	for _, s := range todo {
		if err := r.symbolTrades(ctx, s); err != nil {
			return fmt.Errorf("%s: %w", s.Symbol, err)
		}
	}
	return nil
}

func (r *run) symbolTrades(ctx context.Context, s symbolInfo) error {
	last, before := r.b.state.Trades[s.Symbol]
	from := int64(0)
	if before {
		from = last + 1
	}
	for {
		var rows []struct {
			Symbol          string `json:"symbol"`
			ID              int64  `json:"id"`
			OrderID         int64  `json:"orderId"`
			Price           string `json:"price"`
			Qty             string `json:"qty"`
			QuoteQty        string `json:"quoteQty"`
			Commission      string `json:"commission"`
			CommissionAsset string `json:"commissionAsset"`
			Time            int64  `json:"time"`
			IsBuyer         bool   `json:"isBuyer"`
		}
		q := url.Values{"symbol": {s.Symbol}, "fromId": {strconv.FormatInt(from, 10)}, "limit": {"1000"}}
		if err := r.b.c.get(ctx, "/api/v3/myTrades", q, &rows); err != nil {
			var e *apiError
			if errors.As(err, &e) && e.Code == errInvalidSymbol {
				r.b.state.Dead[s.Symbol] = true
				return nil
			}
			return err
		}
		for _, t := range rows {
			side := model.Sell
			if t.IsBuyer {
				side = model.Buy
			}
			quote := dec(t.QuoteQty)
			if quote.IsZero() {
				quote = dec(t.Price).Mul(dec(t.Qty))
			}
			if err := r.emit(model.Activity{
				NativeID: "trade:" + s.Symbol + ":" + strconv.FormatInt(t.ID, 10),
				Time:     time.UnixMilli(t.Time),
				Type:     model.Trade,
				Side:     side,
				Base:     amt(s.Base, t.Qty),
				Quote:    ptr(model.Amount{Asset: model.CryptoOrFiat(s.Quote), Qty: quote}),
				Fee:      feeOf(t.CommissionAsset, t.Commission),
			}); err != nil {
				return err
			}
			if t.ID > r.b.state.Trades[s.Symbol] || !before {
				r.b.state.Trades[s.Symbol] = t.ID
				before = true
			}
		}
		if len(rows) < 1000 {
			return nil
		}
		from = rows[len(rows)-1].ID + 1
	}
}
