package watchdog

import (
	"encoding/json"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/procx"
)

// fixtures is testdata/fixtures.json: Watchdog runs driven by a fake clock,
// first recorded from the retired Python implementation. Expected values
// are edited by hand when the behavior intentionally changes.
type fixtures struct {
	Cases []struct {
		Name        string          `json:"name"`
		Config      json.RawMessage `json:"config"`
		Cls         string          `json:"cls"`
		Ops         []op            `json:"ops"`
		Checks      []*float64      `json:"checks"`
		ToolRunning bool            `json:"tool_running"`
		Result      json.RawMessage `json:"result"`
	} `json:"cases"`
}

type op struct {
	Op   string  `json:"op"`
	Text string  `json:"text"`
	S    float64 `json:"s"`
}

func plain(t *testing.T, v any) any {
	t.Helper()
	raw, err := fsutil.MarshalJSON(v)
	if err != nil {
		t.Fatal(err)
	}
	var out any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func TestWatchdogFixtures(t *testing.T) {
	raw, err := os.ReadFile("testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var fx fixtures
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatal(err)
	}
	if len(fx.Cases) == 0 {
		t.Fatal("no fixtures")
	}
	for _, c := range fx.Cases {
		t.Run(c.Name, func(t *testing.T) {
			sec, err := fsutil.DecodeJSON(c.Config)
			if err != nil {
				t.Fatal(err)
			}
			now := 100.0
			w := newWithClock(ConfigFrom(sec.(map[string]any)), c.Cls, func() float64 { return now })
			var checks []*float64
			for _, o := range c.Ops {
				switch o.Op {
				case "line":
					w.OnLine(o.Text)
				case "advance":
					now += o.S
				case "check":
					w.mu.Lock()
					rem, fired := w.check(now)
					w.mu.Unlock()
					if fired {
						checks = append(checks, nil)
					} else {
						checks = append(checks, &rem)
					}
				}
			}
			if len(checks) != len(c.Checks) {
				t.Fatalf("%d checks, want %d", len(checks), len(c.Checks))
			}
			for i := range checks {
				g, want := checks[i], c.Checks[i]
				if (g == nil) != (want == nil) || (g != nil && math.Abs(*g-*want) > 1e-9) {
					t.Errorf("check %d: got %v want %v", i, deref(g), deref(want))
				}
			}
			if w.ToolRunning() != c.ToolRunning {
				t.Errorf("tool_running %v, want %v", w.ToolRunning(), c.ToolRunning)
			}
			var want any
			if err := json.Unmarshal(c.Result, &want); err != nil {
				t.Fatal(err)
			}
			var got any
			if r := w.Result(); r != nil {
				got = plain(t, r.Object())
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("result\n got  %v\n want %v", got, want)
			}
		})
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestConfigFromDefaults(t *testing.T) {
	c := ConfigFrom(map[string]any{})
	if !c.Enabled || c.Idle != DefaultIdle || c.ToolIdle != DefaultToolIdle ||
		c.RepeatLimit != DefaultRepeatLimit || c.Wall != nil {
		t.Fatalf("defaults %+v", c)
	}
	c = ConfigFrom(map[string]any{
		"enabled":      "yes",
		"repeat_limit": "x",
		"idle_seconds": 0.5,
	})
	if c.Enabled || c.RepeatLimit != 0 || c.Idle != 500*time.Millisecond {
		t.Fatalf("odd values %+v", c)
	}
}

// A tripped watchdog ends the whole process group, grandchildren included.
func TestStartTerminatesProcessGroup(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "grandchild.pid")
	grandchild, pidExpr := "sleep 30", "$!"
	if runtime.GOOS == "windows" {
		// In Git Bash $! is an MSYS pid; /proc/<pid>/winpid maps it to the
		// Windows pid fsutil.PIDAlive understands. The grandchild is a native
		// program: an MSYS one (sleep) is exec'd by a fork stub that exits,
		// leaving it parentless in the Windows process table, whereas the
		// stub stays as the parent of a native one, as for real workers.
		grandchild, pidExpr = "ping -n 30 127.0.0.1 >/dev/null", "$(cat /proc/$!/winpid)"
	}
	cmd := exec.Command(procx.ShellPath(), "-c",
		grandchild+" & echo "+pidExpr+" > '"+filepath.ToSlash(marker)+"'; wait")
	procx.Detach(cmd)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	// The budget runs from w.Start, so start it once the grandchild exists:
	// a shell slower to start than the budget (Git Bash) would otherwise be
	// killed before it forks.
	var pid int
	for i := 0; ; i++ {
		raw, err := os.ReadFile(marker)
		if err == nil && json.Unmarshal(raw, &pid) == nil && pid > 0 {
			break
		}
		if i == 500 {
			t.Fatalf("grandchild never started: %v", err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cfg := DefaultConfig()
	cfg.Wall = map[string]time.Duration{"standard": 300 * time.Millisecond}
	w := New(cfg, "no-such-class")
	start := time.Now()
	w.Start(func() { TerminateGroup(cmd.Process.Pid, exited) })
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("child survived the wall budget")
	}
	w.Stop()
	if d := time.Since(start); d < 300*time.Millisecond || d > 2*time.Second {
		t.Errorf("tripped after %v", d)
	}
	r := w.Result()
	if r == nil || r.Reason != "wall" || r.RuntimeS < 0.3 || r.RuntimeS > 1.0 {
		t.Fatalf("result %+v", r)
	}
	for i := 0; i < 50; i++ {
		if !fsutil.PIDAlive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("grandchild survived process-group termination")
}

func TestDisabledNeverStartsTimer(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = false
	cfg.Idle = time.Millisecond
	w := New(cfg, "standard")
	w.Start(func() { t.Error("terminated while disabled") })
	time.Sleep(50 * time.Millisecond)
	w.Stop()
	if r := w.Result(); r != nil {
		t.Fatalf("result %+v", r)
	}
}

func TestCanonicalNumbersCompareByValue(t *testing.T) {
	pairs := [][2]string{
		{"1", "1.0"},
		{"1000000", "1000000.0"},
		{"1e16", "10000000000000000"},
		{"0", "-0.0"},
		{"-0", "0.0"},
		{"2.5e0", "2.5"},
	}
	for _, p := range pairs {
		a, _ := decodeObject(`{"n": ` + p[0] + `}`)
		b, _ := decodeObject(`{"n": ` + p[1] + `}`)
		if Canonical(a) != Canonical(b) {
			t.Errorf("%s vs %s: %s != %s", p[0], p[1], Canonical(a), Canonical(b))
		}
	}
	if got := Canonical(map[string]any{"n": json.Number("1e-05")}); got != `{"n":1e-05}` {
		t.Errorf("small float = %s", got)
	}
}
