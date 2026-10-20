package traderepublic

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Fake stands for Trade Republic's web API and websocket, shaped as pytr
// and Sure's client describe them. Nothing here reaches Trade Republic.
type Fake struct {
	*httptest.Server
	Phone, PIN string
	TOTP       bool // the account signs in with an authenticator code
	// ApproveAfter: polls of a sign-in before the user approves it.
	ApproveAfter int
	// Decline: the user declines in the app.
	Decline bool
	// Claimed: once scanned and approved, the QR challenge answers CLAIMED
	// with no process to follow, the cookies being the session.
	Claimed bool
	// TooMany: the sign-in answers 429.
	TooMany bool
	// NoSocket: the websocket refuses.
	NoSocket bool
	// Export is the CSV the export downloads.
	Export string

	mu        sync.Mutex
	Logins    int // sign-ins started with a phone number
	QRs       int
	polls     map[string]int
	verified  map[string]bool
	scanned   map[string]string
	sessions  map[string]bool // tr_session values that work
	refreshes map[string]bool // tr_refresh values that work
	Requests  []string        // method and path of every call
	jobs      int
	seq       int
}

func NewFake(t testing.TB) *Fake {
	f := &Fake{Phone: "+33612345678", PIN: "1234", ApproveAfter: 2, polls: map[string]int{}, verified: map[string]bool{},
		scanned: map[string]string{}, sessions: map[string]bool{}, refreshes: map[string]bool{}}
	f.Server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.Close)
	return f
}

// Revoke ends every session, as Trade Republic does after a day.
func (f *Fake) Revoke() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions, f.refreshes = map[string]bool{}, map[string]bool{}
}

func (f *Fake) Calls(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, r := range f.Requests {
		if strings.HasPrefix(r, prefix) {
			n++
		}
	}
	return n
}

func (f *Fake) id(kind string) string {
	f.seq++
	return fmt.Sprintf("%s-0000-4000-8000-%012d", kind, f.seq)
}

