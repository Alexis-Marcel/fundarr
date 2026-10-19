package binance

import (
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

// Binance's transaction history export ("Wallet → Transaction History →
// Export", one line per balance change) covers what the API does not: spot
// orders placed from the simple Buy/Sell screens never show up in myTrades,
// and token migrations (MATIC → POL, LUNA → LUNC) have no endpoint.

const statementHelp = "Binance → Wallet (or Orders) → Transaction History → Export → choose the whole period → Generate. " +
	"Download the CSV (any language) and import it here. It becomes the history for the period it covers; " +
	"the API keeps the history up to date after it. Activities the API knows better (deposits with their " +
	"transaction hash, card purchases with their amount in euros, converts) are kept from the API."

type stmtRow struct {
	line    int
	time    time.Time
	account string
	op      string // lower-case operation
	opName  string
	coin    string
	change  decimal.Decimal
}

var tzInName = regexp.MustCompile(`UTC\s*([+-])\s*(\d{1,2})(?::?(\d{2}))?`)

// Column names in the exports we know of (English and French).
var columns = map[string][]string{
	"time":    {"utc_time", "durée", "duree", "time", "date(utc)", "date"},
	"account": {"account", "compte"},
	"op":      {"operation", "opération"},
	"coin":    {"coin", "jeton", "asset"},
	"change":  {"change", "montant"},
}

func parseStatement(name string, data []byte) ([]stmtRow, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.TrimLeadingSpace = true
	header, err := r.Read()
	if err != nil {
		return nil, errors.New("not a CSV file")
	}
	idx := map[string]int{}
	for i, h := range header {
		h = strings.ToLower(strings.TrimSpace(h))
		for key, names := range columns {
			for _, n := range names {
				if h == n {
					if _, dup := idx[key]; !dup {
						idx[key] = i
					}
				}
			}
		}
	}
	for _, k := range []string{"time", "op", "coin", "change"} {
		if _, ok := idx[k]; !ok {
			return nil, errors.New("this is not Binance's transaction history export (expected columns like UTC_Time, Operation, Coin, Change)")
		}
	}
	// Times are in the zone chosen at export: the file name says it
	// ("…(UTC+2).csv"), a UTC_Time column means UTC.
	loc := time.UTC
	if m := tzInName.FindStringSubmatch(name); m != nil && !strings.EqualFold(header[idx["time"]], "utc_time") {
		h, _ := strconv.Atoi(m[2])
		mins, _ := strconv.Atoi(m[3])
		off := h*3600 + mins*60
		if m[1] == "-" {
			off = -off
		}
		loc = time.FixedZone("statement", off)
	}
	var rows []stmtRow
	for line := 2; ; line++ {
		rec, err := r.Read()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		get := func(k string) string {
			if i, ok := idx[k]; ok && i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		if get("time") == "" && get("coin") == "" {
			continue
		}
		t, err := parseStmtTime(get("time"), loc)
		if err != nil {
			return nil, fmt.Errorf("line %d: %w", line, err)
		}
		ch, err := decimal.NewFromString(strings.ReplaceAll(get("change"), ",", "."))
		if err != nil {
			return nil, fmt.Errorf("line %d: bad amount %q", line, get("change"))
		}
		if ch.IsZero() {
			continue
		}
		rows = append(rows, stmtRow{line: line, time: t.UTC(), account: get("account"), op: strings.ToLower(get("op")), opName: get("op"),
			coin: strings.ToUpper(get("coin")), change: ch})
	}
	if len(rows) == 0 {
		return nil, errors.New("the file holds no movement")
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].time.Before(rows[j].time) })
	return rows, nil
}

