package binance

import (
	"strings"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/pkg/model"
	"github.com/shopspring/decimal"
)

// A French export in UTC+2, like the one Binance's site generates.
const frStatement = "\xef\xbb\xbfIdentifiant utilisateur,Durée,Compte,Opération,Jeton,Change,Remarque\n" +
	// Card purchase: 19.62 EUR → 21.48905849 USDC (the API has it as fiat-payment).
	"1,22-03-10 00:07:55,Spot,Transaction Related,EUR,-19.62,x\n" +
	"1,22-03-10 00:07:55,Spot,Deposit,EUR,19.62,\n" +
	"1,22-03-10 00:07:55,Spot,Transaction Related,USDC,21.48905849,x\n" +
	// Two fills in the same second, two different assets (spend, buy, fee each).
	"1,24-01-06 23:38:45,Spot,Transaction Spend,USDT,-100,\n" +
	"1,24-01-06 23:38:45,Spot,Transaction Buy,BTC,0.002,\n" +
	"1,24-01-06 23:38:45,Spot,Transaction Fee,BTC,-0.000002,\n" +
	"1,24-01-06 23:38:45,Spot,Transaction Spend,USDT,-50,\n" +
	"1,24-01-06 23:38:45,Spot,Transaction Buy,ETH,0.02,\n" +
	"1,24-01-06 23:38:45,Spot,Transaction Fee,ETH,-0.00002,\n" +
	"1,24-01-06 23:38:45,Spot,Referral Commission,ETH,0.000001,\n" +
	// A sell: SOL out, USDT in.
	"1,23-12-27 00:25:58,Spot,Transaction Sold,SOL,-1,\n" +
	"1,23-12-27 00:25:58,Spot,Transaction Revenue,USDT,95,\n" +
	"1,23-12-27 00:25:58,Spot,Transaction Fee,USDT,-0.095,\n" +
	// A convert, its two sides a second apart (the API has it).
	"1,24-11-06 10:18:10,Spot,Binance Convert,AVAX,2.090301,\n" +
	"1,24-11-06 10:18:11,Spot,Binance Convert,EUR,-50,\n" +
	// Deposit and withdrawal the API knows (with hash and fee).
	"1,23-03-14 19:07:08,Spot,Deposit,BTC,0.00495438,\n" +
	"1,22-09-02 10:02:53,Spot,Withdraw,ETH,-0.05,\n" +
	// MATIC → POL migration, three days apart.
	"1,24-09-11 15:50:00,Spot,Asset Recovery,MATIC,-775.56119,\n" +
	"1,24-09-12 17:26:40,Spot,Token Swap - Distribution,POL,775.56119,\n" +
	// LUNA → LUNC redenomination, same second.
	"1,22-05-28 15:57:45,Spot,Token Swap - Redenomination/Rebranding,LUNA,-9.9905,\n" +
	"1,22-05-28 15:57:45,Spot,Token Swap - Redenomination/Rebranding,LUNC,9.9905,\n" +
	// Something new: kept as a movement so balances still add up.
	"1,25-01-01 12:00:00,Spot,Mystery Payout,DOGE,7,\n" +
	"1,25-01-02 12:00:00,Spot,Airdrop Assets,ARB,3,\n"

