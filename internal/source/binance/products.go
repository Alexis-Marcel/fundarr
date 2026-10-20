package binance

// Products beyond spot that move balances: Auto-Invest, Simple Earn rewards,
// ETH/SOL liquid staking, Binance Pay and P2P (C2C). Each walks its
// documented time ranges and pages; ranges and page sizes are from the
// official docs and connectors (binance-connector-js, binance-api-swagger).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

// Slow paths: these cost 150 IP weight (6000 a minute) or 3000 UID weight
// a call; spacing them keeps a first sync from being banned.
var productSlow = map[string]time.Duration{
	"/sapi/v1/simple-earn/flexible/history/rewardsRecord": 2 * time.Second,
	"/sapi/v1/simple-earn/locked/history/rewardsRecord":   2 * time.Second,
	"/sapi/v1/eth-staking/eth/history/stakingHistory":     2 * time.Second,
	"/sapi/v1/eth-staking/eth/history/redemptionHistory":  2 * time.Second,
	"/sapi/v1/eth-staking/wbeth/history/wrapHistory":      2 * time.Second,
	"/sapi/v1/eth-staking/wbeth/history/unwrapHistory":    2 * time.Second,
	"/sapi/v1/sol-staking/sol/history/stakingHistory":     2 * time.Second,
	"/sapi/v1/sol-staking/sol/history/redemptionHistory":  2 * time.Second,
	"/sapi/v1/pay/transactions":                           2 * time.Second,
}

// When each product started: no need to ask about earlier dates.
var (
	autoInvestStart = time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	simpleEarnStart = time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
	ethStakingStart = time.Date(2020, 12, 1, 0, 0, 0, 0, time.UTC)
	solStakingStart = time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
)

// Documented retention: only recent history can be asked for.
const (
	payRetention = 18 * 30 * 24 * time.Hour // Pay: last 18 months
	c2cRetention = 6 * 30 * 24 * time.Hour  // P2P: last 6 months
)

// clamp returns the part of the run's range after start (and after
// now-retention when retention is set).
func (r *run) clamp(start time.Time, retention time.Duration) (time.Time, time.Time) {
	from := r.from
	if from.Before(start) {
		from = start
	}
	if retention > 0 {
		if oldest := r.to.Add(-retention + 24*time.Hour); from.Before(oldest) {
			from = oldest
		}
	}
	return from, r.to
}

// decodePage reads the rows of a history page, whatever the wrapper:
// a bare array, or an object with rows/list/data and a total.
func decodePage(raw json.RawMessage) ([]json.RawMessage, int, error) {
	raw = json.RawMessage(strings.TrimSpace(string(raw)))
	if len(raw) == 0 || string(raw) == "null" {
		return nil, 0, nil
	}
	if raw[0] == '[' {
		var rows []json.RawMessage
		err := json.Unmarshal(raw, &rows)
		return rows, -1, err
	}
	var obj struct {
		Rows  []json.RawMessage `json:"rows"`
		List  []json.RawMessage `json:"list"`
		Data  []json.RawMessage `json:"data"`
		Total *int              `json:"total"`
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		return nil, 0, err
	}
	rows := obj.Rows
	if rows == nil {
		rows = obj.List
	}
	if rows == nil {
		rows = obj.Data
	}
	total := -1
	if obj.Total != nil {
		total = *obj.Total
	}
	return rows, total, nil
}

// pager describes how a history endpoint is walked.
type pager struct {
	path             string
	params           url.Values
	startKey, endKey string        // startTime/endTime by default
	window           time.Duration // documented max range
	pageKey, sizeKey string        // current/size by default
	size             int
}

// walk calls each row of every page of every window between from and to.
func (r *run) walk(ctx context.Context, p pager, from, to time.Time, each func(json.RawMessage) error) error {
	if p.startKey == "" {
		p.startKey, p.endKey = "startTime", "endTime"
	}
	if p.pageKey == "" {
		p.pageKey, p.sizeKey = "current", "size"
	}
	if p.size == 0 {
		p.size = 100
	}
	for _, w := range windows(from, to, p.window) {
		for page, seen := 1, 0; ; page++ {
			q := url.Values{p.startKey: {ms(w[0])}, p.endKey: {ms(w[1])},
				p.pageKey: {strconv.Itoa(page)}, p.sizeKey: {strconv.Itoa(p.size)}}
			for k, v := range p.params {
				q[k] = v
			}
			var raw json.RawMessage
			if err := r.b.c.get(ctx, p.path, q, &raw); err != nil {
				return err
			}
			rows, total, err := decodePage(raw)
			if err != nil {
				return fmt.Errorf("%s: %w", p.path, err)
			}
			for _, row := range rows {
				if err := each(row); err != nil {
					return err
				}
			}
			seen += len(rows)
			if len(rows) < p.size || (total >= 0 && seen >= total) {
				break
			}
		}
	}
	return nil
}

