package sandbox

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// Regression: Go's simple lowercase maps U+0130 (İ) to "i", so `İ.com`
// became the valid host `i.com` that Python (full mapping: "i̇")
// refuses. The Go policy must never be wider than Python's.
func TestDottedCapitalINeverBecomesAnASCIIHost(t *testing.T) {
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	if got := config.PyLower("İ.COM ΑΣ"); got != "i̇.com ας" {
		t.Errorf("PyLower = %q", got)
	}
	if DomainRefusal("İ.com") == "" {
		t.Error("İ.com not refused")
	}
	if _, err := AddDomainEntry("İ.com", "user", ""); err == nil {
		t.Error("İ.com stored")
	}
	// Through the config layers and the sandbox normalization alike.
	if err := os.MkdirAll(filepath.Join(home, ".claude"), 0o755); err != nil {
		t.Fatal(err)
	}
	layer := "sandbox:\n  network:\n    allowed_domains: [\"İ.com\"]\n"
	if err := os.WriteFile(filepath.Join(home, ".claude", "orchestra.yaml"), []byte(layer), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(home)
	cfg, err := config.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range ConfigOf(cfg).Network.AllowedDomains {
		if d == "i.com" {
			t.Errorf("allowlist got i.com: %v", ConfigOf(cfg).Network.AllowedDomains)
		}
	}
	if !contains(ConfigOf(cfg).Network.AllowedDomains, "i̇.com") {
		t.Errorf("allowlist %v", ConfigOf(cfg).Network.AllowedDomains)
	}
}

// Concurrent writers serialize on the store lock: no grant is lost.
func TestConcurrentGrantsAreAllKept(t *testing.T) {
	h := newHome(t)
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := AddGrantEntry(h.h(filepath.Join("g", string(rune('a'+i)))), "user", "", ""); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if l, _ := LoadLearned(); len(l.Grants) != 8 {
		t.Errorf("kept %d of 8 grants", len(l.Grants))
	}
}
