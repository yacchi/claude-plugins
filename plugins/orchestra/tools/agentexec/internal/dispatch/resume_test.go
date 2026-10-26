package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// resumeGatePi is a stand-in pi: it appends its argv to argv.log beside it,
// echoes the --session id (or sid-fresh for a new session) and reports the
// last request's context as usage, or no usage at all when usage is "".
const resumeGatePi = `#!/bin/sh
dir=$(dirname "$0")
printf '%%s\n' "$*" >> "$dir/argv.log"
sid=sid-fresh
prev=
for a in "$@"; do
  if [ "$prev" = "--session" ]; then sid="$a"; fi
  prev="$a"
done
cat >/dev/null
echo "{\"type\":\"session\",\"id\":\"$sid\"}"
echo '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"DONE"}]%s,"stopReason":"stop"}}'
`

// resumeGateRun dispatches one routed standard-class task through the fake
// pi, which reports usage as the context its last request re-read (usage ""
// = none). It returns the result and the argv the fake pi was started with.
func resumeGateRun(t *testing.T, cfg *config.Resolved, wd, usage string, req Request) (map[string]any, string) {
	t.Helper()
	dir := t.TempDir()
	usageJSON := ""
	if usage != "" {
		usageJSON = fmt.Sprintf(`,"usage":{"input":%s,"output":0,"cacheRead":0,"cacheWrite":0}`, usage)
	}
	script := fmt.Sprintf(resumeGatePi, usageJSON)
	if err := os.WriteFile(filepath.Join(dir, "pi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	req.Class, req.Task, req.RunID, req.Workdir = "standard", "t-1", "run-1", wd
	req.Isolate, req.NoCooldown, req.NoUsage = "never", true, true
	req.Exhausted = []string{"claude"}
	out, code := Run(context.Background(), cfg, req)
	if code != 0 || out["status"] != "ok" {
		t.Fatalf("dispatch exit %d, output %v", code, out)
	}
	log, err := os.ReadFile(filepath.Join(dir, "argv.log"))
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.TrimSpace(string(log))
	return out, argv[strings.LastIndex(argv, "\n")+1:]
}

// resumeGateConfig is the defaults (sandbox off, the ledger and session store
// under a temp dir) with enforcement.resume_max_tokens set to limit.
func resumeGateConfig(t *testing.T, limit any) (*config.Resolved, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	// Readiness only lists pi as routable when the agent-exec permission rule is granted.
	mustWrite(t, filepath.Join(home, ".claude", "settings.json"),
		`{"permissions":{"allow":["Bash(agent-exec:*)"]}}`)
	cfg := testConfig(t)
	config.ObjectAt(cfg.Values, "sandbox")["mode"] = "off"
	config.ObjectAt(cfg.Values, "enforcement")["resume_max_tokens"] = limit
	return cfg, t.TempDir()
}

func TestResumeGateLongStoredContextStartsFresh(t *testing.T) {
	cfg, wd := resumeGateConfig(t, int64(100000))
	first, _ := resumeGateRun(t, cfg, wd, "130000", Request{})
	if first["resumable"] != false || first["context_tokens"] != int64(130000) {
		t.Fatalf("long round 1: resumable %v, context_tokens %v", first["resumable"], first["context_tokens"])
	}
	second, argv := resumeGateRun(t, cfg, wd, "130000", Request{})
	if second["resumed"] != false || strings.Contains(argv, "--session") {
		t.Fatalf("correction over a long context must start fresh: resumed %v, argv %q", second["resumed"], argv)
	}
}

func TestResumeGateShortStoredContextResumes(t *testing.T) {
	cfg, wd := resumeGateConfig(t, int64(100000))
	first, _ := resumeGateRun(t, cfg, wd, "40000", Request{})
	if first["resumable"] != true || first["context_tokens"] != int64(40000) {
		t.Fatalf("short round 1: resumable %v, context_tokens %v", first["resumable"], first["context_tokens"])
	}
	second, _ := resumeGateRun(t, cfg, wd, "40000", Request{})
	if second["resumed"] != true {
		t.Fatalf("correction over a short context must resume, got resumed %v", second["resumed"])
	}
}

func TestResumeGateExplicitResumeIgnoresLimit(t *testing.T) {
	cfg, wd := resumeGateConfig(t, int64(100000))
	first, _ := resumeGateRun(t, cfg, wd, "130000", Request{})
	second, _ := resumeGateRun(t, cfg, wd, "130000", Request{Resume: first["session_id"].(string)})
	if second["resumed"] != true {
		t.Fatalf("explicit --resume must resume a long session, got resumed %v", second["resumed"])
	}
}

func TestResumeGateUnknownContextIsResumable(t *testing.T) {
	cfg, wd := resumeGateConfig(t, int64(100000))
	first, _ := resumeGateRun(t, cfg, wd, "", Request{})
	if first["context_tokens"] != nil || first["resumable"] != true {
		t.Fatalf("unknown context: context_tokens %v, resumable %v", first["context_tokens"], first["resumable"])
	}
	second, _ := resumeGateRun(t, cfg, wd, "", Request{})
	if second["resumed"] != true {
		t.Fatalf("unknown context must still resume, got resumed %v", second["resumed"])
	}
}

func TestResumeGateLimitOffAlwaysResumes(t *testing.T) {
	cfg, wd := resumeGateConfig(t, "off")
	first, _ := resumeGateRun(t, cfg, wd, "130000", Request{})
	if first["resumable"] != true {
		t.Fatalf("limit off: resumable %v", first["resumable"])
	}
	second, _ := resumeGateRun(t, cfg, wd, "130000", Request{})
	if second["resumed"] != true {
		t.Fatalf("limit off must auto-resume, got resumed %v", second["resumed"])
	}
}

func TestResumeGateLatestRunDecidesTheStore(t *testing.T) {
	cfg, wd := resumeGateConfig(t, int64(100000))
	resumeGateRun(t, cfg, wd, "130000", Request{})
	// The correction starts fresh; its short run makes the task resumable.
	resumeGateRun(t, cfg, wd, "40000", Request{})
	third, _ := resumeGateRun(t, cfg, wd, "40000", Request{})
	if third["resumed"] != true {
		t.Fatalf("a later short run must make the task resumable, got resumed %v", third["resumed"])
	}
}
