package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"golang.org/x/crypto/argon2"
	"golang.org/x/oauth2"
)

// Auth modes, like the *arr apps:
//   - password: a username and password created in the UI on first launch
//     (or fixed by FUNDARR_PASSWORD_HASH);
//   - oidc: any OpenID Connect provider, with an allowlist;
//   - proxy: a reverse proxy authenticates and passes the user in a header;
//   - none: no login at all.
type AuthConfig struct {
	Mode string // "password", "oidc", "proxy" or "none"

	// Fixed credentials from the environment; when empty, the ones created
	// in the UI (stored in Credentials) are used.
	Username     string
	PasswordHash string // argon2id PHC string
	Credentials  credentials

	ProxyHeader string // proxy mode: header carrying the user (Remote-User)

	OIDCIssuer       string
	OIDCClientID     string
	OIDCClientSecret string
	OIDCAllowedSubs  []string
	OIDCAllowedMails []string
	// PublicURL is where users reach Fundarr (for the OIDC redirect).
	PublicURL string
	// OIDCAutoRedirect sends signed-out visitors straight to the provider:
	// with a session there, they come back signed in without a click.
	OIDCAutoRedirect bool

	SessionKey []byte
	SessionTTL time.Duration
}

// credentials stores the account created in the UI.
type credentials interface {
	Setting(ctx context.Context, key string) (string, error)
	PutSetting(ctx context.Context, key, value string) error
}

const (
	keyUser = "auth.username"
	keyHash = "auth.password_hash"
)

type auth struct {
	cfg      AuthConfig
	provider *oidc.Provider
	verifier *oidc.IDTokenVerifier
	oauth    *oauth2.Config
	endSess  string
}

const sessionCookie = "fundarr_session"
const stateCookie = "fundarr_oidc"

func newAuth(ctx context.Context, cfg AuthConfig) (*auth, error) {
	if cfg.SessionTTL == 0 {
		cfg.SessionTTL = 12 * time.Hour
	}
	a := &auth{cfg: cfg}
	switch cfg.Mode {
	case "none":
	case "proxy":
		if cfg.ProxyHeader == "" {
			cfg.ProxyHeader = "Remote-User"
		}
		a.cfg.ProxyHeader = cfg.ProxyHeader
	case "password":
		if cfg.PasswordHash != "" {
			if _, _, _, err := parsePHC(cfg.PasswordHash); err != nil {
				return nil, fmt.Errorf("FUNDARR_PASSWORD_HASH: %w", err)
			}
		} else if cfg.Credentials == nil {
			return nil, errors.New("password mode needs a credential store")
		}
		if a.cfg.Username == "" {
			a.cfg.Username = "admin"
		}
	case "oidc":
		if cfg.OIDCIssuer == "" || cfg.OIDCClientID == "" || cfg.PublicURL == "" {
			return nil, errors.New("OIDC needs FUNDARR_OIDC_ISSUER, FUNDARR_OIDC_CLIENT_ID and FUNDARR_URL")
		}
		if len(cfg.OIDCAllowedSubs) == 0 && len(cfg.OIDCAllowedMails) == 0 {
			return nil, errors.New("OIDC needs FUNDARR_OIDC_ALLOWED_SUBS or FUNDARR_OIDC_ALLOWED_EMAILS: Fundarr holds financial credentials, so not every account of the provider may sign in")
		}
		p, err := oidc.NewProvider(ctx, cfg.OIDCIssuer)
		if err != nil {
			return nil, fmt.Errorf("OIDC discovery: %w", err)
		}
		a.provider = p
		a.verifier = p.Verifier(&oidc.Config{ClientID: cfg.OIDCClientID})
		a.oauth = &oauth2.Config{
			ClientID:     cfg.OIDCClientID,
			ClientSecret: cfg.OIDCClientSecret,
			Endpoint:     p.Endpoint(),
			RedirectURL:  strings.TrimRight(cfg.PublicURL, "/") + "/auth/callback",
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		}
		var meta struct {
			EndSession string `json:"end_session_endpoint"`
		}
		_ = p.Claims(&meta)
		a.endSess = meta.EndSession
	default:
		return nil, fmt.Errorf("unknown auth mode %q (oidc, password or none)", cfg.Mode)
	}
	return a, nil
}

// ---- session cookie: "name|expiry|mac"

func (a *auth) sign(payload string) string {
	m := hmac.New(sha256.New, a.cfg.SessionKey)
	m.Write([]byte(payload))
	return base64.RawURLEncoding.EncodeToString(m.Sum(nil))
}

func (a *auth) issue(w http.ResponseWriter, r *http.Request, name string) {
	exp := time.Now().Add(a.cfg.SessionTTL).Unix()
	payload := base64.RawURLEncoding.EncodeToString([]byte(name)) + "|" + strconv.FormatInt(exp, 10)
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: payload + "|" + a.sign(payload), Path: "/",
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode, MaxAge: int(a.cfg.SessionTTL.Seconds())})
}

