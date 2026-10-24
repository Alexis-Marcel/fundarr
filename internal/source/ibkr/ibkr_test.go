package ibkr

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// Made-up credentials and account: nothing here is a real account.
const (
	testToken = "000000000000000000000001"
	testQuery = "100001"
	testRef   = "2000000001"
)

// The responses below follow IBKR's documented FlexStatementResponse.
func respOK(ref string) string {
	return `<FlexStatementResponse timestamp="23 October, 2026 09:00 PM EDT"><Status>Success</Status>` +
		`<ReferenceCode>` + ref + `</ReferenceCode>` +
		`<url>https://gdcdyn.interactivebrokers.com/Universal/servlet/FlexStatementService.GetStatement</url></FlexStatementResponse>`
}

func respFail(code, msg string) string {
	return `<FlexStatementResponse timestamp="23 October, 2026 09:00 PM EDT"><Status>Fail</Status>` +
		`<ErrorCode>` + code + `</ErrorCode><ErrorMessage>` + msg + `</ErrorMessage></FlexStatementResponse>`
}

// fixture is an Activity Flex Query report in XML, with the sections the
// Setup asks for. Its Cash Report and Open Positions are what its history
// adds up to.
const fixture = `<?xml version="1.0" encoding="UTF-8"?>
<FlexQueryResponse queryName="Fundarr" type="AF">
<FlexStatements count="2">
<FlexStatement accountId="U0000001" fromDate="20251101" toDate="20261023" period="Custom" whenGenerated="20261024;030000">
<Trades>
<Trade accountId="U0000001" currency="USD" assetCategory="CASH" symbol="EUR.USD" description="EUR.USD" isin="" transactionID="9000000001" tradeID="800001" dateTime="20260106;101500" tradeDate="20260106" buySell="SELL" quantity="-1850" tradePrice="1.081" proceeds="1999.85" taxes="0" ibCommission="-2" ibCommissionCurrency="USD" netCash="1997.85" levelOfDetail="EXECUTION" />
<Trade accountId="U0000001" currency="EUR" assetCategory="STK" symbol="IWDA" description="ISHARES CORE MSCI WORLD" isin="IE00B4L5Y983" transactionID="9000000002" tradeID="800002" dateTime="20260107;093512" tradeDate="20260107" buySell="BUY" quantity="20" tradePrice="80" proceeds="-1600" taxes="0" ibCommission="-3" ibCommissionCurrency="EUR" netCash="-1603" levelOfDetail="EXECUTION" />
<Trade accountId="U0000001" currency="USD" assetCategory="STK" symbol="AAPL" description="APPLE INC" isin="US0378331005" transactionID="9000000003" tradeID="800003" dateTime="20260108;100001" tradeDate="20260108" buySell="BUY" quantity="10" tradePrice="150" proceeds="-1500" taxes="0" ibCommission="-1" ibCommissionCurrency="USD" netCash="-1501" levelOfDetail="EXECUTION" />
<Trade accountId="U0000001" currency="USD" assetCategory="STK" symbol="AAPL" description="APPLE INC" isin="US0378331005" transactionID="" tradeID="" dateTime="20260108;100001" tradeDate="20260108" buySell="BUY" quantity="10" tradePrice="150" proceeds="-1500" taxes="0" ibCommission="-1" ibCommissionCurrency="USD" netCash="-1501" levelOfDetail="ORDER" />
<Trade accountId="U0000001" currency="USD" assetCategory="STK" symbol="AAPL" description="APPLE INC" isin="US0378331005" transactionID="9000000004" tradeID="800004" dateTime="20260210;153000" tradeDate="20260210" buySell="SELL" quantity="-4" tradePrice="170" proceeds="680" taxes="-0.05" ibCommission="-1" ibCommissionCurrency="USD" netCash="678.95" levelOfDetail="EXECUTION" />
</Trades>
<CashTransactions>
<CashTransaction accountId="U0000001" currency="EUR" assetCategory="" symbol="" description="CASH RECEIPTS / ELECTRONIC FUND TRANSFERS" isin="" type="Deposits/Withdrawals" amount="5000" dateTime="20260105" settleDate="20260105" transactionID="9100000001" levelOfDetail="DETAIL" />
<CashTransaction accountId="U0000001" currency="EUR" assetCategory="" symbol="" description="EUR CREDIT INT FOR JAN-2026" isin="" type="Broker Interest Received" amount="0.42" dateTime="20260203" settleDate="20260203" transactionID="9100000002" levelOfDetail="DETAIL" />
<CashTransaction accountId="U0000001" currency="USD" assetCategory="" symbol="" description="SNAPSHOT VALUE BUNDLE FOR JAN 2026" isin="" type="Other Fees" amount="-1" dateTime="20260204" settleDate="20260204" transactionID="9100000003" levelOfDetail="DETAIL" />
<CashTransaction accountId="U0000001" currency="USD" assetCategory="STK" symbol="AAPL" description="AAPL(US0378331005) CASH DIVIDEND USD 0.25 PER SHARE (Ordinary Dividend)" isin="US0378331005" type="Dividends" amount="1.5" dateTime="20260215" settleDate="20260215" transactionID="9100000004" levelOfDetail="DETAIL" />
<CashTransaction accountId="U0000001" currency="USD" assetCategory="STK" symbol="AAPL" description="AAPL(US0378331005) CASH DIVIDEND USD 0.25 PER SHARE - US TAX" isin="US0378331005" type="Withholding Tax" amount="-0.23" dateTime="20260215" settleDate="20260215" transactionID="9100000005" levelOfDetail="DETAIL" />
<CashTransaction accountId="U0000001" currency="EUR" assetCategory="" symbol="" description="DISBURSEMENT INITIATED BY ACCOUNT HOLDER" isin="" type="Deposits/Withdrawals" amount="-500" dateTime="20260302" settleDate="20260302" transactionID="9100000006" levelOfDetail="DETAIL" />
</CashTransactions>
<CorporateActions>
<CorporateAction accountId="U0000001" currency="USD" assetCategory="STK" symbol="AAPL" description="APPLE INC" isin="US0378331005" actionDescription="AAPL(US0378331005) SPLIT 4 FOR 1" type="FS" dateTime="20260310;202500" quantity="18" proceeds="0" value="0" transactionID="9200000001" actionID="7000001" levelOfDetail="DETAIL" />
</CorporateActions>
<Transfers>
<Transfer accountId="U0000001" currency="USD" assetCategory="STK" symbol="MSFT" description="MICROSOFT CORP" isin="US5949181045" type="ACATS" direction="IN" date="20260120" dateTime="20260120" quantity="5" transferPrice="0" positionAmount="2000" transactionID="9300000001" levelOfDetail="TRANSACTION" />
</Transfers>
<OpenPositions>
<OpenPosition accountId="U0000001" currency="USD" assetCategory="STK" symbol="AAPL" description="APPLE INC" isin="US0378331005" position="24" markPrice="45" positionValue="1080" side="Long" levelOfDetail="SUMMARY" />
<OpenPosition accountId="U0000001" currency="EUR" assetCategory="STK" symbol="IWDA" description="ISHARES CORE MSCI WORLD" isin="IE00B4L5Y983" position="20" markPrice="85" positionValue="1700" side="Long" levelOfDetail="SUMMARY" />
<OpenPosition accountId="U0000001" currency="USD" assetCategory="STK" symbol="MSFT" description="MICROSOFT CORP" isin="US5949181045" position="5" markPrice="400" positionValue="2000" side="Long" levelOfDetail="SUMMARY" />
<OpenPosition accountId="U0000001" currency="USD" assetCategory="OPT" symbol="AAPL  261218C00200000" description="AAPL 18DEC26 200 C" isin="" position="1" markPrice="1" positionValue="100" side="Long" levelOfDetail="SUMMARY" />
</OpenPositions>
<CashReport>
<CashReportCurrency accountId="U0000001" currency="BASE_SUMMARY" levelOfDetail="BaseCurrency" startingCash="0" endingCash="2135.00" />
<CashReportCurrency accountId="U0000001" currency="EUR" levelOfDetail="Currency" startingCash="0" endingCash="1047.42" />
<CashReportCurrency accountId="U0000001" currency="USD" levelOfDetail="Currency" startingCash="0" endingCash="1176.07" />
</CashReport>
</FlexStatement>
<FlexStatement accountId="U0000002" fromDate="20251101" toDate="20261023" period="Custom" whenGenerated="20261024;030000">
<CashTransactions>
<CashTransaction accountId="U0000002" currency="EUR" type="Deposits/Withdrawals" amount="99" dateTime="20260105" transactionID="9100000099" levelOfDetail="DETAIL" />
</CashTransactions>
<CashReport />
</FlexStatement>
</FlexStatements>
</FlexQueryResponse>`