func ok(status string) bool {
	s := strings.ToUpper(strings.TrimSpace(status))
	return s == "" || s == "SUCCESS" || s == "SUCCESSFUL" || s == "COMPLETED" || s == "FINISHED"
}

// ---- Auto-Invest

// autoInvest: recurring buys, index-linked plans, portfolio plans and
// one-time buys. Each executed subscription is a purchase of targetAsset
// paid with sourceAsset; the fee is taken out of the source amount.
func (r *run) autoInvest(ctx context.Context) error {
	from, to := r.clamp(autoInvestStart, 0)
	// The docs list planType=ALL, but the live API rejects it as malformed
	// (-1102): ask for each plan type.
	for _, plan := range []string{"SINGLE", "PORTFOLIO", "INDEX"} {
		p := pager{path: "/sapi/v1/lending/auto-invest/history/list", params: url.Values{"planType": {plan}}, window: 29 * 24 * time.Hour}
		if err := r.walk(ctx, p, from, to, r.autoInvestRow); err != nil {
			return fmt.Errorf("%s plans: %w", strings.ToLower(plan), err)
		}
	}
	return nil
}

func (r *run) autoInvestRow(raw json.RawMessage) error {
	{
		var x struct {
			ID                  int64  `json:"id"`
			TargetAsset         string `json:"targetAsset"`
			PlanType            string `json:"planType"`
			PlanName            string `json:"planName"`
			PlanID              int64  `json:"planId"`
			TransactionDateTime int64  `json:"transactionDateTime"`
			TransactionStatus   string `json:"transactionStatus"`
			SourceAsset         string `json:"sourceAsset"`
			SourceAssetAmount   string `json:"sourceAssetAmount"`
			TargetAssetAmount   string `json:"targetAssetAmount"`
			TransactionFee      string `json:"transactionFee"`
			TransactionFeeUnit  string `json:"transactionFeeUnit"`
			ExecutionType       string `json:"executionType"`
		}
		if err := json.Unmarshal(raw, &x); err != nil {
			return err
		}
		if !strings.EqualFold(x.TransactionStatus, "SUCCESS") || dec(x.TargetAssetAmount).IsZero() {
			return nil
		}
		paid := amt(x.SourceAsset, x.SourceAssetAmount)
		fee := feeOf(x.TransactionFeeUnit, x.TransactionFee)
		if fee != nil && fee.Asset.Symbol == paid.Asset.Symbol {
			paid.Qty = paid.Qty.Sub(fee.Qty)
		}
		note := strings.TrimSpace("Auto-Invest " + x.PlanName)
		return r.emit(model.Activity{
			NativeID: "auto-invest:" + strconv.FormatInt(x.ID, 10),
			Time:     time.UnixMilli(x.TransactionDateTime),
			Type:     model.Trade,
			Side:     model.Buy,
			Base:     amt(x.TargetAsset, x.TargetAssetAmount),
			Quote:    ptr(paid),
			Fee:      fee,
			Note:     note,
		})
	}
}

// ---- Simple Earn rewards

// earnRewards: interest and bonuses of Simple Earn flexible and locked
// products (they are not in assetDividend; distributions there that say
// "Simple Earn" are skipped to never count them twice).
func (r *run) earnRewards(ctx context.Context) error {
	from, to := r.clamp(simpleEarnStart, 0)
	window := 29 * 24 * time.Hour // documented: at most 30 days
	// type is mandatory on the live API: real-time APR interest and tiered
	// bonus are separate records.
	for _, typ := range []string{"REALTIME", "BONUS"} {
		err := r.walk(ctx, pager{path: "/sapi/v1/simple-earn/flexible/history/rewardsRecord", params: url.Values{"type": {typ}}, window: window}, from, to, func(raw json.RawMessage) error {
			var x struct {
				Asset     string `json:"asset"`
				Rewards   string `json:"rewards"`
				ProjectID string `json:"projectId"`
				Type      string `json:"type"`
				Time      int64  `json:"time"`
			}
			if err := json.Unmarshal(raw, &x); err != nil {
				return err
			}
			if dec(x.Rewards).Sign() <= 0 {
				return nil
			}
			return r.emit(model.Activity{
				NativeID: fmt.Sprintf("earn-flexible:%s:%s:%s:%d", x.ProjectID, x.Asset, x.Type, x.Time),
				Time:     time.UnixMilli(x.Time),
				Type:     model.Reward,
				Base:     amt(x.Asset, x.Rewards),
				Note:     strings.TrimSpace("Simple Earn flexible " + strings.ToLower(x.Type)),
			})
		})
		if err != nil {
			return fmt.Errorf("flexible %s: %w", strings.ToLower(typ), err)
		}
	}
	return r.walk(ctx, pager{path: "/sapi/v1/simple-earn/locked/history/rewardsRecord", window: window}, from, to, func(raw json.RawMessage) error {
		var x struct {
			PositionID json.Number `json:"positionId"`
			Time       int64       `json:"time"`
			Asset      string      `json:"asset"`
			Amount     string      `json:"amount"`
			Type       string      `json:"type"`
		}
		if err := json.Unmarshal(raw, &x); err != nil {
			return err
		}
		if dec(x.Amount).Sign() <= 0 {
			return nil
		}
		return r.emit(model.Activity{
			NativeID: fmt.Sprintf("earn-locked:%s:%s:%s:%d", x.PositionID, x.Asset, x.Type, x.Time),
			Time:     time.UnixMilli(x.Time),
			Type:     model.Reward,
			Base:     amt(x.Asset, x.Amount),
			Note:     "Simple Earn locked",
		})
	})
}

