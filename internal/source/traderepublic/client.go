package traderepublic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/source"
)

// Trade Republic's web API, as its web app (app.traderepublic.com) uses it.
// It is not public: what follows is what pytr (github.com/pytr-org/pytr)
// and Sure's client found. Sign-in, v2:
//
//	POST /api/v2/auth/web/login {phoneNumber, pin}      → processId
//	GET  /api/v2/auth/web/login/processes/{id}          → status, requiredAction
//	POST …/processes/{id}/authenticator-verification    {code} (TOTP accounts)
//	POST /api/v2/auth/web/login/qr-challenges           → challengeId, qrCodePayload
//	GET  /api/v2/auth/web/login/qr-challenges/{id}      → processId once scanned
//
// Each with the headers of the web app (X-TR-Device-Info, X-TR-App-Version,
// X-Tr-Platform) and a Chrome user agent. The user approves in the Trade
// Republic app; the session is three cookies (tr_session, tr_refresh,
// tr_device). tr_refresh is a JWT that ends 24 hours after the sign-in;
// GET /api/v1/auth/web/session renews tr_session with it. Then:
//
//	POST /api/v1/portfolio-analytics/transactions/export/request {from, to} → jobId
//	GET  …/export/status?jobId=   until COMPLETED
//	GET  …/export/download?jobId= → the CSV of Profile → Statements
//
// Balances come over the websocket (ws.go). A refused sign-in is never
// retried: Trade Republic locks the account for a day after a few.

const (
	defaultBase = "https://api.traderepublic.com"
	// The web app's build, sent with every sign-in call: Trade Republic
	// refuses old ones after a while. FUNDARR_TR_APP_VERSION (or the
	// source's advanced setting) overrides it without a new Fundarr.
	appVersion = "2.2631.13"
	platform   = "web-pro"
	userAgent  = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	locale     = "en"
	// The export starts here: Trade Republic opened in 2015.
	exportFrom = "2015-01-01"
)

func env(name, def string) string {
	if v := strings.TrimSpace(os.Getenv(name)); v != "" {
		return v
	}
	return def
}

type client struct {
	base       string
	http       *http.Client
	jar        *cookiejar.Jar
	appVersion string
	platform   string
	ua         string
	device     string // stable device id, 128 hex digits
	logf       func(format string, args ...any)
	sleep      func(ctx context.Context, d time.Duration) error
	now        func() time.Time
}