func fail(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"errors":[{"errorCode":%q,"errorMessage":"x"}]}`, code)
}

func reply(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(v)
}

// jwt makes a tr_refresh-like token ending at exp.
func jwt(exp time.Time, n int) string {
	enc := base64.RawURLEncoding.EncodeToString
	return enc([]byte(`{"alg":"none"}`)) + "." + enc([]byte(fmt.Sprintf(`{"exp":%d,"n":%d}`, exp.Unix(), n))) + ".sig"
}

func (f *Fake) giveSession(w http.ResponseWriter) {
	f.seq++
	ses, ref := fmt.Sprintf("ses-%d", f.seq), jwt(time.Now().Add(24*time.Hour), f.seq)
	f.sessions[ses], f.refreshes[ref] = true, true
	http.SetCookie(w, &http.Cookie{Name: "tr_session", Value: ses, Path: "/", HttpOnly: true})
	// Scoped to where it is used, as a real server may do: a jar asked for
	// "/" alone does not tell it.
	http.SetCookie(w, &http.Cookie{Name: "tr_refresh", Value: ref, Path: "/api/v1/auth/web/session", HttpOnly: true})
	http.SetCookie(w, &http.Cookie{Name: "tr_device", Value: "dev", Path: "/"})
}

func (f *Fake) signedIn(r *http.Request) bool {
	c, err := r.Cookie("tr_session")
	return err == nil && f.sessions[c.Value]
}

func (f *Fake) serve(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Upgrade") == "websocket" {
		f.socket(w, r)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Requests = append(f.Requests, r.Method+" "+r.URL.Path)
	if r.Header.Get("X-TR-Device-Info") == "" || r.Header.Get("X-TR-App-Version") == "" || r.Header.Get("X-Tr-Platform") != "web-pro" ||
		!strings.Contains(r.Header.Get("User-Agent"), "Chrome/") {
		fail(w, 400, "MISSING_REQUIRED_HEADER")
		return
	}
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	p := r.URL.Path
	switch {
	case r.Method == "POST" && p == "/api/v2/auth/web/login":
		f.Logins++
		if f.TooMany {
			w.Header().Set("Retry-After", "3600")
			fail(w, 429, "TOO_MANY_REQUESTS")
			return
		}
		if body["phoneNumber"] != f.Phone || body["pin"] != f.PIN {
			fail(w, 401, "AUTHENTICATION_ERROR")
			return
		}
		reply(w, map[string]any{"processId": f.id("p"), "countdownInSeconds": 120})
	case r.Method == "POST" && p == "/api/v2/auth/web/login/qr-challenges":
		f.QRs++
		reply(w, map[string]any{"challengeId": f.id("q"), "challengeExpiresAt": time.Now().Add(2 * time.Minute).Format(time.RFC3339),
			"qrCodePayload": "trqr://challenge/1"})
	case r.Method == "GET" && strings.HasPrefix(p, "/api/v2/auth/web/login/qr-challenges/"):
		id := strings.TrimPrefix(p, "/api/v2/auth/web/login/qr-challenges/")
		f.polls[id]++
		if f.polls[id] < 2 {
			reply(w, map[string]any{"status": "PENDING", "qrCodePayload": fmt.Sprintf("trqr://challenge/%d", f.polls[id]+1)})
			return
		}
		if f.Claimed {
			f.giveSession(w)
			reply(w, map[string]any{"status": "CLAIMED"})
			return
		}
		if f.scanned[id] == "" {
			f.scanned[id] = f.id("p")
		}
		reply(w, map[string]any{"status": "SCANNED", "processId": f.scanned[id]})
	case r.Method == "POST" && strings.HasSuffix(p, "/authenticator-verification"):
		id := strings.TrimSuffix(strings.TrimPrefix(p, "/api/v2/auth/web/login/processes/"), "/authenticator-verification")
		if body["code"] != "123456" {
			fail(w, 400, "VALIDATION_CODE_INVALID")
			return
		}
		f.verified[id] = true
		w.WriteHeader(204)
	case r.Method == "GET" && strings.HasPrefix(p, "/api/v2/auth/web/login/processes/"):
		id := strings.TrimPrefix(p, "/api/v2/auth/web/login/processes/")
		if f.TOTP && !f.verified[id] && !strings.HasPrefix(id, "q") {
			reply(w, map[string]any{"status": "PENDING", "requiredAction": "AUTHENTICATOR_VERIFICATION", "expiresAt": time.Now().Add(2 * time.Minute).UnixMilli()})
			return
		}
		f.polls[id]++
		switch {
		case f.Decline:
			reply(w, map[string]any{"status": "REJECTED"})
		case f.polls[id] > f.ApproveAfter:
			f.giveSession(w)
			reply(w, map[string]any{"status": "CONFIRMED"})
		default:
			reply(w, map[string]any{"status": "PENDING", "expiresAt": time.Now().Add(2 * time.Minute).Format(time.RFC3339)})
		}
	case r.Method == "GET" && p == "/api/v1/auth/web/session":
		c, err := r.Cookie("tr_refresh")
		if err != nil || !f.refreshes[c.Value] {
			fail(w, 401, "UNAUTHORIZED")
			return
		}
		f.seq++
		ses := fmt.Sprintf("ses-%d", f.seq)
		f.sessions[ses] = true
		http.SetCookie(w, &http.Cookie{Name: "tr_session", Value: ses, Path: "/"})
		w.WriteHeader(204)
	case !f.signedIn(r):
		fail(w, 401, "UNAUTHORIZED")
	case r.Method == "GET" && p == "/api/v2/auth/account":
		reply(w, map[string]any{"securitiesAccountNumber": "SEC1", "currency": "EUR"})
	case r.Method == "POST" && p == "/api/v1/portfolio-analytics/transactions/export/request":
		if body["from"] == "" || body["to"] == "" {
			fail(w, 400, "BAD_REQUEST")
			return
		}
		f.jobs = 0
		reply(w, map[string]any{"jobId": "job-1"})
	case r.Method == "GET" && p == "/api/v1/portfolio-analytics/transactions/export/status":
		f.jobs++
		st := "PENDING"
		if f.jobs > 1 {
			st = "COMPLETED"
		}
		reply(w, map[string]any{"status": st})
	case r.Method == "GET" && p == "/api/v1/portfolio-analytics/transactions/export/download":
		w.Header().Set("Content-Type", "text/csv")
		w.Write([]byte(f.Export))
	default:
		fail(w, 404, "NOT_FOUND")
	}
}

// What the fake account holds, as the websocket tells it.
var (
	fakePairs = `{"accounts":[{"securitiesAccountNumber":"SEC1","cashAccountNumber":"CASH1","productType":"DEFAULT"},` +
		`{"securitiesAccountNumber":"SEC2","cashAccountNumber":"CASH2","productType":"TAX_WRAPPER"}]}`
	fakePortfolio = map[string]string{
		"SEC1": `{"categories":[{"categoryType":"stocksAndETFs","positions":[{"instrumentId":"FR0000120271","netSize":"6"},` +
			`{"instrumentId":"US0378331005","netSize":"4"},{"instrumentId":"IE00B4L5Y983","netSize":"0.0033"},{"instrumentId":"DE000A0D9PT0","netSize":"1"},` +
			`{"instrumentId":"NL0000000021","netSize":"1.5"}]},{"categoryType":"privateMarkets","positions":[{"instrumentId":"LU3170240538","netSize":"100"}]},` +
			`{"categoryType":"cryptos","positions":[{"instrumentId":"XF000BTC0017","netSize":"0.001"}]}]}`,
		"SEC2": `{"categories":[{"categoryType":"stocksAndETFs","positions":[{"instrumentId":"IE00B4L5Y983","netSize":5}]}]}`,
	}
	fakeCash = `[{"accountNumber":"CASH1","currencyId":"EUR","amount":-38.47},{"accountNumber":"CASH2","currencyId":"EUR","amount":70}]`
)

func (f *Fake) socket(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	ok := f.signedIn(r) && !f.NoSocket
	f.Requests = append(f.Requests, "WS /")
	f.mu.Unlock()
	if !ok {
		http.Error(w, "unauthorized", 401)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()
	_, msg, err := c.Read(ctx)
	if err != nil || !strings.HasPrefix(string(msg), "connect 31 ") || !strings.Contains(string(msg), `"clientId":"app.traderepublic.com"`) {
		c.Close(websocket.StatusPolicyViolation, "bad connect")
		return
	}
	c.Write(ctx, websocket.MessageText, []byte("connected"))
	for {
		_, msg, err := c.Read(ctx)
		if err != nil {
			return
		}
		cmd, rest, _ := strings.Cut(string(msg), " ")
		if cmd != "sub" {
			continue
		}
		id, payload, _ := strings.Cut(rest, " ")
		var sub map[string]string
		_ = json.Unmarshal([]byte(payload), &sub)
		// Something else first, as the real socket interleaves topics.
		c.Write(ctx, websocket.MessageText, []byte("999 A {}"))
		switch sub["type"] {
		case "accountPairs":
			c.Write(ctx, websocket.MessageText, []byte(id+" A "+fakePairs))
		case "compactPortfolioByType":
			if p, ok := fakePortfolio[sub["secAccNo"]]; ok {
				c.Write(ctx, websocket.MessageText, []byte(id+" A "+p))
			} else {
				c.Write(ctx, websocket.MessageText, []byte(id+` E {"errors":[{"errorCode":"NOT_FOUND"}]}`))
			}
		case "cash":
			c.Write(ctx, websocket.MessageText, []byte(id+" A "+fakeCash))
		default:
			c.Write(ctx, websocket.MessageText, []byte(id+` E {"errors":[{"errorCode":"UNKNOWN_TOPIC"}]}`))
		}
	}
}

// FakeExport is an export matching the fake's balances.
func FakeExport() string { return fakeExport() }