// user returns the signed-in user's name, "" if none.
func (a *auth) user(r *http.Request) string {
	switch a.cfg.Mode {
	case "none":
		return "admin"
	case "proxy":
		return strings.TrimSpace(r.Header.Get(a.cfg.ProxyHeader))
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	parts := strings.Split(c.Value, "|")
	if len(parts) != 3 {
		return ""
	}
	payload := parts[0] + "|" + parts[1]
	if subtle.ConstantTimeCompare([]byte(a.sign(payload)), []byte(parts[2])) != 1 {
		return ""
	}
	exp, err := strconv.ParseInt(parts[1], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return ""
	}
	name, _ := base64.RawURLEncoding.DecodeString(parts[0])
	return string(name)
}

func secure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
}

func clearCookie(w http.ResponseWriter, r *http.Request, name string) {
	http.SetCookie(w, &http.Cookie{Name: name, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode})
}

// ---- password

func parsePHC(s string) (params [3]uint32, salt, hash []byte, err error) {
	// $argon2id$v=19$m=65536,t=3,p=4$salt$hash
	p := strings.Split(s, "$")
	if len(p) != 6 || p[1] != "argon2id" {
		return params, nil, nil, errors.New("expected an argon2id PHC string ($argon2id$v=19$m=…,t=…,p=…$salt$hash)")
	}
	var m, t, par uint32
	if _, err := fmt.Sscanf(p[3], "m=%d,t=%d,p=%d", &m, &t, &par); err != nil {
		return params, nil, nil, errors.New("bad argon2id parameters")
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(p[4])
	hash, err2 := base64.RawStdEncoding.DecodeString(p[5])
	if err1 != nil || err2 != nil || len(hash) == 0 {
		return params, nil, nil, errors.New("bad argon2id salt or hash encoding")
	}
	return [3]uint32{m, t, par}, salt, hash, nil
}

// stored returns the username and password hash in effect ("" if none yet).
func (a *auth) stored(ctx context.Context) (string, string, error) {
	if a.cfg.PasswordHash != "" {
		return a.cfg.Username, a.cfg.PasswordHash, nil
	}
	user, err := a.cfg.Credentials.Setting(ctx, keyUser)
	if err != nil {
		return "", "", err
	}
	hash, err := a.cfg.Credentials.Setting(ctx, keyHash)
	return user, hash, err
}

// needsSetup is true in password mode until an account exists.
func (a *auth) needsSetup(ctx context.Context) bool {
	if a.cfg.Mode != "password" {
		return false
	}
	_, hash, err := a.stored(ctx)
	return err == nil && hash == ""
}

func checkPassword(hash, pw string) bool {
	params, salt, want, err := parsePHC(hash)
	if err != nil {
		return false
	}
	got := argon2.IDKey([]byte(pw), salt, params[1], params[0], uint8(params[2]), uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// HashPassword makes a PHC string, for the `fundarr hash-password` command.
func HashPassword(pw string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	h := argon2.IDKey([]byte(pw), salt, 3, 64*1024, 2, 32)
	return fmt.Sprintf("$argon2id$v=19$m=%d,t=%d,p=%d$%s$%s", 64*1024, 3, 2,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(h))
}

// ---- handlers

func (a *auth) handleLogin(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Mode != "password" {
		writeErr(w, http.StatusNotFound, "password login is not enabled")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	user, hash, err := a.stored(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || hash == "" ||
		!strings.EqualFold(strings.TrimSpace(body.Username), user) || !checkPassword(hash, body.Password) {
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusUnauthorized, "wrong username or password")
		return
	}
	a.issue(w, r, user)
	writeJSON(w, map[string]any{"user": user})
}

// handleSetup creates the account on first launch. Until then only requests
// from a private network are accepted, so a stranger cannot claim an
// instance that was exposed to the internet before being set up.
func (a *auth) handleSetup(w http.ResponseWriter, r *http.Request) {
	if !a.needsSetup(r.Context()) {
		writeErr(w, http.StatusConflict, "an account already exists")
		return
	}
	if !fromPrivateNetwork(r) {
		writeErr(w, http.StatusForbidden, "create the account from your local network, or set FUNDARR_PASSWORD_HASH")
		return
	}
	var body struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	user := strings.TrimSpace(body.Username)
	if user == "" || len(user) > 64 {
		writeErr(w, http.StatusUnprocessableEntity, "choose a username")
		return
	}
	if len(body.Password) < 8 {
		writeErr(w, http.StatusUnprocessableEntity, "the password needs at least 8 characters")
		return
	}
	if err := a.cfg.Credentials.PutSetting(r.Context(), keyUser, user); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if err := a.cfg.Credentials.PutSetting(r.Context(), keyHash, HashPassword(body.Password)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	a.issue(w, r, user)
	writeJSON(w, map[string]any{"user": user})
}

// handleChangePassword replaces the password of the account created in the UI.
func (a *auth) handleChangePassword(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Mode != "password" {
		writeErr(w, http.StatusNotFound, "password login is not enabled")
		return
	}
	if a.cfg.PasswordHash != "" {
		writeErr(w, http.StatusConflict, "the password is set by FUNDARR_PASSWORD_HASH; change it there")
		return
	}
	var body struct {
		Current string `json:"current"`
		New     string `json:"new"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	_, hash, err := a.stored(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !checkPassword(hash, body.Current) {
		time.Sleep(500 * time.Millisecond)
		writeErr(w, http.StatusForbidden, "the current password is wrong")
		return
	}
	if len(body.New) < 8 {
		writeErr(w, http.StatusUnprocessableEntity, "the password needs at least 8 characters")
		return
	}
	if err := a.cfg.Credentials.PutSetting(r.Context(), keyHash, HashPassword(body.New)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// fromPrivateNetwork is true when the client and every proxy it went
// through (X-Forwarded-For, X-Real-Ip) have private or loopback addresses.
func fromPrivateNetwork(r *http.Request) bool {
	addrs := []string{r.RemoteAddr}
	for _, h := range []string{"X-Forwarded-For", "X-Real-Ip"} {
		for _, v := range strings.Split(r.Header.Get(h), ",") {
			if v = strings.TrimSpace(v); v != "" {
				addrs = append(addrs, v)
			}
		}
	}
	for _, a := range addrs {
		host := a
		if h, _, err := net.SplitHostPort(a); err == nil {
			host = h
		}
		ip := net.ParseIP(strings.Trim(host, "[]"))
		if ip == nil || !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)) {
			return false
		}
	}
	return true
}

// Tailscale and other carrier-grade NAT addresses count as private.
var cgnat = &net.IPNet{IP: net.IPv4(100, 64, 0, 0), Mask: net.CIDRMask(10, 32)}

func randomString() string {
	b := make([]byte, 24)
	rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// handleOIDCStart redirects to the provider with state, nonce and PKCE.
func (a *auth) handleOIDCStart(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Mode != "oidc" {
		http.NotFound(w, r)
		return
	}
	state, nonce, verifier := randomString(), randomString(), oauth2.GenerateVerifier()
	v := url.Values{"s": {state}, "n": {nonce}, "v": {verifier}}.Encode()
	http.SetCookie(w, &http.Cookie{Name: stateCookie, Value: v + "|" + a.sign(v), Path: "/auth/", MaxAge: 600,
		HttpOnly: true, Secure: secure(r), SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

func (a *auth) handleOIDCCallback(w http.ResponseWriter, r *http.Request) {
	if a.cfg.Mode != "oidc" {
		http.NotFound(w, r)
		return
	}
	fail := func(msg string) {
		http.Redirect(w, r, "/?error="+url.QueryEscape(msg), http.StatusFound)
	}
	c, err := r.Cookie(stateCookie)
	clearCookie(w, r, stateCookie)
	if err != nil {
		fail("sign-in expired, try again")
		return
	}
	i := strings.LastIndex(c.Value, "|")
	if i < 0 || subtle.ConstantTimeCompare([]byte(a.sign(c.Value[:i])), []byte(c.Value[i+1:])) != 1 {
		fail("sign-in expired, try again")
		return
	}
	st, _ := url.ParseQuery(c.Value[:i])
	if e := r.URL.Query().Get("error"); e != "" {
		fail(e)
		return
	}
	if r.URL.Query().Get("state") != st.Get("s") {
		fail("sign-in expired, try again")
		return
	}
	tok, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(st.Get("v")))
	if err != nil {
		fail("the identity provider refused the sign-in")
		return
	}
	rawID, _ := tok.Extra("id_token").(string)
	idt, err := a.verifier.Verify(r.Context(), rawID)
	if err != nil || idt.Nonce != st.Get("n") {
		fail("invalid identity token")
		return
	}
	var claims struct {
		Sub      string `json:"sub"`
		Email    string `json:"email"`
		Verified bool   `json:"email_verified"`
		Name     string `json:"name"`
		Username string `json:"preferred_username"`
	}
	_ = idt.Claims(&claims)
	if !a.allowed(claims.Sub, claims.Email, claims.Verified) {
		fail("this account is not allowed to use Fundarr")
		return
	}
	name := claims.Name
	if name == "" {
		name = claims.Username
	}
	if name == "" {
		name = claims.Email
	}
	a.issue(w, r, name)
	http.Redirect(w, r, "/", http.StatusFound)
}

func (a *auth) allowed(sub, email string, verified bool) bool {
	for _, s := range a.cfg.OIDCAllowedSubs {
		if s != "" && s == sub {
			return true
		}
	}
	if verified {
		for _, m := range a.cfg.OIDCAllowedMails {
			if m != "" && strings.EqualFold(m, email) {
				return true
			}
		}
	}
	return false
}

func (a *auth) handleLogout(w http.ResponseWriter, r *http.Request) {
	clearCookie(w, r, sessionCookie)
	target := "/"
	if a.cfg.Mode == "oidc" && a.endSess != "" {
		target = a.endSess + "?" + url.Values{"client_id": {a.cfg.OIDCClientID}, "post_logout_redirect_uri": {strings.TrimRight(a.cfg.PublicURL, "/") + "/"}}.Encode()
	}
	writeJSON(w, map[string]any{"redirect": target})
}