func newClient(base string, hc *http.Client, device, version string) *client {
	jar, _ := cookiejar.New(nil)
	h := &http.Client{Timeout: 60 * time.Second}
	if hc != nil {
		c := *hc
		h = &c
	}
	h.Jar = jar
	if version == "" {
		version = env("FUNDARR_TR_APP_VERSION", appVersion)
	}
	return &client{base: base, http: h, jar: jar, appVersion: version, platform: env("FUNDARR_TR_PLATFORM", platform),
		ua: env("FUNDARR_TR_USER_AGENT", userAgent), device: device, logf: func(string, ...any) {},
		sleep: sleepCtx, now: func() time.Time { return time.Now().UTC() }}
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

func newDevice() string {
	b := make([]byte, 64)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// deviceInfo is the X-TR-Device-Info header: what the web app says of the
// browser, base64 JSON. Only what a server can say truthfully, as pytr.
func (c *client) deviceInfo() string {
	ver := "146.0.0.0"
	if i := strings.Index(c.ua, "Chrome/"); i >= 0 {
		ver, _, _ = strings.Cut(c.ua[i+len("Chrome/"):], " ")
	}
	loc := time.Now().Location()
	name := loc.String()
	if name == "Local" || name == "" {
		name = "Etc/UTC"
	}
	_, off := time.Now().Zone()
	b, _ := json.Marshal(map[string]any{
		"stableDeviceId": c.device, "browser": "Chrome", "browserVersion": ver, "os": "Linux", "osVersion": "",
		"timezone": name, "timezoneOffset": -off / 60, "screen": "1920x1080x24", "preferredLanguages": []string{locale}, "numberOfCores": 4,
	})
	return base64.StdEncoding.EncodeToString(b)
}

// apiError is an answer other than 2xx: its status and Trade Republic's
// error code, never the body (it may echo what was sent).
type apiError struct {
	Status int
	Code   string
	Retry  time.Duration
}

func (e *apiError) Error() string {
	msg := loginErrors[e.Code]
	if msg == "" {
		msg = fmt.Sprintf("HTTP %d", e.Status)
		if e.Code != "" {
			msg += " " + e.Code
		}
	}
	return "Trade Republic: " + msg
}

// unauthorized: the session is gone.
func (e *apiError) unauthorized() bool { return e.Status == 401 || e.Status == 403 }

var loginErrors = map[string]string{
	"PROCESS_GONE":                 "the sign-in request expired",
	"ALREADY_PROCESSED":            "the sign-in was refused or already used",
	"NOT_FOUND":                    "unknown sign-in request",
	"TOO_MANY_REQUESTS":            "too many attempts, wait before trying again",
	"VALIDATION_CODE_INVALID":      "wrong code",
	"VALIDATION_CODE_ALREADY_USED": "that code was already used",
	"MISSING_REQUIRED_HEADER":      "the sign-in headers were refused (the web app version may be too old)",
}

// do sends a request with the web app's headers and decodes a JSON answer
// into out (when not nil); raw, when not nil, gets the body as is.
func (c *client) do(ctx context.Context, method, path string, body any, out any, raw *[]byte) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	u := path
	if !strings.HasPrefix(path, "http") {
		u = c.base + path
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json, text/plain, */*")
	req.Header.Set("Accept-Language", locale)
	req.Header.Set("X-TR-Device-Info", c.deviceInfo())
	req.Header.Set("X-TR-App-Version", c.appVersion)
	req.Header.Set("X-Tr-Platform", c.platform)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("Trade Republic: %w", scrub(err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		e := &apiError{Status: resp.StatusCode}
		var eb struct {
			Errors []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"errors"`
		}
		if json.Unmarshal(data, &eb) == nil && len(eb.Errors) > 0 {
			e.Code = eb.Errors[0].ErrorCode
		}
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil {
			e.Retry = time.Duration(s) * time.Second
		}
		if resp.StatusCode == 405 && strings.Contains(strings.ToLower(resp.Header.Get("Server")), "awselb") {
			e.Code = "BLOCKED_BY_FIREWALL"
		}
		c.logf("%s %s: HTTP %d %s", method, logPath(path), resp.StatusCode, e.Code)
		return e
	}
	if raw != nil {
		*raw = data
	}
	if out != nil && len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("Trade Republic %s: unexpected answer", logPath(path))
		}
	}
	return nil
}

// logPath leaves ids out of what is logged.
func logPath(p string) string {
	if u, err := url.Parse(p); err == nil {
		p = u.Path
	}
	parts := strings.Split(p, "/")
	for i, s := range parts {
		if len(s) > 20 || strings.Count(s, "-") >= 3 {
			parts[i] = "…"
		}
	}
	return strings.Join(parts, "/")
}

// scrub drops the URL (with its query) from a transport error.
func scrub(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return ue.Err
	}
	return err
}

// ---- session

// cookies of the session, to save in the state.
func (c *client) cookies() []kv {
	u, _ := url.Parse(c.base)
	var out []kv
	for _, k := range c.jar.Cookies(u) {
		out = append(out, kv{k.Name, k.Value})
	}
	return out
}

type kv struct {
	Name  string `json:"n"`
	Value string `json:"v"`
}

func (c *client) setCookies(list []kv) {
	u, _ := url.Parse(c.base)
	var cs []*http.Cookie
	for _, k := range list {
		cs = append(cs, &http.Cookie{Name: k.Name, Value: k.Value, Path: "/"})
	}
	c.jar.SetCookies(u, cs)
}

// sessionEnd reads when the session can no longer be renewed: the expiry
// of the tr_refresh JWT (24 hours after the sign-in when it cannot be read).
func sessionEnd(cookies []kv, signedIn time.Time) time.Time {
	for _, k := range cookies {
		if k.Name != "tr_refresh" {
			continue
		}
		parts := strings.Split(k.Value, ".")
		if len(parts) < 2 {
			break
		}
		payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
		if err != nil {
			break
		}
		var claims struct {
			Exp int64 `json:"exp"`
		}
		if json.Unmarshal(payload, &claims) == nil && claims.Exp > 0 {
			return time.Unix(claims.Exp, 0).UTC()
		}
	}
	if signedIn.IsZero() {
		return time.Time{}
	}
	return signedIn.Add(24 * time.Hour)
}

func hasSession(cookies []kv) bool {
	for _, k := range cookies {
		if k.Name == "tr_session" || k.Name == "tr_refresh" {
			return true
		}
	}
	return false
}

// refresh renews tr_session with tr_refresh. A refusal means signing in again.
func (c *client) refresh(ctx context.Context) error {
	err := c.do(ctx, http.MethodGet, "/api/v1/auth/web/session", nil, nil, nil)
	var e *apiError
	if errors.As(err, &e) && e.unauthorized() {
		return source.ErrLogin
	}
	return err
}

// call runs a request with the session, renewing it once when refused.
func (c *client) call(ctx context.Context, method, path string, body, out any, raw *[]byte) error {
	err := c.do(ctx, method, path, body, out, raw)
	var e *apiError
	if errors.As(err, &e) && e.unauthorized() {
		if err := c.refresh(ctx); err != nil {
			return err
		}
		err = c.do(ctx, method, path, body, out, raw)
		if errors.As(err, &e) && e.unauthorized() {
			return source.ErrLogin
		}
	}
	return err
}

// ---- sign-in

// process is a sign-in waiting for the user.
type process struct {
	Method    string    `json:"method"` // "app" or "qr"
	ID        string    `json:"id,omitempty"`
	Challenge string    `json:"challenge,omitempty"` // QR challenge id
	QR        string    `json:"qr,omitempty"`
	Code      bool      `json:"code,omitempty"` // an authenticator code is asked
	Expires   time.Time `json:"expires"`
}

type processAnswer struct {
	ProcessID          string          `json:"processId"`
	CountdownInSeconds int             `json:"countdownInSeconds"`
	Status             string          `json:"status"`
	State              string          `json:"state"`
	RequiredAction     string          `json:"requiredAction"`
	ExpiresAt          json.RawMessage `json:"expiresAt"`
	ChallengeID        string          `json:"challengeId"`
	ChallengeExpiresAt json.RawMessage `json:"challengeExpiresAt"`
	QRCodePayload      string          `json:"qrCodePayload"`
}

// done: the user approved.
func (p processAnswer) done() bool {
	for _, s := range []string{p.Status, p.State} {
		switch strings.ToUpper(s) {
		case "CONFIRMED", "COMPLETED", "APPROVED", "SUCCESS", "OK", "DONE":
			return true
		}
	}
	return false
}

// failed: the sign-in will not go through.
func (p processAnswer) failed() string {
	for _, s := range []string{p.Status, p.State} {
		switch u := strings.ToUpper(s); u {
		case "", "PENDING", "WAITING", "CREATED", "SCANNED", "INITIATED", "IN_PROGRESS":
		case "CONFIRMED", "COMPLETED", "APPROVED", "SUCCESS", "OK", "DONE":
		case "REJECTED", "DECLINED", "DENIED", "CANCELLED", "CANCELED":
			return "the sign-in was declined in the app"
		case "EXPIRED", "TIMEOUT", "GONE":
			return "the sign-in was not approved in time"
		default:
			return "the sign-in stopped (" + u + ")"
		}
	}
	return ""
}

// parseExpiry reads a time as Trade Republic writes it: ISO text, or
// seconds or milliseconds since 1970.
func parseExpiry(raw json.RawMessage, fallback time.Time) time.Time {
	if len(raw) == 0 || string(raw) == "null" {
		return fallback
	}
	var n float64
	if json.Unmarshal(raw, &n) == nil && n > 0 {
		if n > 1e11 {
			return time.UnixMilli(int64(n)).UTC()
		}
		return time.Unix(int64(n), 0).UTC()
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
			return t.UTC()
		}
	}
	return fallback
}

// startApp asks for a sign-in with the phone number and PIN: Trade
// Republic sends a notification to the app.
func (c *client) startApp(ctx context.Context, phone, pin string) (*process, error) {
	var a processAnswer
	if err := c.do(ctx, http.MethodPost, "/api/v2/auth/web/login", map[string]string{"phoneNumber": phone, "pin": pin}, &a, nil); err != nil {
		return nil, err
	}
	if a.ProcessID == "" {
		return nil, errors.New("Trade Republic: the sign-in answer has no process")
	}
	wait := a.CountdownInSeconds
	if wait <= 0 || wait > 600 {
		wait = 120
	}
	p := &process{Method: "app", ID: a.ProcessID, Expires: c.now().Add(time.Duration(wait) * time.Second)}
	var st processAnswer
	if err := c.do(ctx, http.MethodGet, "/api/v2/auth/web/login/processes/"+url.PathEscape(p.ID), nil, &st, nil); err == nil {
		p.Code = st.RequiredAction == "AUTHENTICATOR_VERIFICATION"
		p.Expires = parseExpiry(st.ExpiresAt, p.Expires)
	}
	c.logf("sign-in started: %s, %ds to approve", map[bool]string{true: "authenticator code asked", false: "notification sent to the app"}[p.Code],
		int(p.Expires.Sub(c.now()).Seconds()))
	return p, nil
}

// startQR asks for a QR code the app scans; no phone number nor PIN.
func (c *client) startQR(ctx context.Context) (*process, error) {
	var a processAnswer
	if err := c.do(ctx, http.MethodPost, "/api/v2/auth/web/login/qr-challenges", nil, &a, nil); err != nil {
		return nil, err
	}
	if a.ChallengeID == "" {
		return nil, errors.New("Trade Republic: the QR answer has no challenge")
	}
	p := &process{Method: "qr", Challenge: a.ChallengeID, QR: a.QRCodePayload, Expires: parseExpiry(a.ChallengeExpiresAt, c.now().Add(2*time.Minute))}
	c.logf("QR sign-in started, %ds to scan", int(p.Expires.Sub(c.now()).Seconds()))
	return p, nil
}

// poll checks on a sign-in: true once approved.
func (c *client) poll(ctx context.Context, p *process) (bool, error) {
	if p.ID == "" && p.Challenge != "" {
		var a processAnswer
		if err := c.do(ctx, http.MethodGet, "/api/v2/auth/web/login/qr-challenges/"+url.PathEscape(p.Challenge), nil, &a, nil); err != nil {
			return false, err
		}
		if a.QRCodePayload != "" {
			p.QR = a.QRCodePayload
		}
		if msg := a.failed(); msg != "" {
			return false, errors.New("Trade Republic: " + msg)
		}
		if a.ProcessID == "" {
			return a.done(), nil
		}
		p.ID = a.ProcessID
		c.logf("QR code scanned")
	}
	var a processAnswer
	if err := c.do(ctx, http.MethodGet, "/api/v2/auth/web/login/processes/"+url.PathEscape(p.ID), nil, &a, nil); err != nil {
		return false, err
	}
	if msg := a.failed(); msg != "" {
		return false, errors.New("Trade Republic: " + msg)
	}
	if a.RequiredAction == "AUTHENTICATOR_VERIFICATION" && !a.done() {
		p.Code = true
	}
	return a.done(), nil
}

// answer sends the authenticator code.
func (c *client) answer(ctx context.Context, p *process, code string) error {
	if p.ID == "" {
		return errors.New("no sign-in waits for a code")
	}
	if err := c.do(ctx, http.MethodPost, "/api/v2/auth/web/login/processes/"+url.PathEscape(p.ID)+"/authenticator-verification",
		map[string]string{"code": code}, nil, nil); err != nil {
		return err
	}
	p.Code = false
	c.logf("authenticator code accepted")
	return nil
}

// account checks the session: the securities account number it acts for.
func (c *client) account(ctx context.Context) (string, error) {
	var a struct {
		SecuritiesAccountNumber string `json:"securitiesAccountNumber"`
	}
	if err := c.call(ctx, http.MethodGet, "/api/v2/auth/account", nil, &a, nil); err != nil {
		return "", err
	}
	return a.SecuritiesAccountNumber, nil
}

// ---- export

// export downloads the transaction export from..to (dates).
func (c *client) export(ctx context.Context, from, to string) ([]byte, error) {
	var job struct {
		JobID string `json:"jobId"`
		ID    string `json:"id"`
	}
	c.logf("export: asking for %s to %s", from, to)
	if err := c.call(ctx, http.MethodPost, "/api/v1/portfolio-analytics/transactions/export/request", map[string]string{"from": from, "to": to}, &job, nil); err != nil {
		return nil, err
	}
	if job.JobID == "" {
		job.JobID = job.ID
	}
	if job.JobID == "" {
		return nil, errors.New("Trade Republic: the export request has no job")
	}
	q := "?jobId=" + url.QueryEscape(job.JobID)
	start := c.now()
	for wait := time.Second; ; wait = min(wait*2, 5*time.Second) {
		var st struct {
			Status string `json:"status"`
			State  string `json:"state"`
		}
		if err := c.call(ctx, http.MethodGet, "/api/v1/portfolio-analytics/transactions/export/status"+q, nil, &st, nil); err != nil {
			return nil, err
		}
		s := strings.ToUpper(st.Status + st.State)
		if s == "COMPLETED" || s == "DONE" || s == "READY" || s == "SUCCEEDED" {
			break
		}
		if s == "FAILED" || s == "ERROR" || s == "CANCELLED" {
			return nil, errors.New("Trade Republic: the export failed on its side")
		}
		if c.now().Sub(start) > 3*time.Minute {
			return nil, errors.New("Trade Republic: the export was not ready after 3 minutes")
		}
		if err := c.sleep(ctx, wait); err != nil {
			return nil, err
		}
	}
	c.logf("export: ready after %.0fs", c.now().Sub(start).Seconds())
	var data []byte
	if err := c.call(ctx, http.MethodGet, "/api/v1/portfolio-analytics/transactions/export/download"+q, nil, nil, &data); err != nil {
		return nil, err
	}
	// The file itself, or a link to it.
	if t := bytes.TrimSpace(data); len(t) > 0 && t[0] == '{' {
		var link struct {
			URL         string `json:"url"`
			DownloadURL string `json:"downloadUrl"`
		}
		if json.Unmarshal(t, &link) == nil && (link.URL != "" || link.DownloadURL != "") {
			u := link.URL
			if u == "" {
				u = link.DownloadURL
			}
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
			if err != nil {
				return nil, err
			}
			resp, err := c.http.Do(req)
			if err != nil {
				return nil, fmt.Errorf("Trade Republic export: %w", scrub(err))
			}
			defer resp.Body.Close()
			if resp.StatusCode != 200 {
				return nil, fmt.Errorf("Trade Republic export: HTTP %d", resp.StatusCode)
			}
			if data, err = io.ReadAll(io.LimitReader(resp.Body, 64<<20)); err != nil {
				return nil, err
			}
		}
	}
	c.logf("export: %d KB", (len(data)+1023)/1024)
	return data, nil
}
