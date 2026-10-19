package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

const (
	testKey    = "key-123"
	testSecret = "secret-456"
)

var now = time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC)

func msAt(y int, m time.Month, d int) int64 {
	return time.Date(y, m, d, 10, 0, 0, 0, time.UTC).UnixMilli()
}

type trade struct {
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

// fake is a Binance API double serving fixtures shaped like the documented
// responses. It checks every signed request and honours time ranges,
// offsets and fromId so windowing and pagination are exercised.
type fake struct {
	t        *testing.T
	mu       sync.Mutex
	calls    map[string]int
	queries  map[string][]string // path -> raw queries
	tradeQ   map[string][]string // symbol -> fromId values
	limited  bool                // answer the next signed call with 429 once
	restrict map[string]bool

	deposits    []map[string]any
	withdrawals []map[string]any
	trades      map[string][]trade
	dividends   []map[string]any
	converts    []map[string]any
	known       map[string]bool // pairs myTrades accepts (listed + delisted with history)
}

func newFake(t *testing.T) *fake {
	f := &fake{t: t, calls: map[string]int{}, queries: map[string][]string{}, tradeQ: map[string][]string{},
		restrict: map[string]bool{"enableReading": true}, trades: map[string][]trade{}}

	// Crypto deposits: 0.5 BTC (2018, success), 3 LDO, a pending one ignored,
	// and 1500 rejected rows in one window to exercise offset paging.
	f.deposits = []map[string]any{
		{"id": "769800519366885376", "amount": "0.50000000", "coin": "BTC", "network": "BTC", "status": 1,
			"address": "bc1qxyz", "addressTag": "", "txId": "f00dbabe", "insertTime": msAt(2018, 3, 1), "completeTime": msAt(2018, 3, 1),
			"transferType": 0, "confirmTimes": "2/2", "unlockConfirm": 0, "walletType": 0},
		{"id": "2", "amount": "3", "coin": "LDO", "network": "ETH", "status": 6, "address": "0xabc", "txId": "0x1",
			"insertTime": msAt(2025, 1, 5), "completeTime": msAt(2025, 1, 5)},
		{"id": "3", "amount": "100", "coin": "USDT", "network": "TRX", "status": 0, "address": "T1", "txId": "t2",
			"insertTime": msAt(2025, 1, 6)},
	}
	for i := 0; i < 1500; i++ {
		f.deposits = append(f.deposits, map[string]any{"id": "rej" + strconv.Itoa(i), "amount": "1", "coin": "DOGE", "status": 2,
			"insertTime": msAt(2024, 6, 1) + int64(i)})
	}
	// Withdrawal of 0.2 BTC with a 0.0005 fee, plus a rejected one.
	f.withdrawals = []map[string]any{
		{"id": "b6ae22b3aa844210a7041aee7589627c", "amount": "0.2", "transactionFee": "0.0005", "coin": "BTC", "status": 6,
			"address": "bc1qledger", "txId": "beefcafe", "applyTime": "2025-02-01 08:00:00", "network": "BTC",
			"transferType": 0, "info": "", "confirmNo": 3, "walletType": 0, "txKey": "", "completeTime": "2025-02-01 08:30:00"},
		{"id": "x", "amount": "1", "transactionFee": "0", "coin": "BTC", "status": 3, "applyTime": "2025-02-02 08:00:00"},
	}
	// Spot trades.
	f.trades["BTCEUR"] = []trade{{ID: 28457, OrderID: 100234, Price: "60000", Qty: "0.01", QuoteQty: "600",
		Commission: "0.0005", CommissionAsset: "BNB", Time: msAt(2024, 2, 1), IsBuyer: true}}
	// 1000 buy/sell pairs that cancel out, then a real sell of 0.1 BTC:
	// paging by fromId must reach the last one.
	var bt []trade
	for i := int64(0); i < 1000; i++ {
		bt = append(bt, trade{ID: 500 + i, Price: "60000", Qty: "0.001", QuoteQty: "60", Commission: "0", CommissionAsset: "USDT",
			Time: msAt(2024, 3, 1) + i, IsBuyer: i%2 == 0})
	}
	bt = append(bt, trade{ID: 1500, Price: "60000", Qty: "0.1", QuoteQty: "", Commission: "6", CommissionAsset: "USDT",
		Time: msAt(2024, 4, 1), IsBuyer: false})
	f.trades["BTCUSDT"] = bt
	// A delisted pair (absent from exchangeInfo): sold 0.01 ETH for 15 BUSD.
	f.trades["ETHBUSD"] = []trade{{ID: 77, Price: "1500", Qty: "0.01", QuoteQty: "15", Commission: "0", CommissionAsset: "BUSD",
		Time: msAt(2023, 2, 1), IsBuyer: false}}
	f.known = map[string]bool{"ETHBUSD": true}
	for _, s := range []string{"BTCEUR", "BTCUSDT", "ETHUSDT", "ETHBTC", "BNBUSDT", "LDOUSDT", "TRXUSDT", "XYZUSDT", "DOGEUSDT", "SHIBDOGE"} {
		f.known[s] = true
	}
	// Distributions: 10 TRX airdrop, 0.5 TRX taken back, 1.5 USDT earn
	// interest, and 600 small XYZ rewards (forces range splitting).
	f.dividends = []map[string]any{
		{"id": 242006910, "amount": "10.00000000", "asset": "TRX", "divTime": msAt(2024, 5, 1), "enInfo": "TRX airdrop", "tranId": 2968885920},
		{"id": 242006911, "amount": "0.5", "asset": "TRX", "divTime": msAt(2024, 5, 2), "enInfo": "adjustment", "tranId": 2968885921, "direction": -1},
		{"id": 242006912, "amount": "1.5", "asset": "USDT", "divTime": msAt(2025, 3, 1), "enInfo": "Simple Earn Flexible Interest", "tranId": 3},
	}
	for i := 0; i < 600; i++ {
		f.dividends = append(f.dividends, map[string]any{"id": 300000 + i, "amount": "1", "asset": "XYZ",
			"divTime": msAt(2025, 6, 1) + int64(i)*int64(time.Hour/time.Millisecond), "enInfo": "XYZ distribution", "tranId": 4000 + i})
	}
	// Convert 1000 USDT to 0.5 ETH, and a failed convert.
	f.converts = []map[string]any{
		{"quoteId": "f3b91c525b2644c7bc1e1cd31b6e1aa6", "orderId": 940708407462087195, "orderStatus": "SUCCESS", "fromAsset": "USDT",
			"fromAmount": "1000.00000000", "toAsset": "ETH", "toAmount": "0.50000000", "ratio": "0.0005", "inverseRatio": "2000", "createTime": msAt(2024, 8, 1)},
		{"quoteId": "q2", "orderId": 2, "orderStatus": "FAIL", "fromAsset": "USDT", "fromAmount": "5", "toAsset": "ETH", "toAmount": "0.001", "createTime": msAt(2024, 8, 2)},
	}
	return f
}

func (f *fake) check(r *http.Request) bool {
	q := r.URL.RawQuery
	i := strings.LastIndex(q, "&signature=")
	if i < 0 || r.Header.Get("X-MBX-APIKEY") != testKey {
		return false
	}
	m := hmac.New(sha256.New, []byte(testSecret))
	m.Write([]byte(q[:i]))
	vals := r.URL.Query()
	return hex.EncodeToString(m.Sum(nil)) == q[i+len("&signature="):] && vals.Get("timestamp") != "" && vals.Get("recvWindow") != ""
}

func inRange(r *http.Request, at int64, startKey string) bool {
	q := r.URL.Query()
	s, _ := strconv.ParseInt(q.Get(startKey), 10, 64)
	e, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
	return at >= s && at <= e
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	f.calls[path]++
	f.queries[path] = append(f.queries[path], r.URL.RawQuery)
	q := r.URL.Query()
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch path {
	case "/api/v3/time":
		write(map[string]any{"serverTime": now.UnixMilli()})
		return
	case "/api/v3/exchangeInfo":
		syms := []map[string]any{}
		for _, s := range [][2]string{{"BTC", "EUR"}, {"BTC", "USDT"}, {"ETH", "USDT"}, {"ETH", "BTC"}, {"BNB", "USDT"},
			{"LDO", "USDT"}, {"TRX", "USDT"}, {"XYZ", "USDT"}, {"DOGE", "USDT"}, {"SHIB", "DOGE"}} {
			syms = append(syms, map[string]any{"symbol": s[0] + s[1], "status": "TRADING", "baseAsset": s[0], "quoteAsset": s[1]})
		}
		write(map[string]any{"timezone": "UTC", "serverTime": now.UnixMilli(), "symbols": syms})
		return
	}
	if !f.check(r) {
		w.WriteHeader(http.StatusUnauthorized)
		write(map[string]any{"code": -1022, "msg": "Signature for this request is not valid."})
		return
	}
	if f.limited {
		f.limited = false
		w.Header().Set("Retry-After", "0")
		w.WriteHeader(http.StatusTooManyRequests)
		write(map[string]any{"code": -1003, "msg": "Too many requests"})
		return
	}
	switch path {
	case "/sapi/v1/account/apiRestrictions":
		out := map[string]any{"ipRestrict": true, "createTime": 1698645219000}
		for _, k := range []string{"enableReading", "enableWithdrawals", "enableInternalTransfer", "enableMargin", "enableFutures",
			"permitsUniversalTransfer", "enableVanillaOptions", "enableFixApiTrade", "enableFixReadOnly", "enableSpotAndMarginTrading", "enablePortfolioMarginTrading"} {
			out[k] = f.restrict[k]
		}
		write(out)
	case "/sapi/v1/capital/deposit/hisrec":
		f.windowCheck(r, "startTime", 90*24*time.Hour)
		var rows []map[string]any
		for _, d := range f.deposits {
			if inRange(r, d["insertTime"].(int64), "startTime") {
				rows = append(rows, d)
			}
		}
		write(page(rows, q))
	case "/sapi/v1/capital/withdraw/history":
		f.windowCheck(r, "startTime", 90*24*time.Hour)
		var rows []map[string]any
		for _, d := range f.withdrawals {
			at := parseWhen(d["applyTime"].(string)).UnixMilli()
			if inRange(r, at, "startTime") {
				rows = append(rows, d)
			}
		}
		write(page(rows, q))
	case "/sapi/v1/fiat/orders":
		var rows []map[string]any
		if q.Get("transactionType") == "0" && inRange(r, msAt(2023, 12, 1), "beginTime") {
			rows = append(rows, map[string]any{"orderNo": "7d76d611", "fiatCurrency": "EUR", "indicatedAmount": "1000.00", "amount": "1000.00",
				"totalFee": "0.00", "method": "BankAccount", "status": "Successful", "createTime": msAt(2023, 12, 1), "updateTime": msAt(2023, 12, 1)})
			rows = append(rows, map[string]any{"orderNo": "exp", "fiatCurrency": "EUR", "amount": "50", "totalFee": "0",
				"status": "Expired", "createTime": msAt(2023, 12, 1)})
		}
		if q.Get("transactionType") == "1" && inRange(r, msAt(2025, 9, 1), "beginTime") {
			rows = append(rows, map[string]any{"orderNo": "w1", "fiatCurrency": "EUR", "indicatedAmount": "100", "amount": "100",
				"totalFee": "1", "method": "BankAccount", "status": "Finished", "createTime": msAt(2025, 9, 1), "updateTime": msAt(2025, 9, 1)})
		}
		write(map[string]any{"code": "000000", "message": "success", "data": rows, "total": len(rows), "success": true})
	case "/sapi/v1/fiat/payments":
		var rows []map[string]any
		if q.Get("transactionType") == "0" && inRange(r, msAt(2024, 9, 1), "beginTime") {
			rows = append(rows, map[string]any{"orderNo": "353fca443f06466db0c4dc89f94f027a", "sourceAmount": "100.0", "fiatCurrency": "EUR",
				"obtainAmount": "0.04", "cryptoCurrency": "ETH", "totalFee": "2", "price": "2450", "status": "Completed",
				"paymentMethod": "Credit Card", "createTime": msAt(2024, 9, 1), "updateTime": msAt(2024, 9, 1)})
			rows = append(rows, map[string]any{"orderNo": "failed", "sourceAmount": "20.0", "fiatCurrency": "EUR", "obtainAmount": "4.462",
				"cryptoCurrency": "LUNA", "totalFee": "0.2", "price": "4.437472", "status": "Failed", "paymentMethod": "Credit Card",
				"createTime": msAt(2024, 9, 1), "updateTime": msAt(2024, 9, 1)})
		}
		if q.Get("transactionType") == "1" && inRange(r, msAt(2025, 4, 1), "beginTime") {
			rows = append(rows, map[string]any{"orderNo": "sell1", "sourceAmount": "0.01", "fiatCurrency": "EUR", "obtainAmount": "25",
				"cryptoCurrency": "ETH", "totalFee": "0.5", "price": "2500", "status": "Completed", "createTime": msAt(2025, 4, 1), "updateTime": msAt(2025, 4, 1)})
		}
		write(map[string]any{"code": "000000", "message": "success", "data": rows, "total": len(rows), "success": true})
	case "/sapi/v1/convert/tradeFlow":
		f.windowCheck(r, "startTime", 30*24*time.Hour)
		var rows []map[string]any
		for _, c := range f.converts {
			if inRange(r, c["createTime"].(int64), "startTime") {
				rows = append(rows, c)
			}
		}
		s, _ := strconv.ParseInt(q.Get("startTime"), 10, 64)
		e, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
		// Pretend wide ranges with data have more pages, to force splitting.
		more := len(rows) > 0 && time.Duration(e-s)*time.Millisecond > 10*24*time.Hour
		if more {
			rows = rows[:1]
		}
		write(map[string]any{"list": rows, "startTime": s, "endTime": e, "limit": 100, "moreData": more})
	case "/sapi/v1/asset/assetDividend":
		f.windowCheck(r, "startTime", 180*24*time.Hour)
		var rows []map[string]any
		for _, d := range f.dividends {
			if inRange(r, d["divTime"].(int64), "startTime") {
				rows = append(rows, d)
			}
		}
		total := len(rows)
		limit, _ := strconv.Atoi(q.Get("limit"))
		if len(rows) > limit {
			rows = rows[len(rows)-limit:] // latest ones only
		}
		write(map[string]any{"rows": rows, "total": total})
	case "/sapi/v1/asset/dribblet":
		write(map[string]any{"total": 1, "userAssetDribblets": []map[string]any{{
			"operateTime": msAt(2024, 6, 1), "totalTransferedAmount": "0.00100", "totalServiceChargeAmount": "0.00002", "transId": 45178372831,
			"userAssetDribbletDetails": []map[string]any{
				{"transId": 4359321, "serviceChargeAmount": "0.00002", "amount": "9.5", "operateTime": msAt(2024, 6, 1), "transferedAmount": "0.00100", "fromAsset": "TRX"},
			},
		}}})
	case "/api/v3/myTrades":
		sym := q.Get("symbol")
		f.tradeQ[sym] = append(f.tradeQ[sym], q.Get("fromId"))
		if !f.known[sym] {
			w.WriteHeader(http.StatusBadRequest)
			write(map[string]any{"code": -1121, "msg": "Invalid symbol."})
			return
		}
		from, _ := strconv.ParseInt(q.Get("fromId"), 10, 64)
		limit, _ := strconv.Atoi(q.Get("limit"))
		out := []map[string]any{}
		for _, t := range f.trades[sym] {
			if t.ID >= from && len(out) < limit {
				raw, _ := json.Marshal(t)
				var m map[string]any
				_ = json.Unmarshal(raw, &m)
				m["symbol"], m["orderListId"], m["isMaker"], m["isBestMatch"] = sym, -1, false, true
				out = append(out, m)
			}
		}
		write(out)
	case "/api/v3/account":
		if q.Get("omitZeroBalances") != "true" {
			f.t.Errorf("account without omitZeroBalances")
		}
		write(map[string]any{"canTrade": false, "accountType": "SPOT", "balances": []map[string]any{
			{"asset": "BTC", "free": "0.20000000", "locked": "0.00950000"},
			{"asset": "EUR", "free": "323.5", "locked": "0"},
			{"asset": "BNB", "free": "0.0005", "locked": "0"},
			{"asset": "USDT", "free": "4048.72", "locked": "0"},
			{"asset": "SOL", "free": "0.1", "locked": "0"},
			{"asset": "WBETH", "free": "0.048", "locked": "0"},
			{"asset": "BUSD", "free": "15", "locked": "0"},
			{"asset": "LDUSDT", "free": "995.5", "locked": "0"}, // receipt of the flexible position below
			{"asset": "LDXYZ", "free": "100", "locked": "0"},    // receipt without a position: counted as XYZ
			{"asset": "XYZ", "free": "500", "locked": "0"},
			{"asset": "LDO", "free": "3", "locked": "0"}, // a real token, not a receipt
			{"asset": "TRX", "free": "0", "locked": "0"},
		}})
	case "/sapi/v1/asset/get-funding-asset":
		if r.Method != http.MethodPost {
			f.t.Errorf("funding must be POST")
		}
		write([]map[string]any{{"asset": "ETH", "free": "0.03", "locked": "0", "freeze": "0", "withdrawing": "0", "btcValuation": "0.001"}})
	case "/sapi/v1/simple-earn/flexible/position":
		if q.Get("current") == "1" {
			write(map[string]any{"rows": []map[string]any{{"totalAmount": "995.50000000", "asset": "USDT", "productId": "USDT001"}}, "total": 2})
		} else if q.Get("current") == "2" {
			write(map[string]any{"rows": []map[string]any{{"totalAmount": "0", "asset": "DOT", "productId": "DOT001"}}, "total": 2})
		} else {
			write(map[string]any{"rows": []map[string]any{}, "total": 2})
		}
	case "/sapi/v1/lending/auto-invest/history/list":
		f.windowCheck(r, "startTime", 30*24*time.Hour)
		if q.Get("planType") != "ALL" {
			f.t.Errorf("auto-invest planType %q", q.Get("planType"))
		}
		rows := []map[string]any{}
		if inRange(r, msAt(2025, 5, 1), "startTime") {
			rows = append(rows,
				map[string]any{"id": 12345, "targetAsset": "SOL", "planType": "SINGLE", "planName": "SOL", "planId": 1234,
					"transactionDateTime": msAt(2025, 5, 1), "transactionStatus": "SUCCESS", "failedType": "", "sourceAsset": "USDT",
					"sourceAssetAmount": "20", "targetAssetAmount": "0.1", "sourceWallet": "SPOT_WALLET", "flexibleUsed": "false",
					"transactionFee": "0.02", "transactionFeeUnit": "USDT", "executionPrice": "199.8", "executionType": "RECURRING", "subscriptionCycle": "WEEKLY"},
				map[string]any{"id": 12346, "targetAsset": "SOL", "planType": "SINGLE", "planName": "SOL", "planId": 1234,
					"transactionDateTime": msAt(2025, 5, 1) + 1, "transactionStatus": "FAILED", "failedType": "INSUFFICIENT_BALANCE",
					"sourceAsset": "USDT", "sourceAssetAmount": "0", "targetAssetAmount": "0", "transactionFee": "0", "transactionFeeUnit": "USDT"})
		}
		write(map[string]any{"total": len(rows), "list": rows})
	case "/sapi/v1/simple-earn/flexible/history/rewardsRecord":
		f.windowCheck(r, "startTime", 30*24*time.Hour)
		rows := []map[string]any{}
		if inRange(r, msAt(2025, 3, 1), "startTime") {
			rows = append(rows, map[string]any{"asset": "USDT", "rewards": "1.5", "projectId": "USDT001", "type": "REALTIME", "time": msAt(2025, 3, 1)})
		}
		write(map[string]any{"rows": rows, "total": len(rows)})
	case "/sapi/v1/simple-earn/locked/history/rewardsRecord":
		f.windowCheck(r, "startTime", 30*24*time.Hour)
		rows := []map[string]any{}
		if inRange(r, msAt(2025, 3, 2), "startTime") {
			rows = append(rows, map[string]any{"positionId": 123123, "time": msAt(2025, 3, 2), "asset": "ETH", "lockPeriod": "30", "amount": "0.001", "type": "Locked Rewards"})
		}
		write(map[string]any{"rows": rows, "total": len(rows)})
	case "/sapi/v1/eth-staking/eth/history/stakingHistory", "/sapi/v1/eth-staking/eth/history/redemptionHistory",
		"/sapi/v1/eth-staking/wbeth/history/wrapHistory", "/sapi/v1/eth-staking/wbeth/history/unwrapHistory",
		"/sapi/v1/sol-staking/sol/history/stakingHistory", "/sapi/v1/sol-staking/sol/history/redemptionHistory":
		f.windowCheck(r, "startTime", 92*24*time.Hour)
		rows := []map[string]any{}
		if path == "/sapi/v1/eth-staking/eth/history/stakingHistory" && inRange(r, msAt(2024, 7, 1), "startTime") {
			rows = append(rows, map[string]any{"time": msAt(2024, 7, 1), "asset": "ETH", "amount": "0.1", "distributeAsset": "WBETH",
				"distributeAmount": "0.098", "conversionRatio": "1.02", "status": "SUCCESS"},
				map[string]any{"time": msAt(2024, 7, 2), "asset": "ETH", "amount": "5", "distributeAsset": "WBETH",
					"distributeAmount": "4.9", "status": "FAILED"})
		}
		if path == "/sapi/v1/eth-staking/eth/history/redemptionHistory" && inRange(r, msAt(2024, 8, 1), "startTime") {
			rows = append(rows, map[string]any{"time": msAt(2024, 8, 1), "arrivalTime": msAt(2024, 8, 3), "asset": "WBETH", "amount": "0.05",
				"distributeAsset": "ETH", "distributeAmount": "0.051", "conversionRatio": "1.02", "status": "SUCCESS"})
		}
		write(map[string]any{"rows": rows, "total": len(rows)})
	case "/sapi/v1/pay/transactions":
		f.windowCheck(r, "startTime", 90*24*time.Hour)
		if s, _ := strconv.ParseInt(q.Get("startTime"), 10, 64); time.UnixMilli(s).Before(now.Add(-18 * 31 * 24 * time.Hour)) {
			f.t.Errorf("pay asked beyond 18 months")
		}
		rows := []map[string]any{}
		if inRange(r, msAt(2026, 3, 1), "startTime") {
			rows = append(rows,
				map[string]any{"orderType": "C2C", "transactionId": "M_P_71505104267788288", "transactionTime": msAt(2026, 3, 1),
					"amount": "23.72", "currency": "USDT", "walletType": 1, "fundsDetail": []map[string]any{{"currency": "USDT", "amount": "23.72"}}},
				map[string]any{"orderType": "PAY", "transactionId": "P2", "transactionTime": msAt(2026, 3, 2), "amount": "-5", "currency": "USDT"})
		}
		write(map[string]any{"code": "000000", "message": "success", "data": rows, "success": true})
	case "/sapi/v1/c2c/orderMatch/listUserOrderHistory":
		qs, _ := strconv.ParseInt(q.Get("startTimestamp"), 10, 64)
		qe, _ := strconv.ParseInt(q.Get("endTimestamp"), 10, 64)
		if qe <= qs || time.Duration(qe-qs)*time.Millisecond >= 30*24*time.Hour || time.UnixMilli(qs).Before(now.Add(-6*31*24*time.Hour)) {
			f.t.Errorf("c2c range %d..%d", qs, qe)
		}
		rows := []map[string]any{}
		at := msAt(2026, 8, 1)
		if q.Get("tradeType") == "BUY" && at >= qs && at <= qe {
			rows = append(rows, map[string]any{"orderNumber": "20219644646554779648", "advNo": "11218246497340923904", "tradeType": "BUY",
				"asset": "USDT", "fiat": "EUR", "fiatSymbol": "€", "amount": "50", "totalPrice": "46", "unitPrice": "0.92",
				"orderStatus": "COMPLETED", "createTime": at, "commission": "0", "counterPartNickName": "ab***", "payMethodName": "SEPA"},
				map[string]any{"orderNumber": "x", "tradeType": "BUY", "asset": "USDT", "fiat": "EUR", "amount": "10", "totalPrice": "9",
					"orderStatus": "CANCELLED", "createTime": at})
		}
		write(map[string]any{"code": "000000", "message": "success", "data": rows, "total": len(rows), "success": true})
	case "/sapi/v1/simple-earn/locked/position":
		write(map[string]any{"rows": []map[string]any{{"positionId": "123123", "asset": "ETH", "amount": "0.442", "purchaseTime": "1646182276000"}}, "total": 1})
	default:
		w.WriteHeader(http.StatusNotFound)
		write(map[string]any{"code": -1, "msg": "no route " + path})
	}
}

// windowCheck fails the test when a request exceeds the documented range.
func (f *fake) windowCheck(r *http.Request, startKey string, max time.Duration) {
	q := r.URL.Query()
	s, _ := strconv.ParseInt(q.Get(startKey), 10, 64)
	e, _ := strconv.ParseInt(q.Get("endTime"), 10, 64)
	if e <= s || time.Duration(e-s)*time.Millisecond >= max {
		f.t.Errorf("%s: bad range %d..%d", r.URL.Path, s, e)
	}
}

func page(rows []map[string]any, q map[string][]string) []map[string]any {
	off, _ := strconv.Atoi(first(q["offset"]))
	limit, _ := strconv.Atoi(first(q["limit"]))
	if off >= len(rows) {
		return []map[string]any{}
	}
	end := off + limit
	if end > len(rows) {
		end = len(rows)
	}
	return rows[off:end]
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}

func connect(t *testing.T, f *fake, st []byte) (*Binance, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	src, err := New(source.Config{Values: map[string]string{"api_key": testKey, "api_secret": testSecret, "base_url": srv.URL + "/"},
		HTTP: srv.Client(), State: st})
	if err != nil {
		t.Fatal(err)
	}
	b := src.(*Binance)
	b.c.pause = 0
	for path := range b.c.slow {
		b.c.slow[path] = 0
	}
	b.now = func() time.Time { return now }
	return b, srv
}

func collect(t *testing.T, b *Binance, since time.Time) ([]model.Activity, []string) {
	t.Helper()
	var acts []model.Activity
	var logs []string
	err := b.Activities(context.Background(), source.Fetch{
		Since:    since,
		Emit:     func(a model.Activity) error { acts = append(acts, a); return nil },
		Progress: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	})
	if err != nil {
		t.Fatal(err)
	}
	return acts, logs
}

func byNative(acts []model.Activity) map[string]model.Activity {
	m := map[string]model.Activity{}
	for _, a := range acts {
		if _, dup := m[a.NativeID]; dup {
			panic("duplicate native id " + a.NativeID)
		}
		m[a.NativeID] = a
	}
	return m
}

func eq(t *testing.T, what string, got decimal.Decimal, want string) {
	t.Helper()
	if !got.Equal(decimal.RequireFromString(want)) {
		t.Errorf("%s = %s, want %s", what, got, want)
	}
}

func TestRegistered(t *testing.T) {
	d, err := source.Get("binance")
	if err != nil || d.Name != "Binance" || d.Category != "exchange" || d.Setup == "" || len(d.Fields) != 3 || !d.Fields[1].Secret || !d.Fields[2].Optional {
		t.Fatalf("definition %+v %v", d, err)
	}
	if _, err := New(source.Config{Values: map[string]string{"api_key": "k"}}); err == nil {
		t.Fatal("missing secret must fail")
	}
	if _, err := New(source.Config{Values: map[string]string{"api_key": "k", "api_secret": "s"}, State: []byte("{")}); err == nil {
		t.Fatal("bad state must fail")
	}
	src, err := New(source.Config{Values: map[string]string{"api_key": "k", "api_secret": "s"}})
	if err != nil || src.(*Binance).c.base != defaultBase || src.(*Binance).c.http == nil {
		t.Fatal("defaults")
	}
}

func TestFullHistoryReconciles(t *testing.T) {
	f := newFake(t)
	b, _ := connect(t, f, nil)
	acts, logs := collect(t, b, time.Time{})
	m := byNative(acts)

	d := m["deposit:769800519366885376"]
	if d.Type != model.Deposit || d.Network != "BTC" || d.TxID != "f00dbabe" || d.Address != "bc1qxyz" || d.Time.Location() != time.UTC {
		t.Errorf("deposit %+v", d)
	}
	eq(t, "deposit", d.Base.Qty, "0.5")
	if _, ok := m["deposit:3"]; ok {
		t.Error("pending deposit must be skipped")
	}
	if _, ok := m["deposit:2"]; !ok {
		t.Error("status 6 (credited) deposit must be kept")
	}

	w := m["withdrawal:b6ae22b3aa844210a7041aee7589627c"]
	if w.Type != model.Withdrawal || w.Fee == nil || w.Fee.Asset.Symbol != "BTC" || w.TxID != "beefcafe" ||
		!w.Time.Equal(time.Date(2025, 2, 1, 8, 30, 0, 0, time.UTC)) {
		t.Errorf("withdrawal %+v", w)
	}
	eq(t, "withdrawal fee", w.Fee.Qty, "0.0005")
	if _, ok := m["withdrawal:x"]; ok {
		t.Error("rejected withdrawal must be skipped")
	}

	buy := m["trade:BTCEUR:28457"]
	if buy.Type != model.Trade || buy.Side != model.Buy || buy.Base.Asset.Symbol != "BTC" || buy.Quote.Asset.Kind != model.Fiat ||
		buy.Fee == nil || buy.Fee.Asset.Symbol != "BNB" {
		t.Errorf("buy %+v", buy)
	}
	eq(t, "buy price", buy.Price(), "60000")
	sell := m["trade:BTCUSDT:1500"]
	if sell.Side != model.Sell || sell.Fee.Asset.Symbol != "USDT" {
		t.Errorf("sell %+v", sell)
	}
	eq(t, "sell quote from price*qty", sell.Quote.Qty, "6000")

	conv := m["convert:940708407462087195"]
	if conv.Type != model.Trade || conv.Side != model.Buy || conv.Base.Asset.Symbol != "ETH" || conv.Quote.Asset.Symbol != "USDT" {
		t.Errorf("convert %+v", conv)
	}
	if _, ok := m["convert:2"]; ok {
		t.Error("failed convert must be skipped")
	}

	div := m["distribution:242006910"]
	if div.Type != model.Reward || div.Base.Asset.Symbol != "TRX" || div.Note != "TRX airdrop" {
		t.Errorf("dividend %+v", div)
	}
	if neg := m["distribution:242006911"]; neg.Type != model.Fee {
		t.Errorf("negative distribution %+v", neg)
	}
	xyz := 0
	for _, a := range acts {
		if a.Base.Asset.Symbol == "XYZ" {
			xyz++
		}
	}
	if xyz != 600 {
		t.Errorf("XYZ rewards: %d, want 600 (range splitting)", xyz)
	}

	dust := m["dust:45178372831:TRX"]
	if dust.Type != model.Trade || dust.Base.Asset.Symbol != "BNB" || dust.Quote.Asset.Symbol != "TRX" || dust.Fee == nil {
		t.Errorf("dust %+v", dust)
	}
	eq(t, "dust gross", dust.Base.Qty, "0.00102")

	card := m["fiat-payment:353fca443f06466db0c4dc89f94f027a"]
	if card.Side != model.Buy || card.Base.Asset.Symbol != "ETH" || card.Note != "Credit Card" {
		t.Errorf("card %+v", card)
	}
	eq(t, "card quote", card.Quote.Qty, "98")
	if _, ok := m["fiat-payment:353fca443f06466db0c4dc89f94f027a:funding"]; !ok {
		t.Error("card money must be deposited first")
	}
	if _, ok := m["fiat-payment:failed"]; ok {
		t.Error("failed card purchase must be skipped")
	}
	if s := m["fiat-payment:sell1"]; s.Side != model.Sell || s.Quote.Asset.Symbol != "EUR" {
		t.Errorf("fiat sell %+v", s)
	}
	if fd := m["fiat-deposit:7d76d611"]; fd.Type != model.Deposit || fd.Base.Asset != model.CryptoOrFiat("EUR") {
		t.Errorf("fiat deposit %+v", fd)
	}
	if _, ok := m["fiat-deposit:exp"]; ok {
		t.Error("expired fiat order must be skipped")
	}
	if fw := m["fiat-withdrawal:w1"]; fw.Type != model.Withdrawal || fw.Fee == nil {
		t.Errorf("fiat withdrawal %+v", fw)
	}

	// Products beyond spot.
	ai := m["auto-invest:12345"]
	if ai.Type != model.Trade || ai.Side != model.Buy || ai.Base.Asset.Symbol != "SOL" || ai.Fee == nil || ai.Fee.Asset.Symbol != "USDT" {
		t.Errorf("auto-invest %+v", ai)
	}
	eq(t, "auto-invest paid without fee", ai.Quote.Qty, "19.98")
	if _, ok := m["auto-invest:12346"]; ok {
		t.Error("failed auto-invest must be skipped")
	}
	if _, ok := m["distribution:242006912"]; ok {
		t.Error("Simple Earn interest in assetDividend must be skipped (rewards history has it)")
	}
	if e := m[fmt.Sprintf("earn-flexible:USDT001:USDT:REALTIME:%d", msAt(2025, 3, 1))]; e.Type != model.Reward || !e.Base.Qty.Equal(decimal.RequireFromString("1.5")) {
		t.Errorf("flexible reward %+v", e)
	}
	if e := m[fmt.Sprintf("earn-locked:123123:ETH:Locked Rewards:%d", msAt(2025, 3, 2))]; e.Type != model.Reward {
		t.Errorf("locked reward %+v", e)
	}
	st1 := m[fmt.Sprintf("eth-stake:%d:ETH:0.1", msAt(2024, 7, 1))]
	if st1.Base.Asset.Symbol != "WBETH" || st1.Quote.Asset.Symbol != "ETH" {
		t.Errorf("eth staking %+v", st1)
	}
	if red := m[fmt.Sprintf("eth-redeem:%d:WBETH:0.05", msAt(2024, 8, 1))]; red.Base.Asset.Symbol != "ETH" || !red.Time.Equal(time.UnixMilli(msAt(2024, 8, 3)).UTC()) {
		t.Errorf("eth redemption %+v", red)
	}
	if p := m["pay:M_P_71505104267788288"]; p.Type != model.Deposit || p.Base.Asset.Symbol != "USDT" {
		t.Errorf("pay in %+v", p)
	}
	if p := m["pay:P2"]; p.Type != model.Withdrawal || !p.Base.Qty.Equal(decimal.NewFromInt(5)) {
		t.Errorf("pay out %+v", p)
	}
	if p := m["p2p:20219644646554779648"]; p.Side != model.Buy || p.Quote.Asset.Symbol != "EUR" {
		t.Errorf("p2p %+v", p)
	}
	if p := m["p2p:20219644646554779648:fiat"]; p.Type != model.Deposit {
		t.Errorf("p2p fiat %+v", p)
	}
	if _, ok := m["p2p:x"]; ok {
		t.Error("cancelled p2p must be skipped")
	}
	if d := m["trade:ETHBUSD:77"]; d.Side != model.Sell || d.Quote.Asset.Symbol != "BUSD" {
		t.Errorf("delisted pair trade %+v", d)
	}

	// Paging: 1001 BTCUSDT trades in two calls; 1500 deposits need offsets.
	if q := f.tradeQ["BTCUSDT"]; len(q) != 2 || q[0] != "0" || q[1] != "1500" {
		t.Errorf("BTCUSDT fromIds %v", q)
	}
	if _, asked := f.tradeQ["DOGEUSDT"]; asked {
		t.Error("DOGE was never held: DOGEUSDT must not be queried") // only rejected DOGE deposits
	}
	if _, asked := f.tradeQ["SHIBDOGE"]; asked {
		t.Error("SHIBDOGE must not be queried")
	}
	for _, s := range []string{"BTCEUR", "ETHUSDT", "ETHBTC", "BNBUSDT", "LDOUSDT", "TRXUSDT", "XYZUSDT"} {
		if _, asked := f.tradeQ[s]; !asked {
			t.Errorf("%s not checked", s)
		}
	}
	offsets := 0
	for _, raw := range f.queries["/sapi/v1/capital/deposit/hisrec"] {
		if strings.Contains(raw, "offset=1000") {
			offsets++
		}
	}
	if offsets != 1 {
		t.Errorf("deposit offset paging: %d", offsets)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "trading pairs") {
		t.Errorf("progress: %v", logs)
	}

	// The whole history adds up to what Binance reports.
	bal, err := b.Balances(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, x := range bal {
		got[x.Asset.Symbol] = x.Qty.String()
	}
	want := map[string]string{"BTC": "0.2095", "EUR": "323.5", "BNB": "0.0005", "USDT": "5044.22", "ETH": "0.472", "LDO": "3", "XYZ": "600",
		"SOL": "0.1", "WBETH": "0.048", "BUSD": "15"}
	for k, v := range want {
		if !decimal.RequireFromString(got[k]).Equal(decimal.RequireFromString(v)) {
			t.Errorf("balance %s = %s, want %s", k, got[k], v)
		}
	}
	if _, ok := got["TRX"]; ok {
		t.Error("zero balances must be dropped")
	}
	if gaps := model.Reconcile(acts, bal, decimal.Zero); len(gaps) != 0 {
		for _, g := range gaps {
			t.Errorf("gap %s: replayed %s reported %s", g.Asset.Symbol, g.Replayed, g.Reported)
		}
	}

	// State remembers the last trade per pair.
	var st state
	if err := json.Unmarshal(b.State(), &st); err != nil || st.Trades["BTCUSDT"] != 1500 || st.Trades["BTCEUR"] != 28457 {
		t.Fatalf("state %s", b.State())
	}
	if _, ok := st.Trades["ETHUSDT"]; ok {
		t.Error("pairs without trades are not remembered")
	}
	if !st.Dead["SOLBUSD"] || st.Dead["ETHBUSD"] || st.Dead["BTCUSDT"] {
		t.Errorf("unknown pairs are remembered as dead: %v", st.Dead)
	}
}

func TestIncremental(t *testing.T) {
	f := newFake(t)
	st, _ := json.Marshal(state{Trades: map[string]int64{"BTCUSDT": 1500, "SHIBDOGE": 7}, Dead: map[string]bool{"SOLBUSD": true}})
	b, _ := connect(t, f, st)
	since := time.Date(2025, 1, 10, 0, 0, 0, 0, time.UTC)
	acts, _ := collect(t, b, since)
	m := byNative(acts)
	if _, ok := m["deposit:769800519366885376"]; ok {
		t.Error("2018 deposit is before the incremental range")
	}
	if _, ok := m["deposit:2"]; !ok {
		t.Error("deposit within the 7-day overlap must be fetched again")
	}
	if q := f.tradeQ["BTCUSDT"]; len(q) != 1 || q[0] != "1501" {
		t.Errorf("BTCUSDT resumes after the last id: %v", q)
	}
	if _, asked := f.tradeQ["SHIBDOGE"]; !asked {
		t.Error("a pair traded before is always checked")
	}
	if _, asked := f.tradeQ["SOLBUSD"]; asked {
		t.Error("a dead pair is not asked again")
	}
	for _, raw := range f.queries["/sapi/v1/capital/deposit/hisrec"] {
		s, _ := strconv.ParseInt(strings.SplitN(strings.SplitN(raw, "startTime=", 2)[1], "&", 2)[0], 10, 64)
		if time.UnixMilli(s).Before(since.Add(-overlap)) {
			t.Fatalf("query starts too early: %s", raw)
		}
	}
	if _, ok := m["dust:45178372831:TRX"]; ok {
		t.Error("dust before the range is skipped")
	}
}

func TestTestAndErrors(t *testing.T) {
	f := newFake(t)
	b, _ := connect(t, f, nil)
	var slept []time.Duration
	b.c.sleep = func(ctx context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	ctx := context.Background()

	f.limited = true
	if err := b.Test(ctx); err != nil {
		t.Fatalf("read-only key: %v", err)
	}
	found := false
	for _, d := range slept {
		if d == 20*time.Second { // no usable Retry-After: at least 20 s
			found = true
		}
	}
	if !found {
		t.Errorf("429 must be retried after Retry-After: %v", slept)
	}

	f.restrict["enableSpotAndMarginTrading"] = true
	f.restrict["enableWithdrawals"] = true
	if err := b.Test(ctx); err == nil || !strings.Contains(err.Error(), "trading, withdrawals") {
		t.Fatalf("trading key must be refused: %v", err)
	}
	f.restrict = map[string]bool{"enableInternalTransfer": true, "enableReading": true}
	if err := b.Test(ctx); err == nil || !strings.Contains(err.Error(), "transfers") {
		t.Fatalf("transfer key must be refused: %v", err)
	}
	f.restrict = map[string]bool{}
	if err := b.Test(ctx); err == nil || !strings.Contains(err.Error(), "cannot read") {
		t.Fatalf("no reading: %v", err)
	}

	// Wrong secret: signature rejected, reported as a credential problem.
	b.c.secret = []byte("wrong")
	err := b.Test(ctx)
	if err == nil || !strings.Contains(err.Error(), "rejected the API key") || !strings.Contains(err.Error(), "-1022") {
		t.Fatalf("bad key: %v", err)
	}
	if _, err := b.Balances(ctx); err == nil {
		t.Fatal("balances with a bad key must fail")
	}
	if err := b.Activities(ctx, source.Fetch{Emit: func(model.Activity) error { return nil }}); err == nil || !strings.Contains(err.Error(), "deposits") {
		t.Fatalf("activities with a bad key: %v", err)
	}
	if err := b.Activities(ctx, source.Fetch{}); err == nil {
		t.Fatal("no emit")
	}
	if isAuthError(fmt.Errorf("plain")) || !isAuthError(&apiError{Status: 401}) {
		t.Fatal("isAuthError")
	}
	if (&apiError{Status: 500}).Error() != "binance: HTTP 500" {
		t.Fatal("error text")
	}

	// Rate limit that never lifts gives up after a few tries.
	b.c.secret = []byte(testSecret)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "2")
		w.WriteHeader(http.StatusTeapot)
	}))
	defer srv.Close()
	b.c.base = srv.URL
	slept = nil
	var waits []time.Duration
	b.c.onWait = func(_ string, d time.Duration) { waits = append(waits, d) }
	if err := b.c.get(ctx, "/x", nil, nil); err == nil {
		t.Fatal("418 forever must fail")
	}
	// Growing waits, at least the server's Retry-After, then give up.
	want := []time.Duration{20 * time.Second, 40 * time.Second, time.Minute, 80 * time.Second, 100 * time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("418 retries: %v", waits)
	}
	b.c.onWait = nil
	// Non-JSON error bodies and undecodable answers.
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/bad" {
			w.WriteHeader(http.StatusBadGateway)
			w.Write([]byte("<html>gateway</html>"))
			return
		}
		w.Write([]byte("not json"))
	}))
	defer srv2.Close()
	b.c.base = srv2.URL
	if err := b.c.get(ctx, "/bad", nil, nil); err == nil || !strings.Contains(err.Error(), "gateway") {
		t.Fatalf("html error: %v", err)
	}
	var v struct{}
	if err := b.c.get(ctx, "/ok", nil, &v); err == nil || !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("decode: %v", err)
	}
	// Emit errors stop the run.
	b2, _ := connect(t, newFake(t), nil)
	stop := fmt.Errorf("stop")
	if err := b2.Activities(ctx, source.Fetch{Emit: func(model.Activity) error { return stop }}); err == nil {
		t.Fatal("emit error must propagate")
	}
}

