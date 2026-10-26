package transcript

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// transcriptSetup points every state and config location at a fresh temp
// directory, so no test reads or writes the real machine state.
func transcriptSetup(t *testing.T) (home, configDir string) {
	t.Helper()
	home = t.TempDir()
	configDir = filepath.Join(t.TempDir(), "claude")
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", t.TempDir())
	t.Setenv("CLAUDE_CONFIG_DIR", configDir)
	return home, configDir
}

// transcriptWrite creates path (and its parents) holding content.
func transcriptWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func usageLine(input, cacheRead, cacheCreate int) string {
	return fmt.Sprintf(`{"type":"assistant","message":{"usage":{"input_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`+"\n", input, cacheRead, cacheCreate)
}

func TestLastContextUsesLastUsageLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	transcriptWrite(t, path, usageLine(100, 50, 10)+usageLine(200, 900, 5)+`{"type":"user","message":{"content":"hi"}}`+"\n")

	tokens, ok := LastContext(path)
	if !ok || tokens != 1105 {
		t.Fatalf("LastContext = %d, %v; want 1105, true", tokens, ok)
	}
}

func TestLastContextWithoutUsage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	transcriptWrite(t, path, `{"type":"user","message":{"content":"usage"}}`+"\n")

	if _, ok := LastContext(path); ok {
		t.Fatal("LastContext reported usage for a transcript without any")
	}
}

func TestLastContextReadsLongLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.jsonl")
	long := `{"type":"user","message":{"content":"` + strings.Repeat("x", 2<<20) + `"}}` + "\n"
	transcriptWrite(t, path, usageLine(1, 2, 3)+long+usageLine(7, 8, 9))

	tokens, ok := LastContext(path)
	if !ok || tokens != 24 {
		t.Fatalf("LastContext = %d, %v; want 24, true", tokens, ok)
	}
}

func TestFindSessionFromEitherRoot(t *testing.T) {
	home, configDir := transcriptSetup(t)
	cfgRoot := filepath.Join(configDir, "projects")
	homeRoot := filepath.Join(home, ".claude", "projects")
	transcriptWrite(t, filepath.Join(cfgRoot, "p1", "sid-a.jsonl"), "")
	transcriptWrite(t, filepath.Join(homeRoot, "p1", "sid-b.jsonl"), "")
	roots := Roots("", "")

	for _, sid := range []string{"sid-a", "sid-b"} {
		if _, ok := Find(roots, sid, ""); !ok {
			t.Errorf("Find(%s) found nothing", sid)
		}
	}
}

func TestFindSubagentDirectAndNested(t *testing.T) {
	home, _ := transcriptSetup(t)
	root := filepath.Join(home, ".claude", "projects")
	direct := filepath.Join(root, "p1", "sid", "subagents", "agent-a1.jsonl")
	nested := filepath.Join(root, "p1", "sid", "subagents", "workflows", "w", "agent-a2.jsonl")
	transcriptWrite(t, direct, "")
	transcriptWrite(t, nested, "")
	roots := Roots("", "")

	if got, ok := Find(roots, "sid", "a1"); !ok || got != direct {
		t.Errorf("Find(a1) = %q, %v; want %q", got, ok, direct)
	}
	if got, ok := Find(roots, "sid", "a2"); !ok || got != nested {
		t.Errorf("Find(a2) = %q, %v; want %q", got, ok, nested)
	}
}

func TestFindRejectsTraversal(t *testing.T) {
	home, _ := transcriptSetup(t)
	root := filepath.Join(home, ".claude", "projects")
	transcriptWrite(t, filepath.Join(root, "p1", "sid", "subagents", "agent-x.jsonl"), "")
	transcriptWrite(t, filepath.Join(root, "x.jsonl"), "")
	roots := Roots("", "")

	if _, ok := Find(roots, "../x", ""); ok {
		t.Error("Find accepted a session id with a path separator")
	}
	if _, ok := Find(roots, "sid", "../x"); ok {
		t.Error("Find accepted an agent id with a path separator")
	}
	if _, ok := Find(roots, "..", ""); ok {
		t.Error("Find accepted the parent-directory id")
	}
}

func TestRecordHostedKeepsFirstRecordedDistinctIDs(t *testing.T) {
	transcriptSetup(t)
	first := NewSessionID()
	second := NewSessionID()

	RecordHosted("run-1", first)
	RecordHosted("run-1", second)
	RecordHosted("run-1", first)

	got := HostedSessions("run-1")
	if len(got) != 2 || got[0] != first || got[1] != second {
		t.Fatalf("HostedSessions = %v; want [%s %s]", got, first, second)
	}
}

func TestRecordHostedRejectsInvalidInput(t *testing.T) {
	transcriptSetup(t)

	RecordHosted("../escape", NewSessionID())
	RecordHosted("run-2", "not-a-uuid")

	if got := HostedSessions("run-2"); got != nil {
		t.Errorf("HostedSessions(run-2) = %v; want nil after a non-UUID record", got)
	}
	if _, err := os.Stat(filepath.Join(os.Getenv("XDG_STATE_HOME"), "claude-orchestra", "hosted")); err == nil {
		entries, _ := os.ReadDir(filepath.Join(os.Getenv("XDG_STATE_HOME"), "claude-orchestra", "hosted"))
		if len(entries) != 0 {
			t.Errorf("hosted index holds %d files after invalid records; want none", len(entries))
		}
	}
}

func TestNewSessionIDIsVersion4UUID(t *testing.T) {
	re := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	id := NewSessionID()
	if !re.MatchString(id) {
		t.Fatalf("NewSessionID() = %q; not a version-4 UUID", id)
	}
	if other := NewSessionID(); other == id {
		t.Errorf("NewSessionID returned %q twice", id)
	}
}
