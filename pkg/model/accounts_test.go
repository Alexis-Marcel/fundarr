package model

import (
	"testing"
	"time"

	"github.com/shopspring/decimal"
)

// A broker with a compte-titres ("") and a PEA: cash is checked account by
// account, corrections go to the account that is off, and nothing is ever
// moved from one account to the other.
func TestReconcileByAccount(t *testing.T) {
	eur, tte := CryptoOrFiat("EUR"), Asset{Kind: Security, Symbol: "FR0000120271", ISIN: "FR0000120271"}
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	acts := []Activity{
		{Account: "", Type: Deposit, Base: Amt(eur, "100"), Time: t0},
		{Account: "PEA", Type: Deposit, Base: Amt(eur, "50"), Time: t0},
		{Account: "PEA", Type: Dividend, Base: Amt(eur, "5.40"), Quote: ptr(Amt(tte, "0")), Time: t0.Add(24 * time.Hour)},
	}
	tol := decimal.New(1, -6)
	cases := []struct {
		name string
		bal  []Balance
		want []Gap // Account and Diff only
	}{
		{"each account its cash", []Balance{{Account: "", Amount: Amt(eur, "100")}, {Account: "PEA", Amount: Amt(eur, "55.40")}}, nil},
		{"the PEA's cash unread", []Balance{{Account: "", Amount: Amt(eur, "100")}, {Account: "PEA", Amount: Amt(eur, "0"), Unread: true}}, nil},
		{"one figure for all", []Balance{{Account: AllAccounts, Amount: Amt(eur, "155.40")}}, nil},
		{"the PEA's cash counted in the main one", []Balance{{Account: "", Amount: Amt(eur, "155.40")}}, nil},
		{"the PEA short", []Balance{{Account: "", Amount: Amt(eur, "100")}, {Account: "PEA", Amount: Amt(eur, "50")}},
			[]Gap{{Account: "PEA", Diff: decimal.RequireFromString("-5.40")}}},
		{"short over all", []Balance{{Account: AllAccounts, Amount: Amt(eur, "150")}}, []Gap{{Account: "", Diff: decimal.RequireFromString("-5.40")}}},
		{"opposite ways, short in all", []Balance{{Account: "", Amount: Amt(eur, "140")}, {Account: "PEA", Amount: Amt(eur, "10")}},
			[]Gap{{Account: "PEA", Diff: decimal.RequireFromString("-5.40")}}},
	}
	for _, c := range cases {
		gaps := Reconcile(acts, c.bal, tol)
		if len(gaps) != len(c.want) {
			t.Fatalf("%s: gaps %+v", c.name, gaps)
		}
		for i, g := range gaps {
			if g.Account != c.want[i].Account || !g.Diff.Equal(c.want[i].Diff) {
				t.Fatalf("%s: gap %+v, want %+v", c.name, g, c.want[i])
			}
		}
		adj := Adjustments(acts, gaps, t0.Add(48*time.Hour))
		if len(adj) != len(gaps) {
			t.Fatalf("%s: corrections %+v", c.name, adj)
		}
		for i, a := range adj {
			if a.Account != gaps[i].Account {
				t.Fatalf("%s: correction in %q, gap in %q", c.name, a.Account, gaps[i].Account)
			}
		}
		if g := Reconcile(append(acts, adj...), c.bal, tol); len(g) != 0 {
			t.Fatalf("%s: still %+v", c.name, g)
		}
	}
	// A position counts in its own account: a PEA share is no CTO share.
	bal := []Balance{{Account: "", Amount: Amt(eur, "100")}, {Account: "PEA", Amount: Amt(eur, "55.40")}, {Account: "PEA", Amount: Amt(tte, "2")}}
	gaps := Reconcile(acts, bal, tol)
	if len(gaps) != 1 || gaps[0].Account != "PEA" || gaps[0].Pooled {
		t.Fatalf("PEA shares: %+v", gaps)
	}
	adj := Adjustments(acts, gaps, t0)
	if len(adj) != 1 || adj[0].Account != "PEA" || adj[0].NativeID == Adjustments(acts, []Gap{{Asset: tte, Diff: gaps[0].Diff}}, t0)[0].NativeID {
		t.Fatalf("PEA correction %+v", adj)
	}
}