// empty is a report for a period with nothing in it.
const empty = `<?xml version="1.0" encoding="UTF-8"?>
<FlexQueryResponse queryName="Fundarr" type="AF"><FlexStatements count="1">
<FlexStatement accountId="U0000001" fromDate="20240101" toDate="20241231" period="Custom" whenGenerated="20261024;030000">
<Trades /><CashTransactions /><CorporateActions /><Transfers /><OpenPositions /><CashReport />
</FlexStatement></FlexStatements></FlexQueryResponse>`

// ---- fake Flex Web Service

type fake struct {
	t        *testing.T
	srv      *httptest.Server
	mu       sync.Mutex
	sends    []url.Values
	gets     int
	pending  int // GetStatement answers 1019 this many times first
	sendFail []string
	body     func(fd, td string) string
	lastSend url.Values
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, body: func(fd, td string) string {
		if fd == "" || fd <= "20260105" && td >= "20260105" {
			return fixture
		}
		return empty
	}}
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fake) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	q := r.URL.Query()
	if r.Method != http.MethodGet || r.Header.Get("User-Agent") == "" || q.Get("v") != "3" {
		f.t.Errorf("bad request %s %s UA=%q", r.Method, r.URL, r.Header.Get("User-Agent"))
	}
	w.Header().Set("Content-Type", "text/plain;charset=UTF-8")
	switch r.URL.Path {
	case "/SendRequest":
		f.sends = append(f.sends, q)
		if (q.Get("fd") == "") != (q.Get("td") == "") {
			fmt.Fprint(w, respFail("1020", "Invalid request or unable to validate request."))
			return
		}
		if len(f.sendFail) > 0 {
			code := f.sendFail[0]
			f.sendFail = f.sendFail[1:]
			fmt.Fprint(w, respFail(code, "Failure "+code+"."))
			return
		}
		switch {
		case q.Get("t") != testToken:
			fmt.Fprint(w, respFail("1015", "Token is invalid."))
		case q.Get("q") != testQuery:
			fmt.Fprint(w, respFail("1014", "Query is invalid."))
		default:
			f.lastSend = q
			fmt.Fprint(w, respOK(testRef))
		}
	case "/GetStatement":
		f.gets++
		switch {
		case q.Get("t") != testToken:
			fmt.Fprint(w, respFail("1015", "Token is invalid."))
		case q.Get("q") != testRef || f.lastSend == nil:
			fmt.Fprint(w, respFail("1017", "Reference code is invalid."))
		case f.pending > 0:
			f.pending--
			fmt.Fprint(w, respFail("1019", "Statement generation in progress. Please try again shortly."))
		default:
			fmt.Fprint(w, f.body(f.lastSend.Get("fd"), f.lastSend.Get("td")))
		}
	default:
		http.NotFound(w, r)
	}
}