func TestHelpers(t *testing.T) {
	if w := windows(now, now.Add(100*24*time.Hour), capitalWindow); len(w) != 2 || !w[1][1].Equal(now.Add(100*24*time.Hour)) ||
		!w[1][0].Equal(w[0][1].Add(time.Millisecond)) {
		t.Fatalf("windows %v", w)
	}
	if !parseWhen("bad").IsZero() || !dec("x").IsZero() || feeOf("BTC", "0") != nil || feeOf("", "1") != nil {
		t.Fatal("helpers")
	}
	known := map[string]bool{"LDO": true}
	if earnAsset("LDO", known) != "LDO" || earnAsset("LDBTC", known) != "BTC" || earnAsset("LD", known) != "LD" {
		t.Fatal("earnAsset")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if sleepCtx(ctx, time.Hour) == nil || sleepCtx(ctx, 0) == nil {
		t.Fatal("cancelled sleep")
	}
	if sleepCtx(context.Background(), time.Millisecond) != nil {
		t.Fatal("sleep")
	}
}

// Fiat history: one wide range first, windows if Binance refuses it; a
// failing section does not stop the others; costly paths are spaced.
func TestFiatRangeFallbackAndPartialFailure(t *testing.T) {
	var ranges []string
	var fiatCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		switch r.URL.Path {
		case "/sapi/v1/fiat/orders":
			fiatCalls++
			ranges = append(ranges, q.Get("beginTime")+"-"+q.Get("endTime"))
			if len(ranges) == 1 {
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"code":-1127,"msg":"More than 90 days"}`)
				return
			}
			fmt.Fprint(w, `{"code":"000000","message":"success","data":[],"total":0,"success":true}`)
		case "/sapi/v1/capital/deposit/hisrec":
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprint(w, `{"code":-1000,"msg":"boom"}`)
		case "/api/v3/time":
			fmt.Fprintf(w, `{"serverTime":%d}`, time.Now().UnixMilli())
		default:
			if strings.HasPrefix(r.URL.Path, "/sapi/v1/fiat/payments") {
				fmt.Fprint(w, `{"code":"000000","message":"success","data":[],"total":0,"success":true}`)
				return
			}
			fmt.Fprint(w, `[]`)
		}
	}))
	defer srv.Close()
	def, _ := source.Get("binance")
	s, err := def.New(source.Config{Values: map[string]string{"api_key": "k", "api_secret": testSecret, "base_url": srv.URL}, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	b := s.(*Binance)
	b.c.pause = 0
	var slept []time.Duration
	b.c.sleep = func(ctx context.Context, d time.Duration) error {
		if d > 0 {
			slept = append(slept, d)
		}
		return nil
	}
	b.now = func() time.Time { return time.Date(2018, 3, 1, 0, 0, 0, 0, time.UTC) }
	var logs []string
	err = b.Activities(context.Background(), source.Fetch{
		Since:    time.Date(2018, 1, 1, 0, 0, 0, 0, time.UTC),
		Emit:     func(model.Activity) error { return nil },
		Progress: func(f string, a ...any) { logs = append(logs, fmt.Sprintf(f, a...)) },
	})
	if err == nil || !strings.Contains(err.Error(), "deposits") || strings.Contains(err.Error(), "fiat") {
		t.Fatalf("only deposits fail: %v", err)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "spot trades") {
		t.Fatal("later sections still run")
	}
	// First call: the whole range (refused), then windows; each costly
	// call waits for its spacing.
	if fiatCalls < 3 || len(slept) == 0 {
		t.Fatalf("fiat calls %d, waits %v", fiatCalls, slept)
	}
	for _, d := range slept {
		if d > 16*time.Second {
			t.Fatalf("spacing %v", d)
		}
	}
}

func TestDecodePage(t *testing.T) {
	for _, c := range []struct {
		raw   string
		n     int
		total int
	}{
		{`[{"a":1},{"a":2}]`, 2, -1},
		{`{"total":3,"list":[{}]}`, 1, 3},
		{`{"rows":[{},{}],"total":2}`, 2, 2},
		{`{"code":"000000","data":[{}]}`, 1, -1},
		{`null`, 0, 0},
		{``, 0, 0},
	} {
		rows, total, err := decodePage(json.RawMessage(c.raw))
		if err != nil || len(rows) != c.n || total != c.total {
			t.Errorf("%s: %d rows, total %d, %v", c.raw, len(rows), total, err)
		}
	}
	if _, _, err := decodePage(json.RawMessage(`{"rows":5}`)); err == nil {
		t.Error("bad page must fail")
	}
	if !ok("") || !ok("success") || ok("FAILED") {
		t.Error("ok")
	}
}
