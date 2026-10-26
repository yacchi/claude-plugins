package hookrun

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func stampsTempState(t *testing.T) {
	t.Helper()
	t.Setenv("XDG_STATE_HOME", t.TempDir())
}

func TestStampsKeepEveryEventOfASession(t *testing.T) {
	stampsTempState(t)
	at := time.Unix(1_800_000_000, 0)
	if err := Record("s1", EventSessionStart, at); err != nil {
		t.Fatal(err)
	}
	if err := Record("s1", EventModule, at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	seen := Seen("s1")
	if seen[EventSessionStart] != at.Unix() || seen[EventModule] != at.Add(time.Minute).Unix() {
		t.Fatalf("Seen(s1) = %v, want both events", seen)
	}
}

func TestStampsConcurrentRecordsSurvive(t *testing.T) {
	stampsTempState(t)
	events := make([]string, 8)
	for i := range events {
		events[i] = fmt.Sprintf("event-%d", i)
	}
	var wg sync.WaitGroup
	errs := make(chan error, len(events))
	for _, ev := range events {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- Record("race", ev, time.Unix(1_800_000_000, 0))
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seen := Seen("race")
	for _, ev := range events {
		if _, ok := seen[ev]; !ok {
			t.Errorf("event %s lost; Seen = %v", ev, seen)
		}
	}
}

func TestStampsSessionIDCannotEscapeDir(t *testing.T) {
	stampsTempState(t)
	at := time.Unix(1_800_000_000, 0)
	if err := Record("../../evil/../x", EventModule, at); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(Dir(), "evilx.json")); err != nil {
		t.Fatalf("reduced session file missing: %v", err)
	}
	if err := Record("/../..", EventModule, at); err == nil {
		t.Fatal("Record with an id that reduces to empty succeeded, want error")
	}
}

func TestStampsPruneOnlyOnFirstRecordOfASession(t *testing.T) {
	stampsTempState(t)
	now := time.Unix(1_800_000_000, 0)
	if err := os.MkdirAll(Dir(), 0o755); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(Dir(), "old.json")
	fresh := filepath.Join(Dir(), "fresh.json")
	for p, mtime := range map[string]time.Time{
		old:   now.Add(-15 * 24 * time.Hour),
		fresh: now.Add(-time.Hour),
	} {
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	if err := Record("new", EventModule, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("15-day-old stamp file survived the first record: %v", err)
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Errorf("fresh stamp file pruned: %v", err)
	}

	// A further record of a known session must not prune.
	if err := os.WriteFile(old, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(old, now.Add(-15*24*time.Hour), now.Add(-15*24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := Record("new", EventSessionStart, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("prune ran on a session's second record: %v", err)
	}
}

func TestSeenOfUnknownSessionIsEmptyMap(t *testing.T) {
	stampsTempState(t)
	seen := Seen("nobody")
	if seen == nil || len(seen) != 0 {
		t.Fatalf("Seen(unknown) = %#v, want empty non-nil map", seen)
	}
}
