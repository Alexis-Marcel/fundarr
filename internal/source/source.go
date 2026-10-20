// Package source defines what a connector to a platform (exchange, broker,
// blockchain) must do, and keeps the registry of available connectors.
package source

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

// Field is one setting of a connector, rendered as a form field by the UI.
type Field struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Secret   bool   `json:"secret,omitempty"`   // stored encrypted, never sent back to the UI
	Optional bool   `json:"optional,omitempty"` //
	Help     string `json:"help,omitempty"`
	// Lines: one value per line (a list). A secret list comes back to the
	// UI masked line by line; masked lines sent back keep their stored value.
	Lines bool `json:"lines,omitempty"`
	// Advanced: rarely needed, tucked away in the form.
	Advanced bool `json:"advanced,omitempty"`
	// Hidden: set by the UI itself (a choice made before the form), never
	// shown as a field.
	Hidden bool `json:"hidden,omitempty"`
	// Mode: shown only when the source is used this way (see
	// Definition.Modes); empty for every mode.
	Mode string `json:"mode,omitempty"`
}

// ModeField holds which of a definition's Modes a source uses.
const ModeField = "mode"

// Mode is one way of using a source, picked in its form (stored in the
// ModeField setting; the first mode when unset).
type Mode struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Help string `json:"help,omitempty"`
	Beta bool   `json:"beta,omitempty"`
}

// Definition describes a kind of source.
type Definition struct {
	Kind        string  `json:"kind"`
	Name        string  `json:"name"`
	Category    string  `json:"category"` // "exchange", "broker", "wallet"
	Description string  `json:"description"`
	Fields      []Field `json:"fields"`
	// Setup is shown above the form: how to create a read-only key, etc.
	Setup string `json:"setup,omitempty"`
	// Statement, when set, says how to export the platform's own statement
	// for the connector's Importer (and enables the import in the UI).
	Statement string `json:"statement,omitempty"`
	// Limits says what the platform's API cannot tell, shown where balance
	// corrections are, so the user knows why they exist.
	Limits string `json:"limits,omitempty"`
	// Detect, for a Lines field, recognizes one value (a key, an address)
	// so the UI can say what it is before saving.
	Detect func(value string) (Detected, error) `json:"-"`
	// Preview reads what one value of a list holds now, so the user sees
	// before saving that it is the right account. cfg carries the source's
	// other values (servers, keys).
	Preview func(ctx context.Context, value string, cfg Config) ([]model.Balance, error) `json:"-"`
	// Hidden: kept for sources already set up, not offered for new ones
	// (another kind covers it).
	Hidden bool `json:"hidden,omitempty"`
	// NoTest: the form has no "Test connection" (checking would need the
	// user, like a sign-in to approve).
	NoTest bool `json:"noTest,omitempty"`
	// Modes, when set, are the ways the source can be used (a statement
	// file, a connection...); fields with a Mode belong to one of them.
	Modes []Mode `json:"modes,omitempty"`
	// SealState: the connector's state holds a secret (a session), so the
	// engine stores it encrypted.
	SealState bool `json:"-"`
	// New builds a connector from its settings (secrets decrypted).
	New func(cfg Config) (Source, error) `json:"-"`
}

// Detected is what a value of a list is: Chain names it for people; Value
// is how to store it (it may gain a prefix that picks the chain). Choices
// are the other chains the same value can be read on.
type Detected struct {
	Kind    string   `json:"kind"` // the chain, for icons: "bitcoin", "evm"…
	Chain   string   `json:"chain"`
	Value   string   `json:"value"`
	Choices []Choice `json:"choices,omitempty"`
}

type Choice struct {
	Kind  string `json:"kind"`
	Chain string `json:"chain"`
	Value string `json:"value"`
}

// SplitLabel separates a list value from the name the user gave it: the
// name follows a tab ("zpub…\tSavings").
func SplitLabel(line string) (value, label string) {
	v, l, _ := strings.Cut(line, "\t")
	return strings.TrimSpace(v), strings.TrimSpace(l)
}

