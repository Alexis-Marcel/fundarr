package api

import (
	"encoding/json"
	"testing"
)

// An answer of /api/v1/activities decodes as is; only deposits key the
// transfers.
func TestDecodeAndLedger(t *testing.T) {
	var page struct{ Activities []Activity }
	err := json.Unmarshal([]byte(`{"activities": [
 {"id": "in", "sourceId": "b", "time": "2024-01-02T00:00:00Z", "type": "deposit", "base": {"asset": {"kind": "crypto", "symbol": "BTC"}, "qty": "1"},
  "source": {"id": "b", "name": "Ledger", "kind": "wallet"}, "transferWith": "out", "cost": "40000.00", "currency": "EUR"},
 {"id": "out", "sourceId": "a", "time": "2024-01-01T00:00:00Z", "type": "withdrawal", "base": {"asset": {"kind": "crypto", "symbol": "BTC"}, "qty": "1"},
  "transferWith": "in"}
], "next": ""}`), &page)
	if err != nil || len(page.Activities) != 2 || page.Activities[0].Source.Name != "Ledger" || page.Activities[0].Cost != "40000.00" ||
		page.Activities[0].Base.Qty.String() != "1" {
		t.Fatalf("decoded %+v %v", page, err)
	}
	acts, transfers := Ledger(page.Activities)
	if acts[0].ID != "out" || len(transfers) != 1 || transfers["in"] != "out" {
		t.Fatalf("ledger %+v %v", acts, transfers)
	}
	c := Copy{}
	c.Apply(Changes{Changes: []Change{{Op: Upsert, ID: "in", Activity: &page.Activities[0]}, {Op: Remove, ID: "in"}, {Op: Remove, ID: "nope"}}})
	if len(c) != 0 {
		t.Fatalf("copy %v", c)
	}
}
