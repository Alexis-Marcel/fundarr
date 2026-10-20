package traderepublic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/coder/websocket"
	"github.com/shopspring/decimal"
)

// The websocket the web app reads live data from (wss://api.traderepublic.com),
// signed in with the session cookies:
//
//	→ connect 31 {"locale":"en","platformId":"webtrading","clientId":"app.traderepublic.com",…}
//	← connected
//	→ sub 1 {"type":"accountPairs"}
//	← 1 A {…}          the answer (D: a delta on the previous one, E: an error, C: closed)
//	→ unsub 1
//
// Topics read: accountPairs (each account: its securities and cash account
// numbers, productType DEFAULT or TAX_WRAPPER for the PEA),
// compactPortfolioByType {secAccNo} (positions: instrumentId = ISIN, netSize),
// cash (no parameter: amount and currency, per cash account or not; see
// cashBalances).

var connectMsg = map[string]string{"locale": locale, "platformId": "webtrading", "platformVersion": "chrome - 146.0.0",
	"clientId": "app.traderepublic.com", "clientVersion": "5582"}

type wsConn struct {
	c    *websocket.Conn
	next int
	logf func(string, ...any)
}

func (c *client) dial(ctx context.Context) (*wsConn, error) {
	u, _ := url.Parse(c.base)
	ws := "wss://" + u.Host
	if u.Scheme == "http" {
		ws = "ws://" + u.Host
	}
	var cookie []string
	for _, k := range c.cookies() {
		cookie = append(cookie, k.Name+"="+k.Value)
	}
	h := http.Header{}
	h.Set("Cookie", strings.Join(cookie, "; "))
	h.Set("User-Agent", c.ua)
	conn, _, err := websocket.Dial(ctx, ws, &websocket.DialOptions{HTTPHeader: h, HTTPClient: &http.Client{Timeout: 30 * time.Second, Transport: c.http.Transport}})
	if err != nil {
		return nil, fmt.Errorf("websocket: %w", scrub(err))
	}
	conn.SetReadLimit(16 << 20)
	w := &wsConn{c: conn, logf: c.logf}
	msg, _ := json.Marshal(connectMsg)
	if err := conn.Write(ctx, websocket.MessageText, []byte("connect 31 "+string(msg))); err != nil {
		w.close()
		return nil, fmt.Errorf("websocket: %w", err)
	}
	_, data, err := conn.Read(ctx)
	if err != nil {
		w.close()
		return nil, fmt.Errorf("websocket: %w", err)
	}
	if string(data) != "connected" {
		w.close()
		return nil, fmt.Errorf("websocket: refused (%.40s)", data)
	}
	c.logf("websocket connected")
	return w, nil
}

func (w *wsConn) close() { w.c.Close(websocket.StatusNormalClosure, "") }

// sub asks for one topic and returns its first full answer.
func (w *wsConn) sub(ctx context.Context, payload map[string]any) (json.RawMessage, error) {
	w.next++
	id := strconv.Itoa(w.next)
	b, _ := json.Marshal(payload)
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := w.c.Write(ctx, websocket.MessageText, []byte("sub "+id+" "+string(b))); err != nil {
		return nil, err
	}
	defer w.c.Write(context.Background(), websocket.MessageText, []byte("unsub "+id))
	prev := ""
	for {
		_, data, err := w.c.Read(ctx)
		if err != nil {
			return nil, fmt.Errorf("websocket %s: %w", payload["type"], err)
		}
		msg := string(data)
		gotID, rest, _ := strings.Cut(msg, " ")
		if gotID != id {
			continue
		}
		code, body, _ := strings.Cut(rest, " ")
		switch code {
		case "A":
			prev = body
			return json.RawMessage(body), nil
		case "D":
			if prev == "" {
				return nil, errors.New("websocket: a delta without an answer")
			}
			return json.RawMessage(applyDelta(prev, body)), nil
		case "E":
			var e struct {
				Errors []struct {
					ErrorCode string `json:"errorCode"`
				} `json:"errors"`
			}
			_ = json.Unmarshal([]byte(body), &e)
			code := "error"
			if len(e.Errors) > 0 {
				code = e.Errors[0].ErrorCode
			}
			return nil, fmt.Errorf("websocket %s: %s", payload["type"], code)
		case "C":
			return nil, fmt.Errorf("websocket %s: closed", payload["type"])
		}
	}
}