// ---- liquid staking (ETH → WBETH/BETH, SOL → BNSOL)

type stakeRow struct {
	Time             int64  `json:"time"`
	ArrivalTime      int64  `json:"arrivalTime"`
	Asset            string `json:"asset"`
	Amount           string `json:"amount"`
	DistributeAsset  string `json:"distributeAsset"`
	DistributeAmount string `json:"distributeAmount"`
	FromAsset        string `json:"fromAsset"`
	FromAmount       string `json:"fromAmount"`
	ToAsset          string `json:"toAsset"`
	ToAmount         string `json:"toAmount"`
	Status           string `json:"status"`
}

// conversion turns a staking row into "gave X, got Y". Staking and
// redemption rows give asset/amount and receive distributeAsset/
// distributeAmount; wrap and unwrap rows say from/to.
func (s stakeRow) conversion() (model.Amount, model.Amount) {
	if s.FromAsset != "" {
		return amt(s.FromAsset, s.FromAmount), amt(s.ToAsset, s.ToAmount)
	}
	return amt(s.Asset, s.Amount), amt(s.DistributeAsset, s.DistributeAmount)
}

func (r *run) staking(ctx context.Context) error {
	window := 89 * 24 * time.Hour // documented: at most 3 months
	type product struct {
		path, kind string
		start      time.Time
		redeem     bool
	}
	for _, p := range []product{
		{"/sapi/v1/eth-staking/eth/history/stakingHistory", "eth-stake", ethStakingStart, false},
		{"/sapi/v1/eth-staking/eth/history/redemptionHistory", "eth-redeem", ethStakingStart, true},
		{"/sapi/v1/eth-staking/wbeth/history/wrapHistory", "wbeth-wrap", ethStakingStart, false},
		{"/sapi/v1/eth-staking/wbeth/history/unwrapHistory", "wbeth-unwrap", ethStakingStart, false},
		{"/sapi/v1/sol-staking/sol/history/stakingHistory", "sol-stake", solStakingStart, false},
		{"/sapi/v1/sol-staking/sol/history/redemptionHistory", "sol-redeem", solStakingStart, true},
	} {
		from, to := r.clamp(p.start, 0)
		err := r.walk(ctx, pager{path: p.path, window: window}, from, to, func(raw json.RawMessage) error {
			var s stakeRow
			if err := json.Unmarshal(raw, &s); err != nil {
				return err
			}
			if !ok(s.Status) {
				return nil
			}
			gave, got := s.conversion()
			if gave.Asset.Symbol == "" || got.Asset.Symbol == "" || got.Qty.IsZero() {
				return nil
			}
			when := s.Time
			if p.redeem && s.ArrivalTime > 0 {
				when = s.ArrivalTime
			}
			return r.emit(model.Activity{
				NativeID: fmt.Sprintf("%s:%d:%s:%s", p.kind, s.Time, gave.Asset.Symbol, gave.Qty.String()),
				Time:     time.UnixMilli(when),
				Type:     model.Trade,
				Side:     model.Buy,
				Base:     got,
				Quote:    &gave,
				Note:     "Staking " + p.kind,
			})
		})
		if err != nil {
			return fmt.Errorf("%s: %w", p.kind, err)
		}
	}
	return nil
}

// ---- Binance Pay