func TestStatementImport(t *testing.T) {
	b := &Binance{}
	utc := func(s string) time.Time { tt, _ := time.Parse("2006-01-02 15:04:05", s); return tt }
	api := []model.Activity{
		{NativeID: "fiat-payment:ORD1:funding", Time: utc("2022-03-09 22:07:55"), Type: model.Deposit, Base: model.Amt(model.CryptoOrFiat("EUR"), "19.62")},
		{NativeID: "fiat-payment:ORD1", Time: utc("2022-03-09 22:07:56"), Type: model.Trade, Side: model.Buy, Base: model.Amt(model.CryptoOrFiat("USDC"), "21.48905849"), Quote: amtPtr("EUR", "19.62")},
		{NativeID: "convert:777", Time: utc("2024-11-06 08:18:10"), Type: model.Trade, Side: model.Buy, Base: model.Amt(model.CryptoOrFiat("AVAX"), "2.090301"), Quote: amtPtr("EUR", "50")},
		{NativeID: "deposit:abc", Time: utc("2023-03-14 17:07:00"), Type: model.Deposit, Base: model.Amt(model.CryptoOrFiat("BTC"), "0.00495438"), TxID: "bbe2"},
		{NativeID: "withdrawal:w1", Time: utc("2022-09-02 08:02:50"), Type: model.Withdrawal, Base: model.Amt(model.CryptoOrFiat("ETH"), "0.04904"), Fee: amtPtr("ETH", "0.00096"), TxID: "0x9"},
		// Outside the statement: untouched by the import itself.
		{NativeID: "trade:ETHUSDT:1", Time: utc("2026-10-01 00:00:00"), Type: model.Trade, Side: model.Buy, Base: model.Amt(model.CryptoOrFiat("ETH"), "1"), Quote: amtPtr("USDT", "2000")},
	}
	st, err := b.Import("Binance-Historique-des-transactions-202605301012(UTC+2).csv", []byte(frStatement), api)
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range st.Notes {
		if strings.HasPrefix(n, "check:") {
			t.Error(n)
		}
	}
	if !st.From.Equal(utc("2022-03-09 22:07:55")) || !st.To.Equal(utc("2025-01-02 10:00:00")) {
		t.Fatalf("period %s → %s (times are UTC+2 in the file)", st.From, st.To)
	}
	by := map[string]model.Activity{}
	for _, a := range st.Activities {
		if _, dup := by[a.NativeID]; dup {
			t.Fatalf("duplicate %s", a.NativeID)
		}
		by[a.NativeID] = a
	}
	// API activities kept for the card purchase, the convert, the deposit and the withdrawal.
	for _, id := range []string{"fiat-payment:ORD1:funding", "fiat-payment:ORD1", "convert:777", "deposit:abc", "withdrawal:w1"} {
		if _, ok := by[id]; !ok {
			t.Errorf("API activity %s should be kept", id)
		}
	}
	if _, ok := by["trade:ETHUSDT:1"]; ok {
		t.Error("an API activity outside the period is not the importer's to return")
	}
	var trades, legs, swaps int
	for id, a := range by {
		if !strings.HasPrefix(id, "statement:") {
			continue
		}
		switch {
		case a.Type == model.Trade && strings.Contains(a.Note, "Token swap"):
			swaps++
			if a.Base.Asset.Symbol != "POL" || a.Quote.Asset.Symbol != "MATIC" {
				t.Errorf("migration %+v", a)
			}
		case a.Type == model.Trade:
			trades++
		case strings.Contains(a.Note, "unpaired"):
			legs++
		}
	}
	// BTC buy, ETH buy, SOL sell, LUNA→LUNC; card purchase and convert came from the API.
	if trades != 4 || swaps != 1 || legs != 0 {
		t.Fatalf("trades %d swaps %d legs %d: %+v", trades, swaps, legs, by)
	}
	for _, a := range by {
		if a.Type == model.Trade && a.Base.Asset.Symbol == "SOL" && (a.Side != model.Sell || a.Fee == nil || a.Fee.Asset.Symbol != "USDT") {
			t.Errorf("SOL sale %+v", a)
		}
		if a.Type == model.Trade && a.Base.Asset.Symbol == "BTC" && (a.Quote.Qty.String() != "100" || a.Fee.Qty.String() != "0.000002") {
			t.Errorf("BTC fill %+v", a)
		}
	}
	unknownNoted := false
	for _, n := range st.Notes {
		if strings.Contains(n, "Mystery Payout") {
			unknownNoted = true
		}
	}
	if !unknownNoted {
		t.Error("unknown operations are reported")
	}
	// The whole result adds up to the file, asset by asset.
	h := model.Holdings(st.Activities)
	if !h["crypto:POL"].Qty.Equal(decimal.RequireFromString("775.56119")) || !h["crypto:MATIC"].Qty.Equal(decimal.RequireFromString("-775.56119")) {
		t.Fatalf("holdings %v", h)
	}
}

func TestStatementErrors(t *testing.T) {
	b := &Binance{}
	for name, in := range map[string]string{
		"not csv":     "",
		"wrong file":  "Date,Amount\n2024-01-01,3\n",
		"bad date":    "UTC_Time,Account,Operation,Coin,Change\nyesterday,Spot,Deposit,BTC,1\n",
		"bad amount":  "UTC_Time,Account,Operation,Coin,Change\n2024-01-01 00:00:00,Spot,Deposit,BTC,lots\n",
		"no movement": "UTC_Time,Account,Operation,Coin,Change\n2024-01-01 00:00:00,Spot,Deposit,BTC,0\n",
	} {
		if _, err := b.Import("x.csv", []byte(in), nil); err == nil {
			t.Errorf("%s: error expected", name)
		}
	}
	// English export: UTC_Time is UTC whatever the file name says.
	st, err := b.Import("statement(UTC+2).csv", []byte("User_ID,UTC_Time,Account,Operation,Coin,Change,Remark\n1,2024-01-01 10:00:00,Spot,Deposit,BTC,1,\n1,2024-01-01 11:00:00,Spot,Withdraw,BTC,-0.5,\n"), nil)
	if err != nil || st.From.Hour() != 10 || len(st.Activities) != 2 {
		t.Fatalf("english %+v %v", st, err)
	}
}

func amtPtr(sym, q string) *model.Amount { a := model.Amt(model.CryptoOrFiat(sym), q); return &a }