// applyDelta rebuilds an answer from the previous one: tab-separated
// "=n" (keep n characters), "-n" (drop n), "+text" (insert, URL-encoded).
func applyDelta(prev, delta string) string {
	p := []rune(prev)
	var out strings.Builder
	i := 0
	for _, d := range strings.Split(delta, "\t") {
		if d == "" {
			continue
		}
		n, _ := strconv.Atoi(d[1:])
		switch d[0] {
		case '+':
			s, err := url.QueryUnescape(d[1:])
			if err != nil {
				s = d[1:]
			}
			out.WriteString(strings.TrimSpace(s))
		case '=':
			end := min(i+n, len(p))
			out.WriteString(string(p[i:end]))
			i = end
		case '-':
			i = min(i+n, len(p))
		}
	}
	return out.String()
}

// pairAccount is one account of accountPairs.
type pairAccount struct {
	SecAccNo, CashAccNo, Product string
}

// strField returns the first of keys that holds a non-empty string or number.
func strField(m map[string]any, keys ...string) string {
	for _, k := range keys {
		switch v := m[k].(type) {
		case string:
			if v != "" {
				return v
			}
		case float64:
			return strconv.FormatFloat(v, 'f', -1, 64)
		case json.Number:
			return v.String()
		}
	}
	return ""
}

func decodeAny(raw json.RawMessage) any {
	var v any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	_ = d.Decode(&v)
	return v
}

// objects lists every JSON object in v, depth first.
func objects(v any, out *[]map[string]any) {
	switch x := v.(type) {
	case map[string]any:
		*out = append(*out, x)
		for _, c := range x {
			objects(c, out)
		}
	case []any:
		for _, c := range x {
			objects(c, out)
		}
	}
}

func parsePairs(raw json.RawMessage) []pairAccount {
	var objs []map[string]any
	objects(decodeAny(raw), &objs)
	var out []pairAccount
	for _, o := range objs {
		sec := strField(o, "securitiesAccountNumber", "secAccNo", "securitiesAccount")
		if sec == "" {
			continue
		}
		out = append(out, pairAccount{SecAccNo: sec, CashAccNo: strField(o, "cashAccountNumber", "cashAccNo", "cashAccount"),
			Product: strField(o, "productType", "accountType", "type", "product")})
	}
	return out
}

// cryptoID is Trade Republic's pseudo-ISIN of a coin: XF000BTC0017 is BTC.
var cryptoID = regexp.MustCompile(`^XF000([A-Z0-9]+?)\d{4}$`)

func positionAsset(id, category, name string) model.Asset {
	if m := cryptoID.FindStringSubmatch(id); m != nil || strings.EqualFold(category, "cryptos") {
		sym := id
		if m != nil {
			sym = m[1]
		}
		return model.CryptoOrFiat(sym)
	}
	a := model.Asset{Kind: model.Security, Symbol: id, Name: name}
	if isinRe.MatchString(id) {
		a.ISIN = id
	}
	return a
}

type position struct {
	Asset model.Asset
	Qty   decimal.Decimal
}