var now = time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)

func (f *fake) source(t *testing.T, values map[string]string) *Source {
	t.Helper()
	v := map[string]string{"token": testToken, "query_id": testQuery, "base_url": f.srv.URL}
	for k, x := range values {
		v[k] = x
	}
	src, err := New(source.Config{Values: v, HTTP: f.srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	s := src.(*Source)
	s.c.now = func() time.Time { return now }
	s.c.sleep = func(context.Context, time.Duration) error { return nil }
	return s
}

func collect(t *testing.T, s *Source, since time.Time) map[string]model.Activity {
	t.Helper()
	out := map[string]model.Activity{}
	var logs []string
	err := s.Activities(context.Background(), source.Fetch{Since: since,
		Emit: func(a model.Activity) error {
			if _, dup := out[a.NativeID]; dup {
				t.Errorf("duplicate %s", a.NativeID)
			}
			if a.Time.IsZero() || a.Time.Location() != time.UTC {
				t.Errorf("%s: time %v not UTC", a.NativeID, a.Time)
			}
			out[a.NativeID] = a
			return nil
		},
		Progress: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) }})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func d(s string) decimal.Decimal { return decimal.RequireFromString(s) }

func TestHistoryReconciles(t *testing.T) {
	f := newFake(t)
	f.pending = 2
	s := f.source(t, map[string]string{"history_from": "2025-11-01"})
	acts := collect(t, s, time.Time{})

	if len(f.sends) != 1 || f.sends[0].Get("fd") != "20251101" || f.sends[0].Get("td") != "20261023" {
		t.Fatalf("SendRequest calls %v", f.sends)
	}
	if f.gets != 3 {
		t.Errorf("GetStatement called %d times, want 3 (two in progress)", f.gets)
	}

	check := func(id string, typ model.Type, side model.Side, key, qty string) model.Activity {
		t.Helper()
		a, ok := acts[id]
		if !ok {
			t.Fatalf("missing %s", id)
		}
		if a.Type != typ || a.Side != side || a.Base.Asset.Key() != key || !a.Base.Qty.Equal(d(qty)) {
			t.Errorf("%s = %s %s %s %s, want %s %s %s %s", id, a.Type, a.Side, a.Base.Qty, a.Base.Asset.Key(), typ, side, qty, key)
		}
		return a
	}
	// A conversion: EUR sold for USD, the commission apart.
	if a := check("trade:9000000001", model.Trade, model.Sell, "fiat:EUR", "1850"); a.Quote.Asset.Symbol != "USD" ||
		!a.Quote.Qty.Equal(d("1999.85")) || a.Fee == nil || !a.Fee.Qty.Equal(d("2")) {
		t.Errorf("conversion %+v", a)
	}
	// A purchase, by ISIN, with its commission as Fee and the time in UTC
	// (09:35:12 in New York is 14:35:12 UTC in January).
	a := check("trade:9000000002", model.Trade, model.Buy, "security:IE00B4L5Y983", "20")
	if a.Quote.Asset.Symbol != "EUR" || !a.Quote.Qty.Equal(d("1600")) || a.Fee.Asset.Symbol != "EUR" || !a.Fee.Qty.Equal(d("3")) {
		t.Errorf("IWDA buy %+v", a)
	}
	if want := time.Date(2026, 1, 7, 14, 35, 12, 0, time.UTC); !a.Time.Equal(want) || a.Base.Asset.Symbol != "IWDA" {
		t.Errorf("IWDA %v %s", a.Time, a.Base.Asset.Symbol)
	}
	check("trade:9000000003", model.Trade, model.Buy, "security:US0378331005", "10")
	check("trade:9000000004", model.Trade, model.Sell, "security:US0378331005", "4")
	check("trade:9000000004:tax", model.Tax, "", "fiat:USD", "0.05")
	check("cash:9100000001", model.Deposit, "", "fiat:EUR", "5000")
	check("cash:9100000002", model.Interest, "", "fiat:EUR", "0.42")
	check("cash:9100000003", model.Fee, "", "fiat:USD", "1")
	if a := check("cash:9100000004", model.Dividend, "", "fiat:USD", "1.5"); a.Quote == nil || a.Quote.Asset.ISIN != "US0378331005" || !a.Quote.Qty.IsZero() {
		t.Errorf("dividend %+v", a.Quote)
	}
	check("cash:9100000005", model.Tax, "", "fiat:USD", "0.23")
	check("cash:9100000006", model.Withdrawal, "", "fiat:EUR", "500")
	if a := check("corp:9200000001", model.Trade, model.Buy, "security:US0378331005", "18"); a.Quote == nil || !a.Quote.Qty.IsZero() {
		t.Errorf("split %+v", a.Quote)
	}
	check("transfer:9300000001", model.Deposit, "", "security:US5949181045", "5")
	if len(acts) != 13 {
		t.Errorf("%d activities (the ORDER row and the second account must be left out)", len(acts))
	}

	// Balances come from the same report, without asking again.
	bal, err := s.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(f.sends) != 1 {
		t.Errorf("Balances asked for another report")
	}
	if len(bal) != 5 { // AAPL, IWDA, MSFT, EUR, USD: no option, no BASE_SUMMARY
		t.Fatalf("balances %+v", bal)
	}
	list := make([]model.Activity, 0, len(acts))
	for _, a := range acts {
		list = append(list, a)
	}
	if gaps := model.Reconcile(list, bal, decimal.Zero); len(gaps) != 0 {
		t.Fatalf("history does not add up to balances: %+v", gaps)
	}
}

// Without an opening date, the first sync reads back a year at a time and
// stops after two empty years.
func TestWalkBack(t *testing.T) {
	f := newFake(t)
	s := f.source(t, nil)
	acts := collect(t, s, time.Time{})
	want := [][2]string{{"20251024", "20261023"}, {"20241024", "20251023"}, {"20231025", "20241023"}}
	if len(f.sends) != len(want) {
		t.Fatalf("SendRequest calls %v", f.sends)
	}
	for i, w := range want {
		if f.sends[i].Get("fd") != w[0] || f.sends[i].Get("td") != w[1] {
			t.Errorf("window %d: %s-%s, want %s-%s", i, f.sends[i].Get("fd"), f.sends[i].Get("td"), w[0], w[1])
		}
	}
	if len(acts) != 13 {
		t.Errorf("%d activities", len(acts))
	}
}

// A later sync reads from a week before the last one.
func TestIncremental(t *testing.T) {
	f := newFake(t)
	s := f.source(t, nil)
	collect(t, s, time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC))
	if len(f.sends) != 1 || f.sends[0].Get("fd") != "20260924" || f.sends[0].Get("td") != "20261023" {
		t.Fatalf("SendRequest calls %v", f.sends)
	}
}