func parseStmtTime(s string, loc *time.Location) (time.Time, error) {
	for _, layout := range []string{"2006-01-02 15:04:05", "06-01-02 15:04:05", "2006/01/02 15:04:05", "2006-01-02T15:04:05", "02/01/2006 15:04:05", "02/01/2006 15:04"} {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("unknown date %q", s)
}

// Operation families.
func family(op string) string {
	switch {
	case op == "transaction buy" || op == "transaction spend" || op == "transaction sold" || op == "transaction revenue" ||
		op == "transaction related" || op == "transaction fee" || op == "buy" || op == "sell" || op == "fee":
		return "trade"
	case op == "binance convert" || op == "stablecoins auto-conversion" || strings.HasPrefix(op, "token swap - redenomination") ||
		op == "small assets exchange bnb" || op == "large otc trading" || strings.HasPrefix(op, "auto-invest"):
		return "convert"
	case op == "asset recovery":
		return "recovery"
	case op == "token swap - distribution":
		return "swapdist"
	case op == "withdraw" || op == "fiat withdrawal" || op == "fiat withdraw" || op == "withdrawal":
		return "withdrawal"
	case op == "deposit" || op == "fiat deposit" || op == "buy crypto with fiat" || op == "p2p trading":
		return "deposit"
	case strings.Contains(op, "commission") || strings.Contains(op, "airdrop") || strings.Contains(op, "distribution") ||
		strings.Contains(op, "interest") || strings.Contains(op, "reward") || strings.Contains(op, "cashback") ||
		strings.Contains(op, "bonus") || strings.Contains(op, "staking") || strings.Contains(op, "launchpool"):
		return "reward"
	}
	return ""
}

func stmtAmt(r stmtRow) model.Amount {
	return model.Amount{Asset: model.CryptoOrFiat(r.coin), Qty: r.change.Abs()}
}

// tradeOf builds a trade from what came in and what went out: cash-like
// assets are what things are priced in.
func tradeOf(in, out model.Amount) model.Activity {
	if model.CashLike(in.Asset) && !model.CashLike(out.Asset) {
		return model.Activity{Type: model.Trade, Side: model.Sell, Base: out, Quote: &in}
	}
	return model.Activity{Type: model.Trade, Side: model.Buy, Base: in, Quote: &out}
}

// Import implements source.Importer.
func (b *Binance) Import(name string, data []byte, existing []model.Activity) (*source.Statement, error) {
	rows, err := parseStatement(name, data)
	if err != nil {
		return nil, err
	}
	var acts []model.Activity
	unknown := map[string]int{}
	emit := func(a model.Activity, t time.Time, key string) {
		a.Time = t
		a.NativeID = fmt.Sprintf("statement:%d:%s", t.Unix(), key)
		acts = append(acts, a)
	}

	// Rows of the same second and family belong together.
	type groupKey struct {
		t   int64
		fam string
	}
	groups := map[groupKey][]stmtRow{}
	var order []groupKey
	var recoveries, dists, converts []stmtRow
	for _, r := range rows {
		fam := family(r.op)
		switch fam {
		case "convert":
			converts = append(converts, r)
			continue
		case "recovery":
			recoveries = append(recoveries, r)
			continue
		case "swapdist":
			dists = append(dists, r)
			continue
		}
		k := groupKey{r.time.Unix(), fam}
		if fam == "" || fam == "reward" || fam == "deposit" || fam == "withdrawal" {
			k.fam = fam + ":" + strconv.Itoa(r.line) // one activity per row
		}
		if _, ok := groups[k]; !ok {
			order = append(order, k)
		}
		groups[k] = append(groups[k], r)
	}

	for _, k := range order {
		g := groups[k]
		t := g[0].time
		fam, _, _ := strings.Cut(k.fam, ":")
		switch fam {
		case "trade":
			var ins, outs, fees []stmtRow
			for _, r := range g {
				switch {
				case r.op == "transaction fee" || r.op == "fee":
					fees = append(fees, r)
				case r.change.IsPositive():
					ins = append(ins, r)
				default:
					outs = append(outs, r)
				}
			}
			emitTrades(g, ins, outs, fees, t, k.fam, emit)
		case "reward":
			r := g[0]
			if r.change.IsPositive() {
				emit(model.Activity{Type: model.Reward, Base: stmtAmt(r), Note: r.opName}, t, fmt.Sprintf("reward:%d", r.line))
			} else {
				emit(model.Activity{Type: model.Fee, Base: stmtAmt(r), Note: r.opName}, t, fmt.Sprintf("charge:%d", r.line))
			}
		default:
			r := g[0]
			if fam == "" {
				unknown[r.opName]++
			}
			typ := model.Deposit
			if r.change.IsNegative() {
				typ = model.Withdrawal
			}
			emit(model.Activity{Type: typ, Base: stmtAmt(r), Note: r.opName}, t, fmt.Sprintf("%s:%d", typ, r.line))
		}
	}

	// Converts: Binance writes the two sides a second or two apart. Pair each
	// line with the nearest line of the opposite sign within a few seconds.
	paired := map[int]bool{}
	for i, r := range converts {
		if paired[i] {
			continue
		}
		best := -1
		for j := i + 1; j < len(converts) && converts[j].time.Sub(r.time) <= 5*time.Second; j++ {
			if !paired[j] && converts[j].change.IsPositive() != r.change.IsPositive() && converts[j].coin != r.coin {
				best = j
				break
			}
		}
		if best < 0 {
			continue
		}
		paired[i], paired[best] = true, true
		in, out := r, converts[best]
		if out.change.IsPositive() {
			in, out = out, in
		}
		a := tradeOf(stmtAmt(in), stmtAmt(out))
		a.Note = r.opName
		emit(a, converts[best].time, fmt.Sprintf("convert:%d", r.line))
	}
	for i, r := range converts {
		if !paired[i] {
			typ := model.Deposit
			if r.change.IsNegative() {
				typ = model.Withdrawal
			}
			emit(model.Activity{Type: typ, Base: stmtAmt(r), Note: r.opName + " (unpaired)"}, r.time, fmt.Sprintf("%s:%d", typ, r.line))
		}
	}

	// Token migrations: the old token is taken back (Asset Recovery), the
	// new one handed out later (Token Swap - Distribution), same quantity.
	used := map[int]bool{}
	for _, rec := range recoveries {
		matched := false
		for i, d := range dists {
			if used[i] || !d.change.Equal(rec.change.Abs()) || d.time.Before(rec.time) || d.time.Sub(rec.time) > 45*24*time.Hour {
				continue
			}
			used[i] = true
			matched = true
			a := tradeOf(stmtAmt(d), stmtAmt(rec))
			a.Note = "Token swap " + rec.coin + " → " + d.coin
			emit(a, d.time, fmt.Sprintf("swap:%d", rec.line))
			break
		}
		if !matched {
			emit(model.Activity{Type: model.Withdrawal, Base: stmtAmt(rec), Note: rec.opName}, rec.time, fmt.Sprintf("withdrawal:%d", rec.line))
		}
	}
	for i, d := range dists {
		if !used[i] {
			emit(model.Activity{Type: model.Reward, Base: stmtAmt(d), Note: d.opName}, d.time, fmt.Sprintf("reward:%d", d.line))
		}
	}

	sort.SliceStable(acts, func(i, j int) bool { return acts[i].Time.Before(acts[j].Time) })
	kept, replacedByAPI := preferAPI(acts, existing)

	st := &source.Statement{From: rows[0].time, To: rows[len(rows)-1].time, Activities: kept}
	st.Notes = append(st.Notes, fmt.Sprintf("%d lines read, %d activities, %d of them kept from the API (more detailed)", len(rows), len(kept), replacedByAPI))
	for op, n := range unknown {
		st.Notes = append(st.Notes, fmt.Sprintf("operation %q not recognized (%d lines): imported as deposits/withdrawals", op, n))
	}
	// Every line must be accounted for: per asset, the activities add up to
	// the file's total change.
	want := map[string]decimal.Decimal{}
	for _, r := range rows {
		k := model.CryptoOrFiat(r.coin).Key()
		want[k] = want[k].Add(r.change)
	}
	got := model.Holdings(kept)
	for k, w := range want {
		if g := got[k].Qty; !g.Equal(w) {
			st.Notes = append(st.Notes, fmt.Sprintf("check: %s adds up to %s in the activities but %s in the file", k, g, w))
		}
	}
	return st, nil
}

// emitTrades pairs what came in with what went out. Binance writes each
// fill as consecutive rows (spend, buy, fee), so pairing by position is
// exact when the counts agree; otherwise, with one asset each way, the group
// is one trade; anything else becomes separate movements (quantities stay
// right, prices are unknown).
func emitTrades(g, ins, outs, fees []stmtRow, t time.Time, key string, emit func(model.Activity, time.Time, string)) {
	note := g[0].opName
	if family(g[0].op) == "trade" {
		note = ""
	}
	if len(ins) == len(outs) && len(ins) > 0 && (len(fees) == 0 || len(fees) == len(ins)) {
		for i := range ins {
			a := tradeOf(stmtAmt(ins[i]), stmtAmt(outs[i]))
			if len(fees) > 0 {
				f := stmtAmt(fees[i])
				a.Fee = &f
			}
			a.Note = note
			emit(a, t, fmt.Sprintf("%s:%d", key, ins[i].line))
		}
		return
	}
	sum := func(rs []stmtRow) map[string]model.Amount {
		m := map[string]model.Amount{}
		for _, r := range rs {
			a := stmtAmt(r)
			cur, ok := m[r.coin]
			if !ok {
				cur = model.Amount{Asset: a.Asset}
			}
			cur.Qty = cur.Qty.Add(a.Qty)
			m[r.coin] = cur
		}
		return m
	}
	in, out, fee := sum(ins), sum(outs), sum(fees)
	if len(in) == 1 && len(out) == 1 {
		var a model.Activity
		for _, i := range in {
			for _, o := range out {
				a = tradeOf(i, o)
			}
		}
		n := 0
		for _, f := range fee {
			f := f
			if n == 0 {
				a.Fee = &f
			} else {
				emit(model.Activity{Type: model.Fee, Base: f, Note: "trading fee"}, t, fmt.Sprintf("%s:fee%d", key, n))
			}
			n++
		}
		a.Note = note
		emit(a, t, fmt.Sprintf("%s:%d", key, g[0].line))
		return
	}
	for _, r := range g {
		typ := model.Deposit
		if r.change.IsNegative() {
			typ = model.Withdrawal
		}
		if r.op == "transaction fee" || r.op == "fee" {
			typ = model.Fee
		}
		emit(model.Activity{Type: typ, Base: stmtAmt(r), Note: strings.TrimSpace(r.opName + " (unpaired)")}, t, fmt.Sprintf("%s:leg%d", key, r.line))
	}
}

// preferAPI swaps statement activities for the API's when both describe the
// same movement (same assets and quantities, within a day and a half: the
// export's time zone may be off). API activities carry hashes, ids and fiat
// amounts the export lacks. Returns the merged list and how many were swapped.
func preferAPI(stmt, existing []model.Activity) ([]model.Activity, int) {
	type cand struct {
		a        model.Activity
		consumed bool // matched to a statement activity
		included bool // already in the output
	}
	var api []*cand
	for _, a := range existing {
		if !strings.HasPrefix(a.NativeID, "statement:") {
			api = append(api, &cand{a: a})
		}
	}
	const window = 36 * time.Hour
	near := func(a, b time.Time) bool { d := a.Sub(b); return d < window && d > -window }
	sameAmt := func(a, b model.Amount) bool {
		return a.Asset.Symbol == b.Asset.Symbol && a.Qty.Round(8).Equal(b.Qty.Round(8))
	}
	gross := func(a model.Activity) model.Amount { // withdrawals: sent plus fee in the same asset
		g := a.Base
		if a.Fee != nil && a.Fee.Asset.Symbol == g.Asset.Symbol {
			g.Qty = g.Qty.Add(a.Fee.Qty)
		}
		return g
	}
	match := func(s model.Activity, c model.Activity) bool {
		if !near(s.Time, c.Time) {
			return false
		}
		switch s.Type {
		case model.Deposit, model.Reward:
			if c.Type == s.Type || c.Type == model.Deposit || c.Type == model.Reward {
				return sameAmt(s.Base, c.Base)
			}
			// A card purchase: the statement only shows the crypto received;
			// the API has the trade with the euros paid.
			return c.Type == model.Trade && c.Side == model.Buy && sameAmt(s.Base, c.Base)
		case model.Withdrawal:
			return c.Type == model.Withdrawal && (sameAmt(s.Base, gross(c)) || sameAmt(s.Base, c.Base))
		case model.Trade:
			if c.Type != model.Trade || s.Quote == nil || c.Quote == nil {
				return false
			}
			return (sameAmt(s.Base, c.Base) && sameAmt(*s.Quote, *c.Quote)) || (sameAmt(s.Base, *c.Quote) && sameAmt(*s.Quote, c.Base))
		}
		return false
	}
	var out []model.Activity
	swapped := 0
	for _, s := range stmt {
		var hit *cand
		for _, c := range api {
			if !c.consumed && match(s, c.a) {
				hit = c
				break
			}
		}
		if hit == nil {
			out = append(out, s)
			continue
		}
		hit.consumed = true
		swapped++
		if !hit.included {
			hit.included = true
			out = append(out, hit.a)
		}
		// A card purchase is a fiat deposit plus a trade on the API side:
		// keep them together (the statement's own lines for it then match
		// these and are dropped).
		if stem, ok := fiatStem(hit.a.NativeID); ok {
			for _, c := range api {
				if !c.included && strings.HasPrefix(c.a.NativeID, stem) {
					c.included = true
					out = append(out, c.a)
				}
			}
		}
	}
	return out, swapped
}

func fiatStem(id string) (string, bool) {
	if !strings.HasPrefix(id, "fiat-payment:") {
		return "", false
	}
	rest := strings.TrimPrefix(id, "fiat-payment:")
	order, _, _ := strings.Cut(rest, ":")
	return "fiat-payment:" + order, true
}