func parsePortfolio(raw json.RawMessage) ([]position, error) {
	var p struct {
		Categories []struct {
			CategoryType string `json:"categoryType"`
			Positions    []struct {
				InstrumentID string      `json:"instrumentId"`
				ISIN         string      `json:"isin"`
				NetSize      json.Number `json:"netSize"`
				Quantity     json.Number `json:"quantity"`
				Name         string      `json:"name"`
			} `json:"positions"`
		} `json:"categories"`
		Positions []struct {
			InstrumentID string      `json:"instrumentId"`
			NetSize      json.Number `json:"netSize"`
		} `json:"positions"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, errors.New("websocket compactPortfolioByType: unexpected answer")
	}
	var out []position
	for _, c := range p.Categories {
		for _, x := range c.Positions {
			id := x.InstrumentID
			if id == "" {
				id = x.ISIN
			}
			n := x.NetSize
			if n == "" {
				n = x.Quantity
			}
			q, err := decimal.NewFromString(n.String())
			if id == "" || err != nil {
				continue
			}
			out = append(out, position{positionAsset(strings.ToUpper(id), c.CategoryType, x.Name), q})
		}
	}
	for _, x := range p.Positions {
		if q, err := decimal.NewFromString(x.NetSize.String()); err == nil && x.InstrumentID != "" {
			out = append(out, position{positionAsset(strings.ToUpper(x.InstrumentID), "", ""), q})
		}
	}
	return out, nil
}

type cashLine struct {
	Account string
	Amount  model.Amount
}

func parseCash(raw json.RawMessage) []cashLine {
	var objs []map[string]any
	objects(decodeAny(raw), &objs)
	var out []cashLine
	for _, o := range objs {
		ccy := strField(o, "currencyId", "currency")
		amt := strField(o, "amount", "value", "balance")
		if ccy == "" || amt == "" {
			continue
		}
		q, err := decimal.NewFromString(amt)
		if err != nil {
			continue
		}
		out = append(out, cashLine{Account: strField(o, "accountNumber", "cashAccountNumber"), Amount: model.Amount{Asset: model.CryptoOrFiat(ccy), Qty: q}})
	}
	return out
}

// balances reads what every account holds. hist is each account's cash
// in the export just downloaded (label → currency → amount; nil when
// none): it tells how to read a cash answer that does not name accounts.
func (c *client) balances(ctx context.Context, hist map[string]map[string]decimal.Decimal) ([]model.Balance, error) {
	w, err := c.dial(ctx)
	if err != nil {
		return nil, err
	}
	defer w.close()
	var accounts []pairAccount
	if raw, err := w.sub(ctx, map[string]any{"type": "accountPairs"}); err == nil {
		accounts = parsePairs(raw)
		c.logf("accountPairs: %d account(s)", len(accounts))
	} else {
		c.logf("accountPairs: %v; main account only", err)
	}
	if len(accounts) == 0 {
		sec, err := c.account(ctx)
		if err != nil {
			return nil, err
		}
		if sec == "" {
			return nil, errors.New("no securities account number")
		}
		accounts = []pairAccount{{SecAccNo: sec, Product: "DEFAULT"}}
	}
	var out []model.Balance
	for _, a := range accounts {
		lbl := accountLabel(a.Product)
		raw, err := w.sub(ctx, map[string]any{"type": "compactPortfolioByType", "secAccNo": a.SecAccNo})
		if err != nil {
			return nil, err
		}
		pos, err := parsePortfolio(raw)
		if err != nil {
			return nil, err
		}
		c.logf("positions (%s): %d", accountName(lbl), len(pos))
		for _, p := range pos {
			out = append(out, model.Balance{Account: lbl, Amount: model.Amount{Asset: p.Asset, Qty: p.Qty}})
		}
	}
	raw, err := w.sub(ctx, map[string]any{"type": "cash"})
	if err != nil {
		return nil, err
	}
	cash := parseCash(raw)
	c.logf("cash: %d line(s)", len(cash))
	out = append(out, cashBalances(accounts, cash, hist, c.logf)...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Account != out[j].Account {
			return out[i].Account < out[j].Account
		}
		return out[i].Asset.Key() < out[j].Asset.Key()
	})
	return out, nil
}

// cashBalances puts the cash answer in the accounts it belongs to.
//
// The cash topic takes no account: pytr and Sure read it as the session's
// own account, and Trade Republic may leave the PEA's cash out, give one
// line per cash account, or one figure for all. So:
//
//   - a line naming an account's cash number is that account's;
//   - a line naming none (or an unknown number) is the one account without
//     its line, or else the main account's if the export agrees, or else
//     every account's together (model.AllAccounts: compared summed);
//   - an account with no line has its cash taken from the history
//     (Unread), unless the export says the figures are for all accounts.
//
// Never is cash moved from one account to another to make them agree.
func cashBalances(accounts []pairAccount, cash []cashLine, hist map[string]map[string]decimal.Decimal, logf func(string, ...any)) []model.Balance {
	labelOf := map[string]string{} // cash account number → label
	var labels []string
	seenLbl := map[string]bool{}
	for _, a := range accounts {
		l := accountLabel(a.Product)
		if a.CashAccNo != "" {
			labelOf[a.CashAccNo] = l
		}
		if !seenLbl[l] {
			seenLbl[l] = true
			labels = append(labels, l)
		}
	}
	got := map[string]map[string]decimal.Decimal{} // label → currency → amount
	loose := map[string]decimal.Decimal{}          // currency → amount
	assets := map[string]model.Asset{}
	add := func(m map[string]decimal.Decimal, a model.Amount) {
		m[a.Asset.Symbol] = m[a.Asset.Symbol].Add(a.Qty)
		assets[a.Asset.Symbol] = a.Asset
	}
	for _, l := range cash {
		lbl, ok := labelOf[l.Account]
		if !ok {
			add(loose, l.Amount)
			continue
		}
		if got[lbl] == nil {
			got[lbl] = map[string]decimal.Decimal{}
		}
		add(got[lbl], l.Amount)
	}
	var missing []string
	for _, l := range labels {
		if got[l] == nil {
			missing = append(missing, l)
		}
	}
	// Where the loose lines go when read account by account.
	looseTo, looseOK := "", true
	switch {
	case len(loose) == 0:
	case len(missing) == 1:
		looseTo = missing[0]
	case slices.Contains(missing, ""):
		looseTo, looseOK = "", hist != nil // the main account's, if the export agrees
	default:
		looseOK = false
	}
	split := map[string]map[string]decimal.Decimal{}
	for l, m := range got {
		split[l] = m
	}
	if len(loose) > 0 && looseOK {
		split[looseTo] = loose
	}
	// The export tells which reading holds: each account's figure is its
	// cash, or the figures add up to all of it.
	ccys := map[string]bool{}
	for k := range assets {
		ccys[k] = true
	}
	for _, m := range hist {
		for k := range m {
			ccys[k] = true
		}
	}
	// Read without guessing: every line names its account, or the one
	// account left without its line is the loose lines'.
	reliable := len(loose) == 0 || len(missing) == 1
	perAccount := looseOK
	if hist != nil && looseOK && !fits(split, hist, ccys) {
		switch {
		case fits(map[string]map[string]decimal.Decimal{"": sumOf(got, loose)}, map[string]map[string]decimal.Decimal{"": sumOf(hist, nil)}, ccys):
			perAccount = false // the figures are for all accounts together
		default:
			perAccount = reliable // neither fits: a real gap, read as given
		}
	}
	var out []model.Balance
	asset := func(k string) model.Asset {
		if a, ok := assets[k]; ok {
			return a
		}
		return model.CryptoOrFiat(k)
	}
	if !perAccount {
		logf("cash: Trade Republic gave one figure for several accounts: compared summed over them")
		for k, q := range sumOf(got, loose) {
			out = append(out, model.Balance{Account: model.AllAccounts, Amount: model.Amount{Asset: asset(k), Qty: q}})
		}
		return out
	}
	for _, l := range labels {
		m, ok := split[l]
		if !ok {
			logf("cash (%s): not in Trade Republic's answer: taken from the history", accountName(l))
			unread := map[string]bool{"EUR": true}
			for k := range hist[l] {
				unread[k] = true
			}
			for k := range unread {
				out = append(out, model.Balance{Account: l, Amount: model.Amount{Asset: asset(k), Qty: decimal.Zero}, Unread: true})
			}
			continue
		}
		for k, q := range m {
			out = append(out, model.Balance{Account: l, Amount: model.Amount{Asset: asset(k), Qty: q}})
		}
	}
	return out
}

// fits: each account's figure in got is its cash in hist, to the cent.
func fits(got, hist map[string]map[string]decimal.Decimal, ccys map[string]bool) bool {
	cent := decimal.New(5, -3)
	for l, m := range got {
		for k := range ccys {
			if m[k].Sub(hist[l][k]).Abs().GreaterThan(cent) {
				return false
			}
		}
	}
	return true
}

// sumOf adds up every account's cash, and extra.
func sumOf(m map[string]map[string]decimal.Decimal, extra map[string]decimal.Decimal) map[string]decimal.Decimal {
	out := map[string]decimal.Decimal{}
	for k, q := range extra {
		out[k] = q
	}
	for _, x := range m {
		for k, q := range x {
			out[k] = out[k].Add(q)
		}
	}
	return out
}
