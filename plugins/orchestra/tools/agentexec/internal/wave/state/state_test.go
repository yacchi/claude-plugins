package state

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "wave", "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	tick := 1000.0
	s.Clock = func() float64 { tick++; return tick + 0.5 }
	return s
}

func readEvents(t *testing.T, s *Store) []*jsonx.Object { return ReadEvents(s.Path, -1) }

func TestInitCreatesSchemaExactFile(t *testing.T) {
	s := newStore(t)
	if _, err := s.Init("plan.json", []string{"b", "a"}, "wave-int"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	// Python's json.dump(indent=2, sort_keys=True), no trailing newline.
	want := `{
  "integration": {
    "base": null,
    "path": null,
    "task": "wave-int"
  },
  "needs": [],
  "packages": {
    "a": {
      "attempts": 0,
      "commit": null,
      "detail": "",
      "executor": null,
      "files_changed": null,
      "session": false,
      "since": 1001.5,
      "status": "pending",
      "tree": null
    },
    "b": {
      "attempts": 0,
      "commit": null,
      "detail": "",
      "executor": null,
      "files_changed": null,
      "session": false,
      "since": 1001.5,
      "status": "pending",
      "tree": null
    }
  },
  "plan": "plan.json",
  "stopped": null,
  "updated": 1001.5,
  "version": 1,
  "wave": 0
}`
	if string(raw) != want {
		t.Fatalf("state.json differs:\n%s", raw)
	}
	// Re-init adds only the missing ids and leaves the rest alone.
	if err := s.SetStatus("a", "ready", "x", nil); err != nil {
		t.Fatal(err)
	}
	st, err := s.Init("other.json", []string{"a", "c"}, "other")
	if err != nil {
		t.Fatal(err)
	}
	if st.Plan != "plan.json" || st.Packages["a"].Status != "ready" || st.Packages["c"].Status != "pending" || len(st.Packages) != 3 {
		t.Fatalf("reinit: %+v", st)
	}
}

func TestSetStatusSinceAndEvents(t *testing.T) {
	s := newStore(t)
	if _, err := s.Init("p", []string{"a"}, "i"); err != nil {
		t.Fatal(err)
	}
	if err := s.SetStatus("a", "implementing", "go", func(p *Package) {
		exe := "pi"
		p.Executor = &exe
		p.Attempts = 1
	}); err != nil {
		t.Fatal(err)
	}
	first := s.Snapshot().Packages["a"]
	if err := s.SetStatus("a", "implementing", "again", nil); err != nil {
		t.Fatal(err)
	}
	second := s.Snapshot().Packages["a"]
	if second.Since != first.Since || second.Detail != "again" || *second.Executor != "pi" {
		t.Fatalf("since moved without a status change: %+v vs %+v", first, second)
	}
	if err := s.SetStatus("a", "verifying", "", nil); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Packages["a"].Since == first.Since {
		t.Fatal("since must move on a status change")
	}
	if err := s.SetStatus("a", "bogus", "", nil); err == nil {
		t.Fatal("unknown status accepted")
	}
	ev := readEvents(t, s)
	if len(ev) != 3 {
		t.Fatalf("events: %d", len(ev))
	}
	line, _ := jsonx.Marshal(ev[0])
	if !strings.HasPrefix(string(line), `{"at": 1002.5, "pkg": "a", "from": "pending", "to": "implementing", "event": "status", "detail": "go"}`) {
		t.Fatalf("event shape: %s", line)
	}
	// A package not in the file is created like Python's setdefault.
	if err := s.SetStatus("new", "failed", "d", nil); err != nil {
		t.Fatal(err)
	}
	if s.Snapshot().Packages["new"].Status != "failed" {
		t.Fatal("setdefault")
	}
}

func TestNeeds(t *testing.T) {
	s := newStore(t)
	if _, err := s.Init("p", []string{"a", "b"}, "i"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNeed("a", "scope", "wider"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNeed("b", "conflict", "c"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNeed("", "environment", "wave"); err != nil {
		t.Fatal(err)
	}
	if err := s.AddNeed("a", "nonsense", ""); err == nil {
		t.Fatal("unknown kind accepted")
	}
	st := s.Snapshot()
	if st.Packages["a"].Status != "needs" || len(st.Needs) != 3 || st.Needs[2].ID != nil {
		t.Fatalf("needs: %+v", st.Needs)
	}
	if err := s.ClearNeed("a"); err != nil {
		t.Fatal(err)
	}
	st = s.Snapshot()
	if len(st.Needs) != 2 || st.Packages["a"].Status != "needs" || *st.Needs[0].ID != "b" {
		t.Fatalf("clear targets only its own package and leaves status: %+v", st.Needs)
	}
	ev := readEvents(t, s)
	if got, _ := ev[2].Get("event"); got != "need" {
		t.Fatalf("wave-level need event: %v", got)
	}
}

func TestIntegrationWaveStopped(t *testing.T) {
	s := newStore(t)
	if _, err := s.Init("p", nil, "i"); err != nil {
		t.Fatal(err)
	}
	path, base := "/x", "abc"
	if err := s.SetIntegration(Integration{Path: &path, Base: &base}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWave(4); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkStopped("because"); err != nil {
		t.Fatal(err)
	}
	st := s.Snapshot()
	if *st.Integration.Path != "/x" || st.Integration.Task != nil || st.Wave != 4 || st.Stopped.Reason != "because" {
		t.Fatalf("%+v", st)
	}
	if err := s.ClearStopped(); err != nil || s.Snapshot().Stopped != nil {
		t.Fatalf("clear stopped: %v", err)
	}
}

func TestUnknownFieldsSurviveARewrite(t *testing.T) {
	s := newStore(t)
	input := `{"version": 1, "plan": "p", "wave": 1, "updated": 5, "stopped": null, "needs": [],
	  "integration": {"task": null, "path": null, "base": null},
	  "future": {"b": 1, "a": [1.5, {"z": null, "y": true}]},
	  "packages": {"a": {"status": "pending", "since": 3, "attempts": 0, "tree": null, "executor": null,
	    "session": "sess-1", "files_changed": null, "commit": null, "detail": "", "extra": [1, 2]}}}`
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(s.Path, []byte(input), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWave(2); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(s.Path)
	for _, want := range []string{
		`"future": {
    "a": [
      1.5,
      {
        "y": true,
        "z": null
      }
    ],
    "b": 1
  }`,
		`"extra": [
        1,
        2
      ]`,
		`"session": "sess-1"`,
		`"since": 3.0`,
		`"wave": 2`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %q in\n%s", want, raw)
		}
	}
}

func TestEventAndTruncatedLastLine(t *testing.T) {
	s := newStore(t)
	if _, err := s.Init("p", nil, "i"); err != nil {
		t.Fatal(err)
	}
	s.Event("custom", map[string]any{"pkg": "a", "detail": "free\nform"})
	s.Event("other", nil)
	f, err := os.OpenFile(s.EventsPath(), os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString(`{"at": 1, "pk`)
	f.Close()
	if n := len(ReadEvents(s.Path, -1)); n != 2 {
		t.Fatalf("truncated line must be skipped, got %d", n)
	}
	last := ReadEvents(s.Path, 1)
	if len(last) != 1 {
		t.Fatalf("limit: %d", len(last))
	}
	if ev, _ := last[0].Get("event"); ev != "other" {
		t.Fatalf("limit keeps the newest: %v", ev)
	}
	if ReadEvents(filepath.Join(t.TempDir(), "none.json"), 3) != nil {
		t.Fatal("missing events file")
	}
	if !strings.HasSuffix(s.EventsPath(), "state.events.jsonl") {
		t.Fatal(s.EventsPath())
	}
}

func TestConcurrentUpdatesLoseNothing(t *testing.T) {
	s := newStore(t)
	ids := make([]string, 8)
	for i := range ids {
		ids[i] = string(rune('a' + i))
	}
	if _, err := s.Init("p", ids, "i"); err != nil {
		t.Fatal(err)
	}
	s.Clock = wallClock
	var wg sync.WaitGroup
	const perPkg = 15
	for _, id := range ids {
		wg.Add(1)
		go func() {
			defer wg.Done()
			other, _ := Open(s.Path) // a separate Store, as a separate process would have
			for n := 0; n < perPkg; n++ {
				if err := other.UpdatePackage(id, func(p *Package) { p.Attempts++ }); err != nil {
					t.Error(err)
					return
				}
				other.Event("tick", map[string]any{"pkg": id})
			}
		}()
	}
	wg.Wait()
	st := s.Snapshot()
	for _, id := range ids {
		if st.Packages[id].Attempts != perPkg {
			t.Errorf("%s: %d updates survived", id, st.Packages[id].Attempts)
		}
	}
	if n := len(ReadEvents(s.Path, -1)); n != len(ids)*perPkg {
		t.Fatalf("events: %d", n)
	}
}

func TestStopFlag(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "sub", "state.json")
	if StopRequested(statePath) {
		t.Fatal("no STOP yet")
	}
	if err := RequestStop(statePath, "halt"); err != nil {
		t.Fatal(err)
	}
	if !StopRequested(statePath) {
		t.Fatal("STOP not seen")
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "sub", "STOP")); string(b) != "halt" {
		t.Fatalf("STOP content %q", b)
	}
	if err := ClearStop(statePath); err != nil || StopRequested(statePath) {
		t.Fatalf("clear: %v", err)
	}
	if err := ClearStop(statePath); err != nil {
		t.Fatalf("clearing a missing STOP: %v", err)
	}
}

func TestRegistry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("ORCHESTRA_WAVE_REGISTRY", filepath.Join(dir, "reg", "waves.jsonl"))
	if got := ListWaves(); got != nil {
		t.Fatalf("no registry file: %v", got)
	}
	mkState := func(name string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	s1, s2, s3 := mkState("s1.json"), mkState("s2.json"), mkState("s3.json")
	reg := func(state string, at float64) {
		if err := Register(Registration{State: state, Plan: "plan.json", Repo: dir, Into: "int", RegisteredAt: at}); err != nil {
			t.Fatal(err)
		}
	}
	reg(s1, 1)
	reg(s2, 2)
	reg(s1, 3) // moves s1 to the newest
	reg(s3, 4)
	os.Remove(s3)
	f, _ := os.OpenFile(os.Getenv("ORCHESTRA_WAVE_REGISTRY"), os.O_APPEND|os.O_WRONLY, 0o644)
	f.WriteString("garbage\n{\"state\": \"" + s2 + "\", \"pl")
	f.Close()
	var states []string
	for _, r := range ListWaves() {
		states = append(states, r.State)
	}
	if !reflect.DeepEqual(states, []string{s1, s2}) {
		t.Fatalf("got %v", states)
	}
	planAbs, _ := filepath.Abs("plan.json")
	raw, _ := os.ReadFile(os.Getenv("ORCHESTRA_WAVE_REGISTRY"))
	first := strings.SplitN(string(raw), "\n", 2)[0]
	if !strings.HasPrefix(first, `{"into": "int", "plan": "`+planAbs+`", "registered_at": 1.0, "repo": "`) {
		t.Fatalf("line must be sort_keys with absolute paths: %s", first)
	}
}

func fixtureState() State {
	exe, tree := "pi", "/nonexistent"
	id := "d"
	return State{
		Version: 1, Wave: 2, Updated: 100,
		Packages: map[string]*Package{
			"a": {Status: "implementing", Since: 940, Attempts: 1, Executor: &exe, Tree: &tree},
			"b": {Status: "integrated", Since: 100, Attempts: 2, FilesChanged: ptrInt(3)},
			"c": {Status: "failed", Since: 0, FilesChanged: ptrInt(0)},
			"d": {Status: "needs", Since: 50},
			"e": {Status: "pending"},
			"f": {Status: "verifying", Since: 4000, NetworkDenials: []any{map[string]any{"host": "h", "port": int64(443)}}},
		},
		Needs:   []Need{{ID: &id, Kind: "scope", Detail: "wider\nmore"}, {Kind: "environment", Detail: "wave"}},
		Stopped: &Stopped{Reason: "by test"},
	}
}

func ptrInt(n int) *int { return &n }

func TestRenderStatus(t *testing.T) {
	st := fixtureState()
	ev := ReadEventsFromLines(`{"at": 1, "pkg": null, "from": null, "to": null, "event": "stop", "detail": "x\ny"}`,
		`{"at": 2, "pkg": "a", "from": "pending", "to": "implementing", "event": "status", "detail": ""}`)
	got := RenderStatus(&st, 1000, RenderOptions{
		Events: ev, ApprovalsPending: 2, StatePath: "s.json", PlanTitles: map[string]string{"a": "Title A"},
	})
	want := strings.Join([]string{
		"wave 2  pending=1 implementing=1 verifying=1 integrated=1 needs=1 failed=1  running: 2 in flight  stopped: by test",
		"  a  implementing  1m00s  pi  -  1  Title A",
		"  f  verifying  0m00s  -  -  0",
		"  b  integrated  15m00s  claude  3  2",
		"  d  needs  15m50s  -  -  0",
		"  c  failed  0m00s  -  0  0",
		"needs:",
		"  d  scope  wider",
		"  (wave)  environment  wave",
		"network denied:",
		"  f  h:443",
		"approvals: 2 pending — agent-exec approvals --state s.json",
		"recent:",
		"  -  stop  x",
		"  a  status  pending->implementing",
	}, "\n")
	// b's executor is unset in the fixture: "-" not "claude".
	want = strings.Replace(want, "  b  integrated  15m00s  claude  3  2", "  b  integrated  15m00s  -  3  2", 1)
	if got != want {
		t.Fatalf("render differs:\n%s\n---\n%s", got, want)
	}
}

func ReadEventsFromLines(lines ...string) []*jsonx.Object {
	var out []*jsonx.Object
	for _, l := range lines {
		doc, err := jsonx.Decode([]byte(l))
		if err != nil {
			panic(err)
		}
		out = append(out, doc.(*jsonx.Object))
	}
	return out
}

func TestElapsedFormat(t *testing.T) {
	for in, want := range map[float64]string{-5: "0m00s", 0: "0m00s", 59.9: "0m59s", 3599: "59m59s", 3600: "1h00m", 7384: "2h03m"} {
		if got := formatElapsed(in); got != want {
			t.Errorf("formatElapsed(%v) = %s, want %s", in, got, want)
		}
	}
}

func TestStatusLine(t *testing.T) {
	st := fixtureState()
	got := StatusLine(&st, 130.9, false)
	want := `{"counts": {"failed": 1, "implementing": 1, "integrated": 1, "needs": 1, "pending": 1, "verifying": 1}, "in_flight": 2, "needs": 2, "stopped": true, "updated_ago": 30, "wave": 2}`
	if got != want {
		t.Fatalf("%s", got)
	}
	if strings.Contains(got, "\n") {
		t.Fatal("must be one line")
	}
	if line := StatusLine(&st, 130.9, true); !strings.Contains(line, `"live_changes": {}`) {
		t.Fatalf("live: %s", line)
	}
}

func TestFirstLine(t *testing.T) {
	for in, want := range map[string]string{"": "", "a": "a", "a\nb": "a", "a\r\nb": "a", "\nb": "", "a b": "a"} {
		if got := firstLine(in); got != want {
			t.Errorf("firstLine(%q) = %q", in, got)
		}
	}
}
