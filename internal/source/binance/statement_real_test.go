package binance

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Alexis-Marcel/fundarr/internal/model"
)

// Against a real export: BINANCE_STATEMENT=/path/to/Binance-…csv
func TestStatementReal(t *testing.T) {
	path := os.Getenv("BINANCE_STATEMENT")
	if path == "" {
		t.Skip("BINANCE_STATEMENT not set")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	b := &Binance{}
	st, err := b.Import(filepath.Base(path), data, nil)
	if err != nil {
		t.Fatal(err)
	}
	types := map[string]int{}
	for _, a := range st.Activities {
		k := string(a.Type)
		if a.Type == model.Trade {
			k += "/" + string(a.Side)
		}
		types[k]++
	}
	t.Logf("from %s to %s: %v", st.From, st.To, types)
	for _, n := range st.Notes {
		t.Log(n)
		if strings.HasPrefix(n, "check:") {
			t.Error(n)
		}
	}
	seen := map[string]bool{}
	for _, a := range st.Activities {
		if seen[a.NativeID] {
			t.Errorf("duplicate id %s", a.NativeID)
		}
		seen[a.NativeID] = true
	}
}
