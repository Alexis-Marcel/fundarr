// Package ibkr reads an Interactive Brokers account through the Flex Web
// Service: IBKR's official, read-only reporting API.
//
// The user builds an Activity Flex Query in the Client Portal (sections
// Trades, Cash Transactions, Open Positions, Cash Report, Corporate Actions
// and Transfers, in XML) and creates a Flex Web Service token. A report is
// fetched in two steps: SendRequest (token, query id, optional dates) gives
// a reference code, then GetStatement returns the XML once IBKR has
// generated it (error 1019 until then). See flex.go for the mapping.
//
// Not read: options, futures, CFDs and other derivatives (skipped and
// logged), and every account but the first when a query covers several.
package ibkr

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

const defaultBase = "https://ndcdyn.interactivebrokers.com/AccountManagement/FlexWebService"

// flexVersion is the Flex Web Service version IBKR asks clients to use.
const flexVersion = "3"

func init() {
	source.Register(source.Definition{
		Kind:        "ibkr",
		Name:        "Interactive Brokers",
		Category:    "broker",
		Description: "Stocks, ETFs and bonds, through the Flex Web Service.",
		Fields: []source.Field{
			{Name: "token", Label: "Flex Web Service token", Secret: true},
			{Name: "query_id", Label: "Activity Flex Query ID"},
			{Name: "history_from", Label: "Account opened on", Optional: true, Advanced: true,
				Help: "YYYY-MM-DD. Leave empty: the first sync reads back a year at a time until it finds two empty years."},
			{Name: "base_url", Label: "API URL", Optional: true, Advanced: true, Help: "leave empty"},
		},
		Setup: "In the IBKR Client Portal, open Performance & Reports → Flex Queries. " +
			"Create an Activity Flex Query in XML with the sections Trades (Executions), Cash Transactions, " +
			"Open Positions, Cash Report, Corporate Actions and Transfers, all fields, and note its Query ID. " +
			"Then, under Flex Web Service Configuration, enable the service and generate a token. " +
			"The token only reads reports: it cannot trade or move money.",
		Limits: "IBKR's Flex reports skip options, futures and other derivatives here, and give at most 365 days per request.",
		New:    New,
	})
}

// New builds the connector from its settings.
func New(cfg source.Config) (source.Source, error) {
	token := strings.TrimSpace(cfg.Get("token"))
	query := strings.TrimSpace(cfg.Get("query_id"))
	if token == "" || query == "" {
		return nil, errors.New("ibkr: the Flex token and the query ID are required")
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.Get("base_url")), "/")
	if base == "" {
		base = defaultBase
	}
	s := &Source{c: &client{base: base, token: token, query: query, http: cfg.HTTP, now: time.Now, sleep: sleepCtx}}
	if s.c.http == nil {
		s.c.http = source.NewHTTP()
	}
	if v := strings.TrimSpace(cfg.Get("history_from")); v != "" {
		t, err := time.Parse("2006-01-02", v)
		if err != nil {
			return nil, errors.New("ibkr: \"Account opened on\" must be a date like 2019-03-01")
		}
		s.from = t
	}
	return s, nil
}

// Source is a configured Interactive Brokers connector.
type Source struct {
	c    *client
	from time.Time // when the account was opened, if the user said

	// latest is the newest statement read this run (it ends yesterday): its
	// positions and cash are the balances, read without another request.
	latest   *statement
	latestAt time.Time
}

// State implements source.Source: the engine's last sync time is enough.
func (s *Source) State() []byte { return nil }

// ---- HTTP client

