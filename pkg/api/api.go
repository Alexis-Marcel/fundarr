// Package api holds the JSON types of Fundarr's read API (/api/v1, see
// docs/api.md), so a Go app decodes the answers straight into them, and
// turns a copy of the history into what pkg/model and pkg/portfolio take:
//
//	var page api.Changes // GET /api/v1/changes?after=<cursor>
//	json.NewDecoder(resp.Body).Decode(&page)
//	copy.Apply(page) // copy is an api.Copy, kept with page.Cursor
//	acts, transfers := copy.Ledger()
//	now := portfolio.Now(acts, transfers, prices, time.Now())
package api

import (
	"sort"

	"github.com/Alexis-Marcel/fundarr/pkg/model"
)

// Source is a configured exchange or wallet.
type Source struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Kind string `json:"kind"`
}

// Activity is an activity with what Fundarr worked out across sources, as
// /api/v1/activities and /api/v1/changes serve it.
type Activity struct {
	model.Activity
	Source Source `json:"source"`
	// Cost: what an incoming asset cost, in Currency (bought with cash,
	// carried over a transfer, or valued at the day's price). Not in the
	// changes feed.
	Cost     string `json:"cost,omitempty"`
	Currency string `json:"currency,omitempty"`
	// TransferWith: the other side of a move between your own accounts.
	TransferWith string `json:"transferWith,omitempty"`
	// Correction: made by Fundarr's balance check, not by the platform.
	Correction bool `json:"correction,omitempty"`
}

// Change kinds.
const (
	Upsert = "upsert" // add or replace the activity
	Remove = "remove" // delete it
)

// Change is one entry of the changes feed; Activity is nil on removals.
type Change struct {
	Op       string    `json:"op"`
	ID       string    `json:"id"`
	Activity *Activity `json:"activity,omitempty"`
}

// Changes is one answer of GET /api/v1/changes: pass Cursor as after next
// time, and call again right away while More is true.
type Changes struct {
	Changes []Change `json:"changes"`
	Cursor  string   `json:"cursor"`
	More    bool     `json:"more"`
}

// Copy is a copy of the history kept from the changes feed, by id.
type Copy map[string]Activity

// Apply applies a page of changes, in order.
func (c Copy) Apply(page Changes) {
	for _, ch := range page.Changes {
		switch {
		case ch.Op == Remove:
			delete(c, ch.ID)
		case ch.Op == Upsert && ch.Activity != nil:
			c[ch.ID] = *ch.Activity
		}
	}
}

// Ledger returns the copy as Ledger does.
func (c Copy) Ledger() ([]model.Activity, map[string]string) {
	list := make([]Activity, 0, len(c))
	for _, a := range c {
		list = append(list, a)
	}
	return Ledger(list)
}

// Ledger returns the activities oldest first, and the transfers between
// your own sources Fundarr paired, as a deposit's id → its withdrawal's id:
// the two arguments of model.Books, portfolio.Now, History and TaxFR.
func Ledger(list []Activity) ([]model.Activity, map[string]string) {
	acts := make([]model.Activity, 0, len(list))
	transfers := map[string]string{}
	for _, a := range list {
		acts = append(acts, a.Activity)
		if a.TransferWith != "" && a.Type == model.Deposit {
			transfers[a.ID] = a.TransferWith
		}
	}
	sort.SliceStable(acts, func(i, j int) bool {
		if !acts[i].Time.Equal(acts[j].Time) {
			return acts[i].Time.Before(acts[j].Time)
		}
		return acts[i].ID < acts[j].ID
	})
	return acts, transfers
}
