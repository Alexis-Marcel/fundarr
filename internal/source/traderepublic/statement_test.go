package traderepublic

import (
	"strings"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// The export's columns, as the app writes them since April 2026.
var header = []string{"datetime", "date", "account_type", "category", "type", "asset_class", "name", "symbol", "shares", "price", "amount",
	"fee", "tax", "currency", "original_amount", "original_currency", "fx_rate", "description", "transaction_id", "counterparty_name",
	"counterparty_iban", "payment_reference", "mcc_code"}

// line is one booking: only the columns that matter, the rest empty.
type line map[string]string

// A made-up account: a compte-titres (DEFAULT), a PEA and a junior
// account, with one line of each kind the importer knows.
var fixture = []line{
	{"datetime": "2025-01-02T09:00:00.000000Z", "type": "CUSTOMER_INPAYMENT", "amount": "1010.00", "fee": "-10.00", "description": "Card Top up with ****1234", "transaction_id": "t-inpay"},
	{"datetime": "2025-01-02T10:00:00.000000Z", "account_type": "TAX_WRAPPER", "type": "TRANSFER_INSTANT_INBOUND", "amount": "500.00", "description": "Incoming transfer", "transaction_id": "t-pea-in"},
	{"datetime": "2025-01-03T09:30:00.123456Z", "category": "TRADING", "type": "BUY", "asset_class": "STOCK", "name": "TotalEnergies", "symbol": "FR0000120271", "shares": "10.0000000000", "price": "60.00", "amount": "-600.00", "fee": "-1.00", "description": "BUY FR0000120271", "transaction_id": "t-buy-tte"},
	{"datetime": "2025-01-03T09:31:00Z", "account_type": "TAX_WRAPPER", "category": "TRADING", "type": "BUY", "asset_class": "FUND", "name": "iShares Core MSCI World", "symbol": "IE00B4L5Y983", "shares": "5", "price": "90", "amount": "-450.00", "description": "Savings plan execution IE00B4L5Y983", "transaction_id": "t-pea-buy"},
	{"datetime": "2025-01-03T09:32:00Z", "category": "TRADING", "type": "BUY", "asset_class": "STOCK", "name": "Apple", "symbol": "US0378331005", "shares": "2", "price": "180", "amount": "-360.00", "transaction_id": "t-buy-aapl"},
	{"datetime": "2025-01-04T12:00:00Z", "category": "TRADING", "type": "BUY", "asset_class": "CRYPTO", "name": "Bitcoin", "symbol": "BTC", "shares": "0.0010000000", "price": "60000", "amount": "-60.00", "fee": "-1.00", "transaction_id": "t-btc"},
	{"datetime": "2025-02-10T15:00:00Z", "category": "TRADING", "type": "SELL", "asset_class": "STOCK", "name": "TotalEnergies", "symbol": "FR0000120271", "shares": "-4", "price": "65", "amount": "260.00", "fee": "-1.00", "tax": "-2.50", "transaction_id": "t-sell-tte"},
	{"datetime": "2025-03-01T00:00:00.000000Z", "type": "DIVIDEND", "asset_class": "STOCK", "name": "TotalEnergies", "symbol": "FR0000120271", "shares": "6", "amount": "4.80", "tax": "-1.44", "description": "Cash Dividend for ISIN FR0000120271", "transaction_id": "t-div-tte"},
	// Booked, reversed (every amount negated), booked again.
	{"datetime": "2025-03-05T00:00:00Z", "type": "DIVIDEND", "asset_class": "STOCK", "name": "Apple", "symbol": "US0378331005", "shares": "2", "amount": "0.45", "tax": "-0.07", "original_amount": "0.50", "original_currency": "USD", "fx_rate": "0.9", "transaction_id": "t-div-aapl"},
	{"datetime": "2025-04-01T12:17:14.638045Z", "type": "DIVIDEND", "asset_class": "STOCK", "name": "Apple", "symbol": "US0378331005", "shares": "2", "amount": "-0.45", "tax": "0.07", "original_amount": "-0.50", "original_currency": "USD", "fx_rate": "0.9", "transaction_id": "t-div-aapl-rev"},
	{"datetime": "2025-04-01T12:44:58.343321Z", "type": "DIVIDEND", "asset_class": "STOCK", "name": "Apple", "symbol": "US0378331005", "shares": "2", "amount": "0.45", "tax": "-0.10", "original_amount": "0.50", "original_currency": "USD", "fx_rate": "0.9", "transaction_id": "t-div-aapl-new"},
	// A reversal whose original is in an earlier export.
	{"datetime": "2025-04-01T12:50:00Z", "type": "DIVIDEND", "asset_class": "STOCK", "name": "Apple", "symbol": "US0378331005", "shares": "2", "amount": "-1.00", "tax": "0.15", "transaction_id": "t-div-old-rev"},
	{"datetime": "2025-04-01T00:00:00Z", "type": "INTEREST_PAYMENT", "amount": "2.10", "tax": "-0.63", "description": "Interest payment", "transaction_id": "t-interest"},
	{"datetime": "2025-04-02T18:00:00Z", "type": "CARD_TRANSACTION", "amount": "-25.40", "description": "Boulangerie Paul", "mcc_code": "5462", "transaction_id": "t-card"},
	{"datetime": "2025-04-03T18:00:00Z", "type": "CARD_TRANSACTION", "amount": "-12.00", "fee": "-0.20", "original_amount": "-13.10", "original_currency": "USD", "description": "Coffee NYC", "transaction_id": "t-card-usd"},
	{"datetime": "2025-04-04T18:00:00Z", "type": "CARD_TRANSACTION_REFUND", "amount": "5.00", "description": "Boulangerie Paul", "transaction_id": "t-card-refund"},
	{"datetime": "2025-05-01T00:00:00Z", "type": "BENEFITS_SAVEBACK", "amount": "0.30", "description": "Saveback cash reward", "transaction_id": "t-saveback"},
	{"datetime": "2025-05-02T08:00:00Z", "category": "TRADING", "type": "BUY", "asset_class": "FUND", "name": "iShares Core MSCI World", "symbol": "IE00B4L5Y983", "shares": "0.0033", "price": "91", "amount": "-0.30", "description": "Saveback execution", "transaction_id": "t-saveback-buy"},
	{"datetime": "2025-05-03T00:00:00Z", "type": "STOCKPERK", "amount": "15.00", "transaction_id": "t-perk"},
	{"datetime": "2025-05-03T00:00:00Z", "account_type": "TAX_WRAPPER", "type": "PEA_MARKETING", "amount": "20.00", "transaction_id": "t-pea-bonus"},
	{"datetime": "2025-05-04T00:00:00Z", "type": "TAX_OPTIMIZATION", "amount": "0.000000", "tax": "3.20", "description": "Tax Optimisation", "transaction_id": "t-taxopt"},
	{"datetime": "2025-05-05T00:00:00Z", "type": "TAX_OPTIMIZATION", "amount": "0", "tax": "-1.10", "transaction_id": "t-taxopt2"},
	{"datetime": "2025-06-10T00:00:00Z", "category": "CORPORATE_ACTION", "type": "SPLIT", "asset_class": "STOCK", "name": "Apple", "symbol": "US0378331005", "shares": "2", "transaction_id": "t-split"},
	{"datetime": "2025-06-11T00:00:00Z", "category": "CORPORATE_ACTION", "type": "FREE_RECEIPT", "asset_class": "STOCK", "name": "Spin Co", "symbol": "DE000A0D9PT0", "shares": "1", "price": "10", "transaction_id": "t-free"},
	{"datetime": "2025-06-12T09:00:00Z", "category": "TRADING", "type": "BUY", "asset_class": "STOCK", "name": "Old Co", "symbol": "NL0000000013", "shares": "3", "price": "10", "amount": "-30.00", "transaction_id": "t-buy-old"},
	{"datetime": "2025-06-20T00:00:00Z", "category": "CORPORATE_ACTION", "type": "MERGER", "asset_class": "STOCK", "name": "Old Co", "symbol": "NL0000000013", "shares": "-3", "transaction_id": "t-merge-out"},
	{"datetime": "2025-06-20T00:00:00Z", "category": "CORPORATE_ACTION", "type": "MERGER", "asset_class": "STOCK", "name": "New Co", "symbol": "NL0000000021", "shares": "1.5", "transaction_id": "t-merge-in"},
	// A bond bought, then paid back: the corporate action and the payment.
	{"datetime": "2025-07-01T09:00:00Z", "category": "TRADING", "type": "BUY", "asset_class": "BOND", "name": "Bund Jul. 2025", "symbol": "DE0001141794", "shares": "20", "price": "0.99", "amount": "-19.80", "fee": "-1.00", "transaction_id": "t-bond"},
	{"datetime": "2025-07-15T00:00:00Z", "category": "CORPORATE_ACTION", "type": "REDEMPTION", "asset_class": "BOND", "name": "Bund Jul. 2025", "symbol": "DE0001141794", "shares": "-20", "price": "1", "transaction_id": "t-bond-redemption"},
	{"datetime": "2025-07-16T00:00:00Z", "type": "FINAL_MATURITY", "asset_class": "BOND", "name": "Bund Jul. 2025", "symbol": "DE0001141794", "shares": "20", "amount": "20.00", "transaction_id": "t-bond-paid"},
	// A knock-out certificate gone worthless: no payment.
	{"datetime": "2025-07-20T09:00:00Z", "category": "TRADING", "type": "BUY", "asset_class": "DERIVATIVE", "name": "Turbo Long", "symbol": "DE000TT2G3G7", "shares": "10", "price": "1", "amount": "-10.00", "transaction_id": "t-ko-buy"},
	{"datetime": "2025-07-25T00:00:00Z", "category": "CORPORATE_ACTION", "type": "KNOCK_OUT", "asset_class": "DERIVATIVE", "name": "Turbo Long", "symbol": "DE000TT2G3G7", "shares": "-10", "transaction_id": "t-ko"},
	{"datetime": "2025-08-01T00:00:00Z", "type": "TRANSFER_OUT", "amount": "-100.00", "description": "Outgoing transfer", "transaction_id": "t-out"},
	{"datetime": "2025-08-02T00:00:00Z", "type": "GIFT", "amount": "-51.00", "transaction_id": "t-gift-out"},
	{"datetime": "2025-08-03T00:00:00Z", "type": "GIFT", "amount": "26.00", "transaction_id": "t-gift-in"},
	{"datetime": "2025-08-04T00:00:00Z", "type": "MYSTERY_PAYOUT", "amount": "7.00", "transaction_id": "t-mystery"},
	{"datetime": "2025-08-05T00:00:00Z", "type": "PRIVATE_MARKET_BUY", "asset_class": "PRIVATE_FUND", "name": "Private Equity", "symbol": "LU3170240538", "shares": "100", "amount": "-100.00", "transaction_id": "t-private"},
	{"datetime": "2025-08-06T00:00:00Z", "account_type": "JUNIOR", "type": "TRANSFER_IN", "amount": "1.00", "transaction_id": "t-junior"},
	// No transaction id: a hash of the line stands for it.
	{"datetime": "2025-08-07T00:00:00Z", "type": "CARD_TRANSACTION", "amount": "-3.00", "description": "Metro"},
}

// render writes the fixture as a file: sep is the separator, quoted puts
// every field in quotes, valueDate adds the older value_date column.
func render(lines []line, sep string, quoted, valueDate bool) string {
	cols := append([]string(nil), header...)
	if valueDate {
		cols = append(cols[:19:19], append([]string{"value_date"}, cols[19:]...)...)
	}
	q := func(s string) string {
		if quoted {
			return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
		}
		return s
	}
	var b strings.Builder
	for i, c := range cols {
		if i > 0 {
			b.WriteString(sep)
		}
		b.WriteString(q(c))
	}
	b.WriteString("\n")
	for _, l := range lines {
		for i, c := range cols {
			if i > 0 {
				b.WriteString(sep)
			}
			v := l[c]
			switch c {
			case "date":
				v = l["datetime"][:10]
			case "account_type":
				if v == "" {
					v = "DEFAULT"
				}
			case "category":
				if v == "" {
					v = "CASH"
				}
			case "currency":
				v = "EUR"
			case "value_date":
				v = l["datetime"][:10]
			}
			b.WriteString(q(v))
		}
		b.WriteString("\n")
	}
	return b.String()
}

func held(acts []model.Activity, account string) map[string]decimal.Decimal {
	var mine []model.Activity
	for _, a := range acts {
		if a.Account == account {
			mine = append(mine, a)
		}
	}
	out := map[string]decimal.Decimal{}
	for k, a := range model.Holdings(mine) {
		if !a.Qty.IsZero() {
			out[k] = a.Qty
		}
	}
	return out
}

func TestImport(t *testing.T) {
	variants := map[string]string{
		"comma":     render(fixture, ",", false, false),
		"semicolon": "\xef\xbb\xbf" + render(fixture, ";", false, true),
		"quoted":    render(fixture, ",", true, false),
	}
	var first []model.Activity
	for name, data := range variants {
		t.Run(name, func(t *testing.T) {
			st, err := (&Source{}).Import("export.csv", []byte(data), nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, n := range st.Notes {
				if strings.HasPrefix(n, "check:") {
					t.Error(n)
				}
			}
			if !st.From.Equal(time.Date(2025, 1, 2, 9, 0, 0, 0, time.UTC)) || !st.To.Equal(time.Date(2025, 8, 7, 0, 0, 0, 0, time.UTC)) {
				t.Errorf("period %s → %s", st.From, st.To)
			}
			if strings.Join(st.Accounts, ",") != ",Junior,PEA" {
				t.Errorf("accounts %q", st.Accounts)
			}
			checkActivities(t, st.Activities)
			notes := strings.Join(st.Notes, "\n")
			for _, want := range []string{"1 dividend(s) booked again", "1 corporate action line(s) left out", "CASH/MYSTERY_PAYOUT"} {
				if !strings.Contains(notes, want) {
					t.Errorf("notes lack %q:\n%s", want, notes)
				}
			}
			if first == nil {
				first = st.Activities
			} else if len(first) != len(st.Activities) {
				t.Errorf("%d activities, %d in another format", len(st.Activities), len(first))
			} else {
				for i := range first {
					if first[i].NativeID != st.Activities[i].NativeID {
						t.Errorf("activity %d: %s, %s in another format", i, st.Activities[i].NativeID, first[i].NativeID)
					}
				}
			}
		})
	}
}

func checkActivities(t *testing.T, acts []model.Activity) {
	t.Helper()
	by := map[string]model.Activity{}
	for _, a := range acts {
		if _, dup := by[a.NativeID]; dup {
			t.Fatalf("duplicate %s", a.NativeID)
		}
		if a.Time.IsZero() || a.Type == "" || a.Base.Qty.IsNegative() {
			t.Fatalf("incomplete %+v", a)
		}
		by[a.NativeID] = a
	}
	d := decimal.RequireFromString
	eq := func(id string, typ model.Type, side model.Side, asset string, q string) model.Activity {
		t.Helper()
		a, ok := by[id]
		if !ok {
			t.Errorf("no activity %s", id)
			return a
		}
		if a.Type != typ || a.Side != side || a.Base.Asset.Key() != asset || !a.Base.Qty.Equal(d(q)) {
			t.Errorf("%s: %s %s %s %s, want %s %s %s %s", id, a.Type, a.Side, a.Base.Asset.Key(), a.Base.Qty, typ, side, asset, q)
		}
		return a
	}
	// Money in: the gross, with the top-up fee.
	if a := eq("tr:t-inpay", model.Deposit, "", "fiat:EUR", "1010"); a.Fee == nil || !a.Fee.Qty.Equal(d("10")) {
		t.Errorf("inpayment fee %+v", a.Fee)
	}
	// Securities by ISIN, crypto by ticker, the fee with the trade.
	if a := eq("tr:t-buy-tte", model.Trade, model.Buy, "security:FR0000120271", "10"); a.Quote == nil || !a.Quote.Qty.Equal(d("600")) ||
		a.Fee == nil || !a.Fee.Qty.Equal(d("1")) || a.Base.Asset.Name != "TotalEnergies" || a.Account != "" {
		t.Errorf("buy %+v", a)
	}
	if a := eq("tr:t-pea-buy", model.Trade, model.Buy, "security:IE00B4L5Y983", "5"); a.Account != "PEA" {
		t.Errorf("PEA buy in %q", a.Account)
	}
	eq("tr:t-btc", model.Trade, model.Buy, "crypto:BTC", "0.001")
	// A sale with its tax apart.
	if a := eq("tr:t-sell-tte", model.Trade, model.Sell, "security:FR0000120271", "4"); !a.Quote.Qty.Equal(d("260")) {
		t.Errorf("sell %+v", a)
	}
	eq("tr:t-sell-tte:tax", model.Tax, "", "fiat:EUR", "2.5")
	// Dividends: gross, of the security, tax apart.
	if a := eq("tr:t-div-tte", model.Dividend, "", "fiat:EUR", "4.8"); a.Quote == nil || a.Quote.Asset.ISIN != "FR0000120271" || !a.Quote.Qty.IsZero() {
		t.Errorf("dividend %+v", a.Quote)
	}
	eq("tr:t-div-tte:tax", model.Tax, "", "fiat:EUR", "1.44")
	// The booking reversed is gone with its reversal; the new one stays.
	for _, id := range []string{"tr:t-div-aapl", "tr:t-div-aapl:tax", "tr:t-div-aapl-rev"} {
		if _, ok := by[id]; ok {
			t.Errorf("%s should cancel out", id)
		}
	}
	if a := eq("tr:t-div-aapl-new", model.Dividend, "", "fiat:EUR", "0.45"); !strings.Contains(a.Note, "0.50 USD") {
		t.Errorf("dividend note %q", a.Note)
	}
	eq("tr:t-div-aapl-new:tax", model.Tax, "", "fiat:EUR", "0.1")
	eq("tr:t-div-old-rev", model.Withdrawal, "", "fiat:EUR", "0.85")
	// Interest gross, its tax apart.
	eq("tr:t-interest", model.Interest, "", "fiat:EUR", "2.1")
	eq("tr:t-interest:tax", model.Tax, "", "fiat:EUR", "0.63")
	// Card spending is money out, refunds in.
	if a := eq("tr:t-card", model.Withdrawal, "", "fiat:EUR", "25.4"); a.Note != "Card: Boulangerie Paul" {
		t.Errorf("card note %q", a.Note)
	}
	if a := eq("tr:t-card-usd", model.Withdrawal, "", "fiat:EUR", "12"); a.Fee == nil || !a.Fee.Qty.Equal(d("0.2")) || !strings.Contains(a.Note, "13.10 USD") {
		t.Errorf("card abroad %+v", a)
	}
	eq("tr:t-card-refund", model.Deposit, "", "fiat:EUR", "5")
	// Rewards: saveback, stock perk, the PEA bonus.
	eq("tr:t-saveback", model.Reward, "", "fiat:EUR", "0.3")
	eq("tr:t-saveback-buy", model.Trade, model.Buy, "security:IE00B4L5Y983", "0.0033")
	eq("tr:t-perk", model.Reward, "", "fiat:EUR", "15")
	if a := eq("tr:t-pea-bonus", model.Reward, "", "fiat:EUR", "20"); a.Account != "PEA" {
		t.Errorf("bonus in %q", a.Account)
	}
	// Tax optimisation: refunded, or charged.
	if a := eq("tr:t-taxopt", model.Deposit, "", "fiat:EUR", "3.2"); !strings.HasPrefix(a.Note, "Tax refund") {
		t.Errorf("tax refund note %q", a.Note)
	}
	eq("tr:t-taxopt2", model.Tax, "", "fiat:EUR", "1.1")
	// Corporate actions: a split adds shares at no cost, a free receipt is
	// a reward, a merger swaps the old shares for the new.
	if a := eq("tr:t-split", model.Trade, model.Buy, "security:US0378331005", "2"); a.Quote == nil || !a.Quote.Qty.IsZero() {
		t.Errorf("split %+v", a.Quote)
	}
	eq("tr:t-free", model.Reward, "", "security:DE000A0D9PT0", "1")
	if a := eq("tr:t-merge-in", model.Trade, model.Buy, "security:NL0000000021", "1.5"); a.Quote == nil || a.Quote.Asset.ISIN != "NL0000000013" || !a.Quote.Qty.Equal(d("3")) {
		t.Errorf("merger %+v", a.Quote)
	}
	if _, ok := by["tr:t-merge-out"]; ok {
		t.Error("the merger's old side is in the trade")
	}
	// The bond: the payment sells it, the corporate action is left out.
	eq("tr:t-bond-paid", model.Trade, model.Sell, "security:DE0001141794", "20")
	if _, ok := by["tr:t-bond-redemption"]; ok {
		t.Error("redemption counted twice")
	}
	if a := eq("tr:t-ko", model.Trade, model.Sell, "security:DE000TT2G3G7", "10"); !a.Quote.Qty.IsZero() {
		t.Errorf("knock-out %+v", a.Quote)
	}
	eq("tr:t-out", model.Withdrawal, "", "fiat:EUR", "100")
	eq("tr:t-gift-out", model.Withdrawal, "", "fiat:EUR", "51")
	eq("tr:t-gift-in", model.Deposit, "", "fiat:EUR", "26")
	eq("tr:t-mystery", model.Deposit, "", "fiat:EUR", "7")
	eq("tr:t-private", model.Trade, model.Buy, "security:LU3170240538", "100")
	if a := eq("tr:t-junior", model.Deposit, "", "fiat:EUR", "1"); a.Account != "Junior" {
		t.Errorf("junior in %q", a.Account)
	}
	var hashed int
	for id := range by {
		if strings.HasPrefix(id, "tr:line:") {
			hashed++
		}
	}
	if hashed != 1 {
		t.Errorf("%d lines without id", hashed)
	}

	// What each account holds once replayed.
	want := map[string]map[string]string{
		"": {"fiat:EUR": "-38.47", "security:FR0000120271": "6", "security:US0378331005": "4", "security:IE00B4L5Y983": "0.0033",
			"crypto:BTC": "0.001", "security:DE000A0D9PT0": "1", "security:NL0000000021": "1.5", "security:LU3170240538": "100"},
		"PEA":    {"fiat:EUR": "70", "security:IE00B4L5Y983": "5"},
		"Junior": {"fiat:EUR": "1"},
	}
	for acc, w := range want {
		got := held(acts, acc)
		if len(got) != len(w) {
			t.Errorf("%q holds %v, want %v", acc, got, w)
		}
		for k, q := range w {
			if !got[k].Equal(d(q)) {
				t.Errorf("%q holds %s %s, want %s", acc, got[k], k, q)
			}
		}
	}
}

func TestImportRejectsOtherFiles(t *testing.T) {
	for _, data := range []string{"", "a,b,c\n1,2,3\n", "\xef\xbb\xbfUser_ID,UTC_Time,Account,Operation,Coin,Change\n"} {
		if _, err := (&Source{}).Import("x.csv", []byte(data), nil); err == nil {
			t.Errorf("%q imported", data)
		}
	}
	// The header alone holds nothing.
	if _, err := (&Source{}).Import("x.csv", []byte(strings.Join(header, ",")+"\n"), nil); err == nil {
		t.Error("an empty export imported")
	}
}

func TestAccountLabel(t *testing.T) {
	for in, want := range map[string]string{"DEFAULT": "", "": "", "TAX_WRAPPER": "PEA", "PEA": "PEA", "SAVINGS_PEA": "PEA", "JUNIOR": "Junior", "PRIVATE_MARKETS": "Private markets"} {
		if got := accountLabel(in); got != want {
			t.Errorf("%s → %q, want %q", in, got, want)
		}
	}
}