type client struct {
	base, token, query string
	http               *http.Client
	now                func() time.Time
	sleep              func(context.Context, time.Duration) error

	mu       sync.Mutex
	lastSend time.Time // pacing of SendRequest
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// APIError is a failure IBKR reports in a FlexStatementResponse.
type APIError struct {
	Step    string // SendRequest or GetStatement
	Code    string
	Message string
	Msg     string // what to do about it, when known
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("ibkr %s: error %s: %s", e.Step, e.Code, e.Message)
	if e.Msg != "" {
		s = fmt.Sprintf("ibkr %s: %s (error %s: %s)", e.Step, e.Msg, e.Code, e.Message)
	}
	return s
}

// ErrToken and ErrQuery tell a bad token from a bad query id.
var (
	ErrToken = errors.New("the Flex Web Service token is wrong or expired")
	ErrQuery = errors.New("the Activity Flex Query ID is wrong")
)

func (e *APIError) Unwrap() error {
	switch e.Code {
	case "1012", "1015":
		return ErrToken
	case "1014":
		return ErrQuery
	}
	return nil
}

// Codes IBKR documents as temporary ("Please try again shortly").
var transient = map[string]bool{
	"1001": true, "1004": true, "1005": true, "1006": true, "1007": true, "1008": true,
	"1009": true, "1018": true, "1019": true, "1021": true,
}

// What to tell the user for the codes they can act on.
var advice = map[string]string{
	"1010": "this is a legacy Flex Query: create an Activity Flex Query",
	"1011": "the Flex Web Service is not active for this account",
	"1012": "the Flex token has expired: generate a new one in the Client Portal",
	"1013": "the token is restricted to other IP addresses",
	"1014": "unknown Activity Flex Query ID: check it in Performance & Reports → Flex Queries",
	"1015": "the Flex token is invalid: check it was copied whole",
	"1016": "the account is invalid",
	"1025": "too many failed attempts: IBKR blocked the token for a while",
}

// response is what both endpoints answer when they do not return a report.
type response struct {
	XMLName       xml.Name `xml:"FlexStatementResponse"`
	Status        string   `xml:"Status"`
	ReferenceCode string   `xml:"ReferenceCode"`
	ErrorCode     string   `xml:"ErrorCode"`
	ErrorMessage  string   `xml:"ErrorMessage"`
}

// get calls one endpoint and returns its body. IBKR requires a User-Agent.
func (c *client) get(ctx context.Context, step string, q url.Values) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+"/"+step+"?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Fundarr")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		// The token is in the URL: never let it reach an error message.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return nil, fmt.Errorf("ibkr %s: %w", step, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256<<20))
	if err != nil {
		return nil, fmt.Errorf("ibkr %s: %w", step, err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, &APIError{Step: step, Code: fmt.Sprintf("HTTP %d", resp.StatusCode), Message: snippet(body)}
	}
	return body, nil
}

// failure reads a FlexStatementResponse: nil when body is something else
// (a report).
func failure(step string, body []byte) (*response, *APIError) {
	if !strings.Contains(string(body[:min(len(body), 512)]), "<FlexStatementResponse") {
		return nil, nil
	}
	var r response
	if err := xml.Unmarshal(body, &r); err != nil {
		return nil, &APIError{Step: step, Code: "?", Message: "unreadable answer: " + snippet(body)}
	}
	if strings.EqualFold(r.Status, "Success") {
		return &r, nil
	}
	return &r, &APIError{Step: step, Code: r.ErrorCode, Message: r.ErrorMessage, Msg: advice[r.ErrorCode]}
}

// sendGap paces SendRequest: IBKR allows 1 request per second and 10 per
// minute per token.
const sendGap = 6 * time.Second

// send asks IBKR to generate the query's report, for [from, to] when set,
// and returns the reference code.
func (c *client) send(ctx context.Context, from, to time.Time) (string, error) {
	q := url.Values{"t": {c.token}, "q": {c.query}, "v": {flexVersion}}
	if !from.IsZero() {
		q.Set("fd", from.Format("20060102"))
		q.Set("td", to.Format("20060102"))
	}
	backoff := 10 * time.Second
	for attempt := 0; ; attempt++ {
		c.mu.Lock()
		wait := time.Duration(0)
		if !c.lastSend.IsZero() {
			wait = sendGap - c.now().Sub(c.lastSend)
		}
		c.mu.Unlock()
		if wait > 0 {
			if err := c.sleep(ctx, wait); err != nil {
				return "", err
			}
		}
		c.mu.Lock()
		c.lastSend = c.now()
		c.mu.Unlock()
		body, err := c.get(ctx, "SendRequest", q)
		if err != nil {
			return "", err
		}
		r, ae := failure("SendRequest", body)
		if ae != nil {
			if transient[ae.Code] && attempt < 4 {
				if err := c.sleep(ctx, backoff); err != nil {
					return "", err
				}
				backoff *= 2
				continue
			}
			return "", ae
		}
		if r == nil || strings.TrimSpace(r.ReferenceCode) == "" {
			return "", &APIError{Step: "SendRequest", Code: "?", Message: "no reference code: " + snippet(body)}
		}
		return strings.TrimSpace(r.ReferenceCode), nil
	}
}

// How long to wait for a report: the first look after a few seconds, then
// less and less often, about five minutes in all.
var pollWaits = []time.Duration{3 * time.Second, 5 * time.Second, 10 * time.Second, 15 * time.Second,
	30 * time.Second, 30 * time.Second, 60 * time.Second, 60 * time.Second, 60 * time.Second}

