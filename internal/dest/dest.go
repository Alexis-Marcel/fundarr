// Package dest defines where normalized activities are delivered (portfolio
// trackers, budgeting apps, files) and keeps the registry of destinations.
package dest

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/Alexis-Marcel/fundarr/internal/model"
	"github.com/Alexis-Marcel/fundarr/internal/source"
	"github.com/shopspring/decimal"
)

// Definition describes a kind of destination.
type Definition struct {
	Kind        string                                       `json:"kind"`
	Name        string                                       `json:"name"`
	Description string                                       `json:"description"`
	Fields      []source.Field                               `json:"fields"`
	Setup       string                                       `json:"setup,omitempty"`
	New         func(cfg source.Config) (Destination, error) `json:"-"`
}

// Account is a place in the destination activities can go to.
type Account struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Currency string `json:"currency,omitempty"`
}

// Status of one delivered activity.
type Status string

const (
	Delivered Status = "delivered" // written in the destination
	Duplicate Status = "duplicate" // the destination already had it
	Skipped   Status = "skipped"   // nothing to write (e.g. not representable); not retried
	Failed    Status = "failed"    // retried on the next run
)

// Result for one activity.
type Result struct {
	ActivityID string `json:"activityId"`
	Status     Status `json:"status"`
	ExternalID string `json:"externalId,omitempty"`
	Message    string `json:"message,omitempty"`
}

// Batch is the activities of one source going to one destination account.
type Batch struct {
	SourceName string
	Account    string // Account.ID chosen for this source
	Activities []model.Activity
	// Costs: what an incoming asset cost, by activity id, in the account's
	// currency (see model.Costs). Missing when no price is known.
	Costs map[string]decimal.Decimal
	// Transfers: for a deposit or withdrawal between two of your own
	// accounts, the name of the source on the other side, by activity id.
	Transfers map[string]string
}

// Destination is a configured target.
type Destination interface {
	// Accounts lists where activities can go, for the mapping screen.
	Accounts(ctx context.Context) ([]Account, error)
	// Push writes the activities and reports each one's fate. It must be
	// safe to call again with activities that were already delivered.
	Push(ctx context.Context, b Batch) ([]Result, error)
}

// Config is the same shape as a source's.
type Config = source.Config

var (
	mu   sync.RWMutex
	defs = map[string]Definition{}
)

func Register(d Definition) {
	mu.Lock()
	defer mu.Unlock()
	if _, dup := defs[d.Kind]; dup {
		panic("dest: duplicate kind " + d.Kind)
	}
	defs[d.Kind] = d
}

func Get(kind string) (Definition, error) {
	mu.RLock()
	defer mu.RUnlock()
	d, ok := defs[kind]
	if !ok {
		return Definition{}, fmt.Errorf("unknown destination kind %q", kind)
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
