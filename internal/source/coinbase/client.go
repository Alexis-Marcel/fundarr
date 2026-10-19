package coinbase

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ---- CDP API key and JWT
//
// Coinbase App (v2) and Advanced Trade (v3) take a CDP API key: a name
// ("organizations/{org}/apiKeys/{id}", or a bare id for newer keys) and a
// private key, either ECDSA P-256 (PEM, signed ES256) or Ed25519 (base64 of
// seed + public key, signed EdDSA). Each request carries its own JWT, valid
// two minutes, bound to "METHOD host/path".

type signer struct {
	alg  string // "ES256" or "EdDSA"
	sign func(msg []byte) ([]byte, error)
}

var pemBlock = regexp.MustCompile(`(?s)-----BEGIN ([A-Z0-9 ]+)-----(.*?)-----END [A-Z0-9 ]+-----`)

// parseKey reads the private key as the user pasted it: multi-line PEM, PEM
// flattened to one line (a password field drops line breaks), PEM with
// literal "\n", or an Ed25519 base64 string. Errors never quote the key.
func parseKey(secret string) (*signer, error) {
	s := strings.TrimSpace(secret)
	s = strings.Trim(s, `"`)
	s = strings.ReplaceAll(s, `\n`, "\n")
	if m := pemBlock.FindStringSubmatch(s); m != nil {
		der, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(m[2]), ""))
		if err != nil {
			return nil, errors.New("coinbase: the private key is not valid PEM")
		}
		var key any
		switch m[1] {
		case "EC PRIVATE KEY":
			key, err = x509.ParseECPrivateKey(der)
		case "PRIVATE KEY":
			key, err = x509.ParsePKCS8PrivateKey(der)
		default:
			return nil, fmt.Errorf("coinbase: unexpected key type %q (want EC PRIVATE KEY)", m[1])
		}
		if err != nil {
			return nil, errors.New("coinbase: the private key cannot be read")
		}
		switch k := key.(type) {
		case *ecdsa.PrivateKey:
			if k.Curve != elliptic.P256() {
				return nil, errors.New("coinbase: the EC key must use the P-256 curve")
			}
			return ecSigner(k), nil
		case ed25519.PrivateKey:
			return edSigner(k), nil
		}
		return nil, errors.New("coinbase: the private key must be ECDSA P-256 or Ed25519")
	}
	raw := strings.Join(strings.Fields(s), "")
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		b, err := enc.DecodeString(raw)
		if err != nil {
			continue
		}
		switch len(b) {
		case ed25519.PrivateKeySize: // seed + public key
			k := ed25519.NewKeyFromSeed(b[:ed25519.SeedSize])
			if !k.Public().(ed25519.PublicKey).Equal(ed25519.PublicKey(b[ed25519.SeedSize:])) {
				return nil, errors.New("coinbase: the Ed25519 key is damaged (public half does not match)")
			}
			return edSigner(k), nil
		case ed25519.SeedSize:
			return edSigner(ed25519.NewKeyFromSeed(b)), nil
		}
	}
	return nil, errors.New("coinbase: the private key is neither a PEM EC key nor an Ed25519 key")
}

func ecSigner(k *ecdsa.PrivateKey) *signer {
	return &signer{alg: "ES256", sign: func(msg []byte) ([]byte, error) {
		h := sha256.Sum256(msg)
		r, s, err := ecdsa.Sign(rand.Reader, k, h[:])
		if err != nil {
			return nil, err
		}
		// JWS wants r || s, each 32 bytes, not ASN.1.
		out := make([]byte, 64)
		r.FillBytes(out[:32])
		s.FillBytes(out[32:])
		return out, nil
	}}
}

func edSigner(k ed25519.PrivateKey) *signer {
	return &signer{alg: "EdDSA", sign: func(msg []byte) ([]byte, error) {
		return k.Sign(nil, msg, crypto.Hash(0))
	}}
}