// fetch returns the report a reference code names, once generated.
func (c *client) fetch(ctx context.Context, ref string) ([]byte, error) {
	q := url.Values{"t": {c.token}, "q": {ref}, "v": {flexVersion}}
	for i := 0; ; i++ {
		if i < len(pollWaits) {
			if err := c.sleep(ctx, pollWaits[i]); err != nil {
				return nil, err
			}
		}
		body, err := c.get(ctx, "GetStatement", q)
		if err != nil {
			return nil, err
		}
		_, ae := failure("GetStatement", body)
		if ae == nil {
			return body, nil
		}
		if !transient[ae.Code] || i >= len(pollWaits) {
			return nil, ae
		}
	}
}

// report requests and downloads one report.
func (c *client) report(ctx context.Context, from, to time.Time) ([]byte, error) {
	ref, err := c.send(ctx, from, to)
	if err != nil {
		return nil, err
	}
	return c.fetch(ctx, ref)
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

func progress(f source.Fetch, format string, args ...any) {
	if f.Progress != nil {
		f.Progress(format, args...)
	}
}

// ---- Test

// Test implements source.Tester: one week's report, which checks the token,
// the query id and that the query is an XML Activity Flex Query.
func (s *Source) Test(ctx context.Context) error {
	to := s.yesterday()
	_, err := s.statement(ctx, to.AddDate(0, 0, -6), to, nil)
	return err
}

// yesterday is the last day a report covers: IBKR builds them after the
// close, so today is not complete yet.
func (s *Source) yesterday() time.Time {
	n := s.c.now().In(eastern)
	return time.Date(n.Year(), n.Month(), n.Day(), 0, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
}

// statement downloads and parses the report for [from, to].
func (s *Source) statement(ctx context.Context, from, to time.Time, log func(string, ...any)) (*statement, error) {
	body, err := s.c.report(ctx, from, to)
	if err != nil {
		return nil, err
	}
	st, err := parse(body, log)
	if err != nil {
		return nil, err
	}
	if to.Equal(s.yesterday()) {
		s.latest, s.latestAt = st, s.c.now()
	}
	return st, nil
}

// ---- Activities

// overlap re-reads a week before the last sync: IBKR books some cash
// (dividends, interest, fees) days after their date.
const overlap = 7 * 24 * time.Hour

// window is the longest period IBKR returns in one report.
const window = 365

// maxWindows bounds the walk back in time when the opening date is unknown.
const maxWindows = 30

func (s *Source) Activities(ctx context.Context, f source.Fetch) error {
	to := s.yesterday()
	log := func(format string, args ...any) { progress(f, format, args...) }
	seen := map[string]bool{}
	emit := func(a model.Activity) error {
		if seen[a.NativeID] { // windows touch at their edges
			return nil
		}
		seen[a.NativeID] = true
		return f.Emit(a)
	}
	var stop time.Time // the first day to read; zero: until two empty years
	switch {
	case !f.Since.IsZero():
		since := f.Since.Add(-overlap).UTC()
		stop = time.Date(since.Year(), since.Month(), since.Day(), 0, 0, 0, 0, time.UTC)
	case !s.from.IsZero():
		stop = s.from
	}
	empty := 0
	for i, end := 0, to; ; i++ {
		if i >= maxWindows {
			progress(f, "stopped after %d years: set \"Account opened on\" to read further back", maxWindows)
			break
		}
		start := end.AddDate(0, 0, -(window - 1))
		if !stop.IsZero() && start.Before(stop) {
			start = stop
		}
		progress(f, "report %s to %s", start.Format("2006-01-02"), end.Format("2006-01-02"))
		st, err := s.statement(ctx, start, end, log)
		if err != nil {
			var ae *APIError
			if errors.As(err, &ae) && ae.Code == "1003" && stop.IsZero() && end.Before(to) {
				break // before the account existed
			}
			return err
		}
		n, err := st.activities(emit)
		if err != nil {
			return err
		}
		if !stop.IsZero() && !start.After(stop) {
			break
		}
		if n == 0 && len(st.positions) == 0 {
			empty++
		} else {
			empty = 0
		}
		if stop.IsZero() && empty >= 2 {
			break
		}
		end = start.AddDate(0, 0, -1)
	}
	return nil
}

// ---- Balances

// Balances implements source.Source: open positions and cash per currency
// from the newest report, read again when Activities did not just read it.
func (s *Source) Balances(ctx context.Context) ([]model.Balance, error) {
	st := s.latest
	if st == nil || s.c.now().Sub(s.latestAt) > 15*time.Minute {
		to := s.yesterday()
		var err error
		if st, err = s.statement(ctx, to.AddDate(0, 0, -6), to, nil); err != nil {
			return nil, err
		}
	}
	return st.balances()
}