// Balances on their own ask for the last week.
func TestBalancesAlone(t *testing.T) {
	f := newFake(t)
	f.body = func(string, string) string { return fixture }
	s := f.source(t, nil)
	bal, err := s.Balances(context.Background())
	if err != nil || len(bal) != 5 {
		t.Fatalf("balances %+v %v", bal, err)
	}
	if len(f.sends) != 1 || f.sends[0].Get("fd") != "20261017" {
		t.Fatalf("SendRequest calls %v", f.sends)
	}
	f.body = func(string, string) string { return strings.Replace(empty, "<CashReport />", "", 1) }
	s = f.source(t, nil)
	if _, err := s.Balances(context.Background()); err == nil || !strings.Contains(err.Error(), "Cash Report") {
		t.Fatalf("no Cash Report: %v", err)
	}
}

func TestErrors(t *testing.T) {
	f := newFake(t)
	ctx := context.Background()

	err := f.source(t, map[string]string{"token": "wrong"}).Test(ctx)
	if !errors.Is(err, ErrToken) || errors.Is(err, ErrQuery) || !strings.Contains(err.Error(), "token is invalid") {
		t.Errorf("bad token: %v", err)
	}
	if strings.Contains(fmt.Sprint(err), testToken) {
		t.Errorf("the token leaks: %v", err)
	}
	err = f.source(t, map[string]string{"query_id": "999"}).Test(ctx)
	if !errors.Is(err, ErrQuery) || errors.Is(err, ErrToken) || !strings.Contains(err.Error(), "Query ID") {
		t.Errorf("bad query: %v", err)
	}
	f.sendFail = []string{"1012"}
	if err := f.source(t, nil).Test(ctx); !errors.Is(err, ErrToken) || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired token: %v", err)
	}
	// Too many requests, then a server busy: tried again.
	f.sendFail = []string{"1018", "1009"}
	if err := f.source(t, nil).Test(ctx); err != nil {
		t.Errorf("transient errors: %v", err)
	}
	// A report that is not XML.
	f.body = func(string, string) string { return `"ClientAccountID","CurrencyPrimary","Symbol"` + "\n" }
	if err := f.source(t, nil).Test(ctx); err == nil || !strings.Contains(err.Error(), "XML") {
		t.Errorf("CSV report: %v", err)
	}
	if _, err := New(source.Config{Values: map[string]string{"token": testToken}}); err == nil {
		t.Error("no query id accepted")
	}
	if _, err := New(source.Config{Values: map[string]string{"token": testToken, "query_id": "1", "history_from": "2019"}}); err == nil {
		t.Error("bad opening date accepted")
	}
}

// A report still generating after every wait gives up with IBKR's message.
func TestStillGenerating(t *testing.T) {
	f := newFake(t)
	f.pending = 1000
	err := f.source(t, nil).Test(context.Background())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Code != "1019" {
		t.Fatalf("err %v", err)
	}
}

func TestFlexTime(t *testing.T) {
	cases := map[string]time.Time{
		"20260806;112923":      time.Date(2026, 8, 6, 15, 29, 23, 0, time.UTC),
		"2026-08-06, 11:29:23": time.Date(2026, 8, 6, 15, 29, 23, 0, time.UTC),
		"20260115":             time.Date(2026, 1, 15, 5, 0, 0, 0, time.UTC),
		"01/15/2026":           time.Date(2026, 1, 15, 5, 0, 0, 0, time.UTC),
		"":                     {},
	}
	for in, want := range cases {
		if got := flexTime(in); !got.Equal(want) {
			t.Errorf("flexTime(%q) = %v, want %v", in, got, want)
		}
	}
}
