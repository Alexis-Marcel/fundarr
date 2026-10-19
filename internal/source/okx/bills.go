package okx

import (
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

// ---- bill types (GET /api/v5/account/subtypes)

type billType struct {
	Type     string // bill type of this subtype
	TypeDesc string
	SubDesc  string
}

func (s *Source) billTypes(ctx context.Context) (map[string]billType, error) {
	var data []struct {
		Type     string `json:"type"`
		TypeDesc string `json:"typeDesc"`
		Subs     []struct {
			SubType     string `json:"subType"`
			SubTypeDesc string `json:"subTypeDesc"`
		} `json:"subTypeDetails"`
	}
	if err := s.c.do(ctx, http.MethodGet, "/api/v5/account/subtypes", nil, nil, &data); err != nil {
		return map[string]billType{}, err
	}
	out := map[string]billType{}
	for _, t := range data {
		for _, st := range t.Subs {
			out[st.SubType] = billType{Type: t.Type, TypeDesc: t.TypeDesc, SubDesc: st.SubTypeDesc}
		}
	}
	return out, nil
}

// ---- funding account ledger (GET /api/v5/asset/bills-history, since 2021-02-01)

type fundingBill struct {
	BillID string `json:"billId"`
	Ccy    string `json:"ccy"`
	BalChg string `json:"balChg"`
	Type   string `json:"type"`
	Ts     string `json:"ts"`
	Notes  string `json:"notes"`
}

func (s *Source) fundingBills(ctx context.Context, cutoff time.Time) ([]fundingBill, error) {
	var out []fundingBill
	after := ""
	for {
		q := url.Values{"limit": {"100"}, "pagingType": {"2"}}
		if after != "" {
			q.Set("after", after)
		}
		var page []fundingBill
		if err := s.c.do(ctx, http.MethodGet, "/api/v5/asset/bills-history", q, nil, &page); err != nil {
			return nil, err
		}
		done := len(page) < 100
		for _, b := range page {
			if !cutoff.IsZero() && msTime(b.Ts).Before(cutoff) {
				done = true
				continue
			}
			out = append(out, b)
		}
		if done || len(page) == 0 || page[len(page)-1].BillID == after {
			return out, nil
		}
		after = page[len(page)-1].BillID
	}
}

// Funding bill types (from the asset/bills documentation).
var (
	// Covered by deposit-history / withdrawal-history, or moves between the
	// accounts summed in Balances (funding, trading, Simple Earn flexible).
	fundingSkip = set("1", "2", "13", "221", "130", "131", "75", "76")
	// Income credited for nothing in return.
	fundingReward = set("28", "48", "49", "68", "72", "77", "83", "89", "139", "150", "151", "152", "162", "172", "173",
		"185", "189", "204", "209", "222", "223", "227", "229", "232", "233", "242", "251", "263", "265", "266", "271",
		"299", "300", "307", "308", "309", "314", "328", "333", "343", "344", "350", "354", "400", "408", "516", "523")
	// Charges.
	fundingFee = set("84", "346", "347")
	// Names for notes.
	fundingNames = map[string]string{
		"28": "airdrop", "48": "event reward", "49": "event giveaway", "68": "fee rebate", "72": "token received",
		"73": "token given away", "80": "staking subscription", "82": "staking redemption", "83": "staking yield",
		"84": "violation fee", "89": "deposit yield", "139": "ETH staking earnings", "151": "referral reward",
		"160": "dual investment subscription", "161": "dual investment collection", "162": "dual investment profit",
		"200": "buy", "229": "airdrop", "272": "convert (buy)", "273": "convert (sell)", "300": "fee discount rebate",
		"308": "Simple Earn Fixed interest", "343": "Simple Earn Fixed interest", "350": "BTC staking earnings",
		"400": "auto lend interest",
	}
)

func set(v ...string) map[string]bool {
	m := map[string]bool{}
	for _, s := range v {
		m[s] = true
	}
	return m
}

// ledgerLine is a balance change from either account, normalized.
type ledgerLine struct {
	id, ccy, note string
	chg           decimal.Decimal
	ts            time.Time
	reward, fee   bool // explicitly classified
}

// mapFundingBills maps funding bills; skip lists extra types covered by
// another history in this run.
func mapFundingBills(bills []fundingBill, skip map[string]bool, emit func(model.Activity) error) error {
	var rest []ledgerLine
	for _, b := range bills {
		if fundingSkip[b.Type] || skip[b.Type] {
			continue
		}
		name := fundingNames[b.Type]
		if name == "" {
			name = "funding bill type " + b.Type
		}
		rest = append(rest, ledgerLine{
			id: "fbill:" + b.BillID, ccy: b.Ccy, chg: dec(b.BalChg), ts: msTime(b.Ts), note: "OKX " + name,
			reward: fundingReward[b.Type], fee: fundingFee[b.Type],
		})
	}
	return emitLedger(rest, emit)
}

// emitLedger turns ledger lines that are not trades into activities. Two
// lines at the same instant, one out and one in, in different currencies,
// are an exchange (convert, easy convert, staking swap) and become a trade.
func emitLedger(lines []ledgerLine, emit func(model.Activity) error) error {
	byTs := map[int64][]ledgerLine{}
	var order []int64
	for _, l := range lines {
		if l.chg.IsZero() {
			continue
		}
		k := l.ts.UnixMilli()
		if _, ok := byTs[k]; !ok {
			order = append(order, k)
		}
		byTs[k] = append(byTs[k], l)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	for _, k := range order {
		g := byTs[k]
		if len(g) == 2 && !g[0].reward && !g[1].reward && !g[0].fee && !g[1].fee &&
			g[0].chg.Sign() != g[1].chg.Sign() && !strings.EqualFold(g[0].ccy, g[1].ccy) {
			in, out := g[0], g[1]
			if in.chg.IsNegative() {
				in, out = out, in
			}
			ids := []string{in.id, out.id}
			sort.Strings(ids)
			if err := emit(model.Activity{
				NativeID: "pair:" + ids[0] + "+" + ids[1], Time: in.ts, Type: model.Trade, Side: model.Buy,
				Base:  model.Amount{Asset: model.CryptoOrFiat(in.ccy), Qty: in.chg.Abs()},
				Quote: &model.Amount{Asset: model.CryptoOrFiat(out.ccy), Qty: out.chg.Abs()},
				Note:  in.note,
			}); err != nil {
				return err
			}
			continue
		}
		for _, l := range g {
			a := model.Activity{NativeID: l.id, Time: l.ts, Note: l.note,
				Base: model.Amount{Asset: model.CryptoOrFiat(l.ccy), Qty: l.chg.Abs()}}
			switch {
			case l.chg.IsPositive() && l.reward:
				a.Type = model.Reward
			case l.chg.IsNegative() && l.fee:
				a.Type = model.Fee
			case l.chg.IsPositive():
				a.Type = model.Deposit
			default:
				a.Type = model.Withdrawal
			}
			if err := emit(a); err != nil {
				return err
			}
		}
	}
	return nil
}

// ---- trading account ledger

type tradingBill struct {
	BillID   string `json:"billId"`
	Type     string `json:"type"`
	SubType  string `json:"subType"`
	Ccy      string `json:"ccy"`
	BalChg   string `json:"balChg"`
	Fee      string `json:"fee"`
	InstID   string `json:"instId"`
	InstType string `json:"instType"`
	TradeID  string `json:"tradeId"`
	OrdID    string `json:"ordId"`
	Ts       string `json:"ts"`
}

// GET /api/v5/account/bills-archive: last 3 months, newest first, paginated by bill id.
func (s *Source) tradingBills(ctx context.Context, cutoff time.Time) ([]tradingBill, error) {
	var out []tradingBill
	after := ""
	for {
		q := url.Values{"limit": {"100"}}
		if !cutoff.IsZero() {
			q.Set("begin", msStr(cutoff))
		}
		if after != "" {
			q.Set("after", after)
		}
		var page []tradingBill
		if err := s.c.do(ctx, http.MethodGet, "/api/v5/account/bills-archive", q, nil, &page); err != nil {
			return nil, err
		}
		out = append(out, page...)
		if len(page) < 100 || page[len(page)-1].BillID == after {
			return out, nil
		}
		after = page[len(page)-1].BillID
	}
}

// Spot trade subtypes: buy, sell, block trade buy/sell, forced repayment buy/sell.
var tradeSub = set("1", "2", "204", "205", "114", "115")

var (
	rewardWords = []string{"interest", "reward", "yield", "earn", "airdrop", "rebate", "bonus", "commission", "profit", "income", "distribut"}
	feeWords    = []string{"fee", "interest deduction", "penalty", "expense"}
)

func hasWord(s string, words []string) bool {
	s = strings.ToLower(s)
	for _, w := range words {
		if strings.Contains(s, w) {
			return true
		}
	}
	return false
}

func mapTradingBills(bills []tradingBill, types map[string]billType, emit func(model.Activity) error) error {
	type group struct {
		inst, id string
		bills    []tradingBill
	}
	trades := map[string]*group{}
	var tradeOrder []string
	var rest []ledgerLine
	for _, b := range bills {
		bt := types[b.SubType]
		typ := b.Type
		if typ == "" {
			typ = bt.Type
		}
		// Transfers between funding and trading accounts are internal.
		if typ == "1" || b.SubType == "11" || b.SubType == "12" || strings.EqualFold(bt.TypeDesc, "transfer") {
			continue
		}
		spot := b.InstType == "" || b.InstType == "SPOT" || b.InstType == "MARGIN"
		if tradeSub[b.SubType] && spot && b.InstID != "" {
			id := b.TradeID
			if id == "" {
				id = b.OrdID + "@" + b.Ts
			}
			k := b.InstID + ":" + id
			g := trades[k]
			if g == nil {
				g = &group{inst: b.InstID, id: id}
				trades[k] = g
				tradeOrder = append(tradeOrder, k)
			}
			g.bills = append(g.bills, b)
			continue
		}
		desc := bt.SubDesc
		if desc == "" {
			desc = bt.TypeDesc
		}
		if desc == "" {
			desc = "trading bill subtype " + b.SubType
		}
		chg := dec(b.BalChg)
		rest = append(rest, ledgerLine{
			id: "tbill:" + b.BillID, ccy: b.Ccy, chg: chg, ts: msTime(b.Ts), note: "OKX " + desc,
			reward: chg.IsPositive() && hasWord(desc, rewardWords),
			fee:    chg.IsNegative() && hasWord(desc, feeWords),
		})
	}
	for _, k := range tradeOrder {
		g := trades[k]
		lines, err := emitTrade(g.inst, g.id, g.bills, emit)
		if err != nil {
			return err
		}
		rest = append(rest, lines...)
	}
	return emitLedger(rest, emit)
}

// emitTrade turns the bills of one fill (one per currency) into a trade. A
// fill missing one side is returned as plain ledger lines.
func emitTrade(inst, id string, bills []tradingBill, emit func(model.Activity) error) ([]ledgerLine, error) {
	parts := strings.Split(strings.ToUpper(inst), "-")
	if len(parts) < 2 {
		return asLines(bills), nil
	}
	base, quote := parts[0], parts[1]
	gross := map[string]decimal.Decimal{}
	var at time.Time
	type feeLine struct {
		ccy, billID string
		amt         decimal.Decimal
	}
	var fees, rebates []feeLine
	for _, b := range bills {
		ccy := strings.ToUpper(b.Ccy)
		fee := dec(b.Fee)
		// balChg already includes the fee: negative fee = charged, positive = rebate.
		gross[ccy] = gross[ccy].Add(dec(b.BalChg).Sub(fee))
		if fee.IsNegative() {
			fees = append(fees, feeLine{ccy, b.BillID, fee.Neg()})
		} else if fee.IsPositive() {
			rebates = append(rebates, feeLine{ccy, b.BillID, fee})
		}
		if t := msTime(b.Ts); t.After(at) {
			at = t
		}
	}
	gb, okB := gross[base]
	gq, okQ := gross[quote]
	if !okB || !okQ || gb.IsZero() || gq.IsZero() || gb.Sign() == gq.Sign() {
		return asLines(bills), nil
	}
	a := model.Activity{
		NativeID: "trade:" + inst + ":" + id, Time: at, Type: model.Trade, Side: model.Buy,
		Base:  model.Amount{Asset: model.CryptoOrFiat(base), Qty: gb.Abs()},
		Quote: &model.Amount{Asset: model.CryptoOrFiat(quote), Qty: gq.Abs()},
		Note:  "OKX spot " + inst,
	}
	if gb.IsNegative() {
		a.Side = model.Sell
	}
	if len(fees) > 0 {
		a.Fee = &model.Amount{Asset: model.CryptoOrFiat(fees[0].ccy), Qty: fees[0].amt}
	}
	if err := emit(a); err != nil {
		return nil, err
	}
	for _, f := range fees[min(1, len(fees)):] {
		if err := emit(model.Activity{NativeID: "tradefee:" + f.billID, Time: at, Type: model.Fee,
			Base: model.Amount{Asset: model.CryptoOrFiat(f.ccy), Qty: f.amt}, Note: "OKX trading fee " + inst}); err != nil {
			return nil, err
		}
	}
	for _, r := range rebates {
		if err := emit(model.Activity{NativeID: "rebate:" + r.billID, Time: at, Type: model.Reward,
			Base: model.Amount{Asset: model.CryptoOrFiat(r.ccy), Qty: r.amt}, Note: "OKX trading fee rebate " + inst}); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

func asLines(bills []tradingBill) []ledgerLine {
	out := make([]ledgerLine, 0, len(bills))
	for _, b := range bills {
		out = append(out, ledgerLine{id: "tbill:" + b.BillID, ccy: b.Ccy, chg: dec(b.BalChg), ts: msTime(b.Ts),
			note: "OKX trade " + b.InstID})
	}
	return out
}

// ---- trading account quarterly archives (since 2021)

// quarters lists "2021Q1" up to the quarter before now (the current one is
// covered by bills-archive).
func quarters(now time.Time) []string {
	now = now.UTC()
	curY, curQ := now.Year(), (int(now.Month())-1)/3+1
	var out []string
	for y := 2021; y <= curY; y++ {
		for q := 1; q <= 4; q++ {
			if y == curY && q >= curQ {
				return out
			}
			out = append(out, fmt.Sprintf("%dQ%d", y, q))
		}
	}
	return out
}

// applyGap respects "1 request per 10 seconds" on the apply endpoint.
const applyGap = 10500 * time.Millisecond

func (s *Source) archives(ctx context.Context, f source.Fetch, types map[string]billType, emit func(model.Activity) error) error {
	applied := 0
	for _, q := range quarters(s.c.now()) {
		st := s.st.Archive[q]
		if st == "done" {
			continue
		}
		year, quarter := q[:4], q[4:]
		if st != "applied" {
			if applied > 0 {
				if err := s.c.sleep(ctx, applyGap); err != nil {
					return err
				}
			}
			applied++
			var res []struct {
				Result string `json:"result"`
			}
			if err := s.c.do(ctx, http.MethodPost, "/api/v5/account/bills-history-archive", nil,
				map[string]string{"year": year, "quarter": quarter}, &res); err != nil {
				return fmt.Errorf("okx archive %s: %w", q, err)
			}
			s.st.Archive[q] = "applied"
			if len(res) == 0 || res[0].Result != "true" {
				progress(f, "trading archive %s requested (ready in about 2 hours, imported on a later run)", q)
				continue
			}
		}
		var link []struct {
			FileHref string `json:"fileHref"`
			State    string `json:"state"`
		}
		if err := s.c.do(ctx, http.MethodGet, "/api/v5/account/bills-history-archive",
			url.Values{"year": {year}, "quarter": {quarter}}, nil, &link); err != nil {
			return fmt.Errorf("okx archive %s: %w", q, err)
		}
		switch {
		case len(link) == 0 || link[0].State == "failed":
			delete(s.st.Archive, q) // apply again next run
			progress(f, "trading archive %s not available, will request it again", q)
		case link[0].State == "finished" && link[0].FileHref != "":
			bills, err := s.download(ctx, link[0].FileHref)
			if err != nil {
				return fmt.Errorf("okx archive %s: %w", q, err)
			}
			if err := mapTradingBills(bills, types, emit); err != nil {
				return err
			}
			s.st.Archive[q] = "done"
			progress(f, "trading archive %s: %d ledger lines", q, len(bills))
		default:
			progress(f, "trading archive %s still being prepared by OKX", q)
		}
	}
	return nil
}

// download fetches an archive link (pre-signed, no OKX headers) and parses
// the CSV inside, whether zipped, gzipped or plain.
func (s *Source) download(ctx context.Context, href string) ([]tradingBill, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, href, nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("download: HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 512<<20))
	if err != nil {
		return nil, err
	}
	return parseArchive(raw)
}

func parseArchive(raw []byte) ([]tradingBill, error) {
	switch {
	case bytes.HasPrefix(raw, []byte("PK")):
		zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
		if err != nil {
			return nil, err
		}
		var out []tradingBill
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			b, err := parseCSV(rc)
			rc.Close()
			if err != nil {
				return nil, fmt.Errorf("%s: %w", f.Name, err)
			}
			out = append(out, b...)
		}
		return out, nil
	case bytes.HasPrefix(raw, []byte{0x1f, 0x8b}):
		gr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return nil, err
		}
		defer gr.Close()
		return parseCSV(gr)
	default:
		return parseCSV(bytes.NewReader(raw))
	}
}

func parseCSV(r io.Reader) ([]tradingBill, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	header, err := cr.Read()
	if err == io.EOF {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	col := map[string]int{}
	for i, h := range header {
		col[strings.TrimSpace(strings.TrimPrefix(h, "\ufeff"))] = i
	}
	for _, need := range []string{"billId", "subType", "ccy", "balChg", "ts"} {
		if _, ok := col[need]; !ok {
			return nil, fmt.Errorf("archive file has no %q column", need)
		}
	}
	get := func(rec []string, name string) string {
		if i, ok := col[name]; ok && i < len(rec) {
			return strings.TrimSpace(rec[i])
		}
		return ""
	}
	var out []tradingBill
	for line := 2; ; line++ {
		rec, err := cr.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		out = append(out, tradingBill{
			BillID: get(rec, "billId"), Type: get(rec, "type"), SubType: get(rec, "subType"), Ccy: get(rec, "ccy"),
			BalChg: get(rec, "balChg"), Fee: get(rec, "fee"), InstID: get(rec, "instId"), InstType: get(rec, "instType"),
			TradeID: get(rec, "tradeId"), OrdID: get(rec, "ordId"), Ts: get(rec, "ts"),
		})
	}
}
