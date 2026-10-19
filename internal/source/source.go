// Package source defines what a connector to a platform (exchange, broker,
// blockchain) must do, and keeps the registry of available connectors.
package source

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"time"

	"github.com/Alexis-Marcel/fundarr/internal/model"
)

// Field is one setting of a connector, rendered as a form field by the UI.
type Field struct {
	Name     string `json:"name"`
	Label    string `json:"label"`
	Secret   bool   `json:"secret,omitempty"`   // stored encrypted, never sent back to the UI
	Optional bool   `json:"optional,omitempty"` //
	Help     string `json:"help,omitempty"`
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
	// New builds a connector from its settings (secrets decrypted).
	New func(cfg Config) (Source, error) `json:"-"`
}

// Config is what a connector receives: its settings and an HTTP client.
type Config struct {
	Values map[string]string
	HTTP   *http.Client
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
	From, To   time.Time        // period covered (inclusive)
	Activities []model.Activity // the whole history of that period
	Notes      []string         // what the user should know (lines skipped...)
}

// Tester can be implemented to check credentials when a source is saved.
type Tester interface {
	Test(ctx context.Context) error
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