// pay: money sent to or received from other Binance users (Pay, gifts,
// crypto boxes, payouts). A negative amount left the account. Only the last
// 18 months can be asked for; a range returns its latest `limit` rows, so
// full ranges are split.
func (r *run) pay(ctx context.Context) error {
	from, to := r.clamp(genesis, payRetention)
	var walk func(from, to time.Time) error
	walk = func(from, to time.Time) error {
		var res struct {
			Code    string `json:"code"`
			Message string `json:"message"`
			Data    []struct {
				OrderType       string `json:"orderType"`
				TransactionID   string `json:"transactionId"`
				TransactionTime int64  `json:"transactionTime"`
				Amount          string `json:"amount"`
				Currency        string `json:"currency"`
				FundsDetail     []struct {
					Currency string `json:"currency"`
					Amount   string `json:"amount"`
				} `json:"fundsDetail"`
			} `json:"data"`
			Success *bool `json:"success"`
		}
		q := url.Values{"startTime": {ms(from)}, "endTime": {ms(to)}, "limit": {"100"}}
		if err := r.b.c.get(ctx, "/sapi/v1/pay/transactions", q, &res); err != nil {
			return err
		}
		if res.Success != nil && !*res.Success {
			return errors.New("pay: " + res.Message)
		}
		if len(res.Data) >= 100 && to.Sub(from) > time.Hour {
			end, next := halves(from, to)
			if err := walk(from, end); err != nil {
				return err
			}
			return walk(next, to)
		}
		for _, t := range res.Data {
			// One payment can be funded in several assets: fundsDetail
			// says what actually moved; otherwise amount/currency.
			moves := []struct{ cur, qty string }{{t.Currency, t.Amount}}
			if len(t.FundsDetail) > 0 {
				moves = moves[:0]
				for _, fd := range t.FundsDetail {
					moves = append(moves, struct{ cur, qty string }{fd.Currency, fd.Amount})
				}
			}
			out := dec(t.Amount).IsNegative()
			for i, m := range moves {
				a := amt(m.cur, m.qty)
				if a.Qty.IsZero() {
					continue
				}
				typ := model.Deposit
				if out || a.Qty.IsNegative() {
					typ = model.Withdrawal
				}
				a.Qty = a.Qty.Abs()
				id := "pay:" + t.TransactionID
				if len(moves) > 1 {
					id += ":" + strconv.Itoa(i)
				}
				if err := r.emit(model.Activity{
					NativeID: id,
					Time:     time.UnixMilli(t.TransactionTime),
					Type:     typ,
					Base:     a,
					Note:     "Binance Pay " + strings.ToLower(t.OrderType),
				}); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, w := range windows(from, to, 89*24*time.Hour) { // documented: at most 90 days
		if err := walk(w[0], w[1]); err != nil {
			return err
		}
	}
	return nil
}

// ---- P2P (C2C)

// p2p: crypto bought or sold with other users for fiat paid outside
// Binance. A buy is fiat that came in (paid by bank) then a trade; a sell
// is a trade then fiat that left. Only the last 6 months are available.
func (r *run) p2p(ctx context.Context) error {
	from, to := r.clamp(genesis, c2cRetention)
	for _, side := range []string{"BUY", "SELL"} {
		p := pager{path: "/sapi/v1/c2c/orderMatch/listUserOrderHistory", params: url.Values{"tradeType": {side}},
			startKey: "startTimestamp", endKey: "endTimestamp", window: 29 * 24 * time.Hour, pageKey: "page", sizeKey: "rows"}
		err := r.walk(ctx, p, from, to, func(raw json.RawMessage) error {
			var o struct {
				OrderNumber   string `json:"orderNumber"`
				TradeType     string `json:"tradeType"`
				Asset         string `json:"asset"`
				Fiat          string `json:"fiat"`
				Amount        string `json:"amount"`
				TotalPrice    string `json:"totalPrice"`
				OrderStatus   string `json:"orderStatus"`
				CreateTime    int64  `json:"createTime"`
				Commission    string `json:"commission"`
				PayMethodName string `json:"payMethodName"`
			}
			if err := json.Unmarshal(raw, &o); err != nil {
				return err
			}
			if !strings.EqualFold(o.OrderStatus, "COMPLETED") {
				return nil
			}
			when := time.UnixMilli(o.CreateTime)
			fiat := amt(o.Fiat, o.TotalPrice)
			trade := model.Activity{
				NativeID: "p2p:" + o.OrderNumber,
				Time:     when,
				Type:     model.Trade,
				Base:     amt(o.Asset, o.Amount),
				Quote:    ptr(fiat),
				Fee:      feeOf(o.Asset, o.Commission),
				Note:     strings.TrimSpace("P2P " + o.PayMethodName),
			}
			money := model.Activity{NativeID: "p2p:" + o.OrderNumber + ":fiat", Time: when, Base: fiat, Note: trade.Note}
			if side == "BUY" {
				trade.Side, money.Type = model.Buy, model.Deposit
				if err := r.emit(money); err != nil {
					return err
				}
				return r.emit(trade)
			}
			trade.Side, money.Type = model.Sell, model.Withdrawal
			if err := r.emit(trade); err != nil {
				return err
			}
			return r.emit(money)
		})
		if err != nil {
			return err
		}
	}
	return nil
}
