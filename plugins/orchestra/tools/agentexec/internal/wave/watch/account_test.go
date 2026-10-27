package watch

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/statusfile"
)

func TestProgressCarriesTheRunsAccountFromItsStatusEntry(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	store := newStore(t, "a")
	path := store.Path
	acme := "Acme"
	if err := statusfile.Default().WriteRun(statusfile.Run{Key: statusfile.RunKey(path), Account: &acme}); err != nil {
		t.Fatal(err)
	}
	st, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}

	p := Compute(path, &st, 0, nil)
	if p.Account == nil || *p.Account != "Acme" {
		t.Fatalf("account = %v, want Acme", p.Account)
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"account":"Acme"`) {
		t.Fatalf("progress JSON lacks account: %s", b)
	}
}