// jwt builds the bearer token for one request.
func (c *client) jwt(method, host, path string) (string, error) {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	now := c.now().Unix()
	header, _ := json.Marshal(map[string]string{"alg": c.signer.alg, "typ": "JWT", "kid": c.key, "nonce": hex.EncodeToString(nonce)})
	claims, _ := json.Marshal(map[string]any{
		"iss": "cdp", "sub": c.key, "nbf": now, "exp": now + 120,
		"uri": method + " " + host + path,
	})
	enc := base64.RawURLEncoding
	input := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	sig, err := c.signer.sign([]byte(input))
	if err != nil {
		return "", err
	}
	return input + "." + enc.EncodeToString(sig), nil
}

// ---- HTTP client

type client struct {
	base   string // scheme and host, no trailing slash
	host   string // as written in the JWT "uri" claim
	key    string
	signer *signer
	http   *http.Client
	now    func() time.Time
	sleep  func(context.Context, time.Duration) error
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

// APIError is an error answered by Coinbase. v2 sends {"errors": [{"id",
// "message"}]}, v3 {"error", "message"}.
type APIError struct {
	Status int
	ID     string
	Msg    string
	Path   string
}

func (e *APIError) Error() string {
	s := fmt.Sprintf("coinbase %s: HTTP %d", e.Path, e.Status)
	if e.ID != "" {
		s += " " + e.ID
	}
	if e.Msg != "" {
		s += ": " + e.Msg
	}
	return s
}

func apiError(status int, path string, body []byte) *APIError {
	e := &APIError{Status: status, Path: path}
	var v2 struct {
		Errors []struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"errors"`
		Error   string `json:"error"`
		Message string `json:"message"`
	}
	if json.Unmarshal(body, &v2) == nil {
		if len(v2.Errors) > 0 {
			e.ID, e.Msg = v2.Errors[0].ID, v2.Errors[0].Message
		} else {
			e.ID, e.Msg = v2.Error, v2.Message
		}
	}
	if e.ID == "" && e.Msg == "" {
		e.Msg = snippet(body)
	}
	return e
}

// isAuthError tells a refused key from a temporary failure.
func isAuthError(err error) bool {
	var e *APIError
	return errors.As(err, &e) && (e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden)
}

const maxAttempts = 6

// get calls a GET endpoint; pathQuery may carry a query string (next_uri).
// Rate limits (429), server errors and timeouts are retried with backoff.
func (c *client) get(ctx context.Context, pathQuery string, out any) error {
	path, _, _ := strings.Cut(pathQuery, "?")
	backoff := time.Second
	for attempt := 1; ; attempt++ {
		wait, err := c.once(ctx, pathQuery, path, out)
		if err == nil || wait == 0 || attempt >= maxAttempts {
			return err
		}
		if wait < backoff {
			wait = backoff
		}
		backoff *= 2
		if err := c.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// once performs one request; a non-zero wait means it may be retried.
func (c *client) once(ctx context.Context, pathQuery, path string, out any) (time.Duration, error) {
	token, err := c.jwt(http.MethodGet, c.host, path)
	if err != nil {
		return 0, fmt.Errorf("coinbase: signing request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base+pathQuery, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
		// url.Error names the URL, never the headers: the JWT stays out.
		var ne net.Error
		if (errors.As(err, &ne) && ne.Timeout()) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, io.EOF) {
			return time.Second, fmt.Errorf("coinbase %s: %w", path, err)
		}
		return 0, fmt.Errorf("coinbase %s: %w", path, err)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	resp.Body.Close()
	if err != nil {
		return time.Second, fmt.Errorf("coinbase %s: %w", path, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
		wait := time.Second
		if s, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && s > 0 {
			wait = time.Duration(s) * time.Second
		}
		if wait > 2*time.Minute {
			wait = 2 * time.Minute
		}
		return wait, apiError(resp.StatusCode, path, body)
	}
	if resp.StatusCode/100 != 2 {
		return 0, apiError(resp.StatusCode, path, body)
	}
	if out == nil {
		return 0, nil
	}
	if err := json.Unmarshal(body, out); err != nil {
		return 0, fmt.Errorf("coinbase %s: unexpected response: %w", path, err)
	}
	return 0, nil
}

func snippet(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) > 200 {
		s = s[:200] + "…"
	}
	return s
}

// hostOf returns the host the JWT names for a base URL.
func hostOf(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("coinbase: bad API URL %q", base)
	}
	return u.Host, nil
}