// NewHTTP is the client connectors share: a server that cannot be reached
// is given up after seconds, so a fallback gets its turn, while a slow
// answer still has a minute.
func NewHTTP() *http.Client {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.DialContext = (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext
	t.TLSHandshakeTimeout = 8 * time.Second
	return &http.Client{Timeout: 60 * time.Second, Transport: t}
}

// Config is what a connector receives: its settings and an HTTP client.
type Config struct {
	Values map[string]string
	HTTP   *http.Client
	// Log, when set, records what the connector does outside a sync (a
	// sign-in), for the run log. Never secrets.
	Log func(format string, args ...any)
	// State is what the connector saved after its last run (cursors, ids
	// already seen...). Opaque to the engine.
	State []byte
}

func (c Config) Get(name string) string { return c.Values[name] }

// Fetch is one run's request. Since is the time of the last successful run
// (zero on the first one): connectors fetch from a bit before it, the engine
// deduplicates by native id.
type Fetch struct {
	Since time.Time
	// Emit records one activity. NativeID must be stable across runs.
	Emit func(model.Activity) error
	// Progress reports what is being fetched, for the run log.
	Progress func(format string, args ...any)
}

// Source is a configured connector.
type Source interface {
	// Activities emits the history since f.Since (everything when zero).
	Activities(ctx context.Context, f Fetch) error
	// Balances returns what the platform says is held now.
	Balances(ctx context.Context) ([]model.Balance, error)
	// State returns what to give back in Config.State next time.
	State() []byte
}

// Importer reads a statement exported from the platform (when the API does
// not tell the whole story). The statement becomes the reference for the
// period it covers: the engine replaces that period's history with what
// Import returns, and later syncs ignore API activities inside it.
type Importer interface {
	// Import parses the file. existing is the source's current history, so
	// the connector can keep API activities that describe the same movement
	// better (transaction hashes, ids, fiat amounts).
	Import(name string, data []byte, existing []model.Activity) (*Statement, error)
}

// Statement is what an import produced.
type Statement struct {
	From, To time.Time // period covered (inclusive)
	// Accounts: the sub-accounts the file covers (nil: all of them);
	// activities of other sub-accounts in the period stay.
	Accounts   []string
	Activities []model.Activity // the whole history of that period
	Notes      []string         // what the user should know (lines skipped...)
}

// Tester can be implemented to check credentials when a source is saved.
type Tester interface {
	Test(ctx context.Context) error
}

// Offline is implemented by connectors that may read nothing from the
// platform (statements only): Offline reports whether this one does.
type Offline interface {
	Offline() bool
}

// ErrNoBalances, from Balances, means the platform did not say what is
// held: the engine derives the balances from the history and says so.
var ErrNoBalances = errors.New("balances unknown")

// StatementFetcher is implemented by connectors whose sync downloads the
// platform's own statement (the file Importer reads): the engine imports it
// as if the user had, instead of calling Activities.
type StatementFetcher interface {
	FetchStatement(ctx context.Context, f Fetch) (name string, data []byte, err error)
}

// ---- interactive sign-in

// ErrLogin means the connector must be signed in again by the user (a
// session expired): syncs stop until then, nothing is retried.
var ErrLogin = errors.New("sign-in needed")

// Session is what an interactive connector says of its sign-in, without
// asking the platform.
type Session struct {
	// Needed: the source signs in interactively (a session it keeps).
	Needed bool `json:"needed"`
	// Until: when the current session ends (zero: none).
	Until time.Time `json:"until,omitempty"`
	// Methods: how a sign-in can be started ("app", "qr").
	Methods []string `json:"methods,omitempty"`
}

// Valid reports whether the session can be used at now.
func (s Session) Valid(now time.Time) bool { return !s.Needed || s.Until.After(now) }

// Challenge is what the user has to do for a sign-in to go on.
type Challenge struct {
	// Kind: "approve" (in the platform's app), "code" (type the code its
	// app or SMS shows), "qr" (scan Data with the platform's app).
	Kind    string    `json:"kind"`
	Data    string    `json:"data,omitempty"`
	Digits  int       `json:"digits,omitempty"`
	Expires time.Time `json:"expires"`
}

// Interactive is implemented by connectors that need the user to sign in
// now and then. The engine keeps the connector between calls while a
// sign-in is in progress; the session it ends with is part of State. A
// connector never retries a refused sign-in by itself.
type Interactive interface {
	Session() Session
	// Login starts a sign-in with one of Session().Methods.
	Login(ctx context.Context, method string) (*Challenge, error)
	// Poll checks on it: a nil challenge means signed in.
	Poll(ctx context.Context) (*Challenge, error)
	// Answer sends the code a "code" challenge asks for.
	Answer(ctx context.Context, code string) (*Challenge, error)
}

var (
	mu   sync.RWMutex
	defs = map[string]Definition{}
)

// Register adds a definition; connectors call it from init.
func Register(d Definition) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := defs[d.Kind]; dup {
		panic("source: duplicate kind " + d.Kind)
	}
	defs[d.Kind] = d
}

func Get(kind string) (Definition, error) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := defs[kind]
	if !ok {
		return Definition{}, fmt.Errorf("unknown source kind %q", kind)
	}
	return d, nil
}

func List() []Definition {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Definition, 0, len(defs))
	for _, d := range defs {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
