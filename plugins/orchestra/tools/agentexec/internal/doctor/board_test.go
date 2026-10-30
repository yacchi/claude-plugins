package doctor

import (
	"os"
	"path/filepath"
	"testing"
)

// Only the current session's recorded reason is a warning: a started writer
// (null reason) and another session's record are not.
func TestBoardBlocked(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".claude", "orchestra", "dashboard")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(body string) {
		if err := os.WriteFile(filepath.Join(dir, "blocked.json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if got := boardBlocked("s1"); got != "" {
		t.Errorf("no record: got %q", got)
	}
	write(`{"session":"s1","at":1,"reason":"the permission check answers ask"}`)
	if got := boardBlocked("s1"); got != "the permission check answers ask" {
		t.Errorf("own record: got %q", got)
	}
	if got := boardBlocked("s2"); got != "" {
		t.Errorf("another session's record: got %q", got)
	}
	write(`{"session":"s1","at":2,"reason":null}`)
	if got := boardBlocked("s1"); got != "" {
		t.Errorf("started writer: got %q", got)
	}
}
