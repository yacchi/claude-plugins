package approvals

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/herdr"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

type fixture struct {
	t               *testing.T
	tmp, home, wave string
	statePath       string
	store           *state.Store
	cfg             *config.Resolved
}

func newFixture(t *testing.T) *fixture {
	tmp, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, tmp: tmp, home: filepath.Join(tmp, "home"), wave: filepath.Join(tmp, "wave")}
	_ = os.MkdirAll(filepath.Join(f.home, ".claude"), 0o755)
	_ = os.MkdirAll(f.wave, 0o755)
	t.Setenv("HOME", f.home)
	t.Setenv("XDG_STATE_HOME", filepath.Join(tmp, "xs"))
	f.statePath = filepath.Join(f.wave, "state.json")
	f.store = &state.Store{Path: f.statePath, Clock: func() float64 { return 1000 }}
	if _, err := f.store.Init(filepath.Join(f.wave, "plan.json"), []string{"A", "B"}, "wave-int"); err != nil {
		t.Fatal(err)
	}
	// The network config of the tests: two allowed patterns, one quiet host.
	values := config.Defaults()
	net := values.Object("sandbox").Object("network")
	net.Set("allowed_domains", []any{"api.anthropic.com", "*.pypi.org"})
	net.Set("quiet_domains", []any{"statsig.anthropic.com"})
	f.cfg = &config.Resolved{Values: values}
	return f
}

func (f *fixture) ids() []string {
	items, err := PendingIn(f.statePath, f.cfg)
	if err != nil {
		f.t.Fatal(err)
	}
	out := []string{}
	for _, it := range items {
		out = append(out, it.ID)
	}
	return out
}

func (f *fixture) decide(id, decision, reason string, env ...Env) (map[string]any, error) {
	e := Env{Cfg: f.cfg}
	if len(env) > 0 {
		e = env[0]
		e.Cfg = f.cfg
	}
	return DecideIn(e, f.statePath, id, decision, reason, "instructor")
}

func (f *fixture) audit() []map[string]any {
	raw, _ := os.ReadFile(DecisionsAuditPath())
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func (f *fixture) needs() []string {
	var out []string
	for _, n := range f.store.Snapshot().Needs {
		out = append(out, *n.ID+":"+n.Kind)
	}
	return out
}

func (f *fixture) denials(pid string, hosts ...string) {
	var list []any
	for _, h := range hosts {
		list = append(list, map[string]any{"host": h, "port": int64(443)})
	}
	_ = f.store.UpdatePackage(pid, func(p *state.Package) { p.NetworkDenials = list })
}

func (f *fixture) networkNeed(pid string, hosts ...string) {
	f.denials(pid, hosts...)
	b, _ := json.Marshal(map[string]any{"hosts": hosts, "answer": "x"})
	_ = f.store.AddNeed(pid, "network", string(b))
}

func (f *fixture) permissionNeed(pid, path, candidate string) {
	b, _ := json.Marshal(map[string]any{"denials": []any{map[string]any{"path": path, "grant_candidate": candidate, "why": "not cache"}}})
	_ = f.store.AddNeed(pid, "permission", string(b))
}

func same(t *testing.T, what string, got, want any) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%s = %#v, want %#v", what, got, want)
	}
}

func TestQuietAllowedLearnedAndDeclinedHostsAreNotPending(t *testing.T) {
	f := newFixture(t)
	if err := sandbox.AddDomain("learned.example.com"); err != nil {
		t.Fatal(err)
	}
	f.denials("A", "statsig.anthropic.com", "files.pypi.org", "api.anthropic.com",
		"learned.example.com", "new.example.com", "later.example.com")
	same(t, "ids", f.ids(), []string{"net:later.example.com", "net:new.example.com"})
	if _, err := f.decide("net:new.example.com", "deny", "r"); err != nil {
		t.Fatal(err)
	}
	same(t, "ids", f.ids(), []string{"net:later.example.com"})
}

func TestIDsAreStableAcrossCalls(t *testing.T) {
	f := newFixture(t)
	_ = f.store.UpdatePackage("B", func(p *state.Package) {
		p.NetworkDenials = []any{map[string]any{"host": "b.example.com", "port": int64(443)}}
	})
	_ = f.store.UpdatePackage("A", func(p *state.Package) {
		p.NetworkDenials = []any{map[string]any{"host": "a.example.com", "port": int64(80)},
			map[string]any{"host": "b.example.com", "port": int64(443)}}
	})
	f.permissionNeed("A", "/x/one", "/x/one")
	first, _ := PendingIn(f.statePath, f.cfg)
	second, _ := PendingIn(f.statePath, f.cfg)
	a, _ := jsonx.Marshal(first)
	b, _ := jsonx.Marshal(second)
	if string(a) != string(b) {
		t.Errorf("listing changed between calls")
	}
	same(t, "ids", f.ids(), []string{"net:a.example.com", "net:b.example.com", "path:A:0"})
	same(t, "packages", []string{first[1].Packages[0].ID, first[1].Packages[1].ID}, []string{"A", "B"})
}

func TestRefusedDomainIsNotRecordedAndIsAudited(t *testing.T) {
	f := newFixture(t)
	f.networkNeed("A", "10.0.0.1")
	out, err := f.decide("net:10.0.0.1", "allow", "r")
	var de *DecisionError
	if !errors.As(err, &de) || de.Code != 1 || out["applied"] != false {
		t.Fatalf("decide: %v %v", out, err)
	}
	same(t, "learned", sandbox.LearnedDomains(), []string{})
	same(t, "needs", f.needs(), []string{"A:network"})
	same(t, "ids", f.ids(), []string{"net:10.0.0.1"})
	audit := f.audit()
	if len(audit) != 1 || audit[0]["item"] != "net:10.0.0.1" || audit[0]["applied"] != false {
		t.Errorf("audit %v", audit)
	}
}

func TestAllowResumesOnlyPackagesWaitingOnThatHost(t *testing.T) {
	f := newFixture(t)
	f.networkNeed("A", "x.example.com")
	f.denials("B", "x.example.com")
	_ = f.store.SetStatus("B", "implementing", "", nil)
	out, err := DecideIn(Env{Cfg: f.cfg}, f.statePath, "net:x.example.com", "allow", "r", "user")
	if err != nil {
		t.Fatal(err)
	}
	same(t, "resumed", out["resumed"], []string{"A"})
	same(t, "needs", f.needs(), []string(nil))
	st := f.store.Snapshot()
	same(t, "A", st.Packages["A"].Status, "pending")
	same(t, "B", st.Packages["B"].Status, "implementing")
	l, _ := sandbox.LoadLearned()
	if len(l.Domains) != 1 || l.Domains[0].Domain != "x.example.com" || l.Domains[0].Source != "user" || l.Domains[0].Reason != "r" {
		t.Errorf("learned %+v", l.Domains)
	}
	if f.audit()[0]["applied"] != true {
		t.Error("audit")
	}
}

func TestDenyWritesANoteAndWaitsForTheOtherHost(t *testing.T) {
	f := newFixture(t)
	f.networkNeed("A", "x.example.com", "y.example.com")
	if _, err := f.decide("net:x.example.com", "deny", "unrelated"); err != nil {
		t.Fatal(err)
	}
	same(t, "needs", f.needs(), []string{"A:network"})
	if _, err := f.decide("net:y.example.com", "deny", "r"); err != nil {
		t.Fatal(err)
	}
	same(t, "needs", f.needs(), []string(nil))
	same(t, "A", f.store.Snapshot().Packages["A"].Status, "pending")
	note, _ := os.ReadFile(NotePath(f.wave, "A"))
	if !strings.Contains(string(note), "x.example.com was declined: unrelated") {
		t.Errorf("note %q", note)
	}
	same(t, "learned", sandbox.LearnedDomains(), []string{})
}

func TestPathDecisions(t *testing.T) {
	f := newFixture(t)
	f.permissionNeed("A", filepath.Join(f.home, ".claude", "settings.json"), filepath.Join(f.home, ".claude", "settings.json"))
	_, err := f.decide("path:A:0", "allow", "r")
	var de *DecisionError
	if !errors.As(err, &de) || de.Code != 1 {
		t.Fatalf("refusal: %v", err)
	}
	same(t, "needs", f.needs(), []string{"A:permission"})

	target := filepath.Join(f.home, ".toolcache")
	_ = os.MkdirAll(target, 0o755)
	f.permissionNeed("B", filepath.Join(target, "f"), target)
	if _, err := f.decide("path:B:0", "allow", "r"); err != nil {
		t.Fatal(err)
	}
	l, _ := sandbox.LoadLearned()
	if len(l.Grants) != 1 || l.Grants[0].Path != target || l.Grants[0].Source != "instructor" {
		t.Errorf("grants %+v", l.Grants)
	}
	same(t, "B", f.store.Snapshot().Packages["B"].Status, "pending")

	if _, err := f.decide("path:A:0", "deny", "not needed"); err != nil {
		t.Fatal(err)
	}
	same(t, "needs", f.needs(), []string(nil))
	note, _ := os.ReadFile(NotePath(f.wave, "A"))
	if !strings.Contains(string(note), "settings.json is off-limits: not needed") {
		t.Errorf("note %q", note)
	}
}

type fakeDialog struct {
	sessions map[string]*host.Session
	calls    [][]string
}

func (d *fakeDialog) Session(pkg, _ string) (*host.Session, bool) {
	s, ok := d.sessions[pkg]
	return s, ok
}

func (d *fakeDialog) SendKeys(terminal, key, _ string) error {
	d.calls = append(d.calls, []string{"agent", "send-keys", terminal, key})
	return nil
}

func (f *fixture) dialog(pid, executor string) string {
	tree := filepath.Join(f.tmp, "tree-"+pid)
	_ = os.MkdirAll(tree, 0o755)
	_ = f.store.UpdatePackage(pid, func(p *state.Package) { p.Executor = &executor })
	b, _ := json.Marshal(map[string]any{"orca": "stalled",
		"detail": "dialog:\nAllow write?\nterminal: t-" + pid + "\nworktree: " + tree + "\nresult: r"})
	_ = f.store.AddNeed(pid, "delegate", string(b))
	reg := host.LoadRegistryFile(filepath.Join(f.wave, herdr.RegistryName))
	reg[pid] = map[string]any{"terminal": "t-" + pid, "worktree": tree}
	_ = host.WriteRegistryFile(filepath.Join(f.wave, herdr.RegistryName), reg)
	return tree
}

func TestDialogDecisions(t *testing.T) {
	f := newFixture(t)
	f.dialog("A", "orca")
	d := &fakeDialog{sessions: map[string]*host.Session{}}
	out, err := f.decide("dialog:A", "allow", "r", Env{Dialog: func() DialogHost { return d }})
	var de *DecisionError
	if !errors.As(err, &de) || de.Code != 2 || !strings.Contains(out["error"].(string), "wave mark") || len(d.calls) != 0 {
		t.Fatalf("orca dialog: %v %v %v", out, err, d.calls)
	}
	same(t, "needs", f.needs(), []string{"A:delegate"})

	f2 := newFixture(t)
	tree := f2.dialog("A", "herdr")
	dead := &fakeDialog{sessions: map[string]*host.Session{"A": {Pkg: "A", Worktree: tree}}}
	if _, err := f2.decide("dialog:A", "allow", "r", Env{Dialog: func() DialogHost { return dead }}); !errors.As(err, &de) || de.Code != 2 {
		t.Fatalf("dead terminal: %v", err)
	}
	same(t, "needs", f2.needs(), []string{"A:delegate"})

	for _, c := range []struct{ pid, decision, key string }{{"A", "allow", "enter"}, {"B", "deny", "escape"}} {
		f3 := newFixture(t)
		tree := f3.dialog(c.pid, "herdr")
		live := &fakeDialog{sessions: map[string]*host.Session{c.pid: {Pkg: c.pid, Terminal: "t-" + c.pid, Worktree: tree}}}
		if _, err := f3.decide("dialog:"+c.pid, c.decision, "r", Env{Dialog: func() DialogHost { return live }}); err != nil {
			t.Fatal(err)
		}
		same(t, "keys", live.calls, [][]string{{"agent", "send-keys", "t-" + c.pid, c.key}})
		p := f3.store.Snapshot().Packages[c.pid]
		same(t, "status", []string{p.Status, p.Detail}, []string{"implementing", "await-orca"})
		same(t, "needs", f3.needs(), []string(nil))
	}
}

func TestNetworkEscalateHosts(t *testing.T) {
	type hp = map[string]any
	same(t, "two hosts", NetworkEscalateHosts("ESCALATE: network registry.npmjs.org, `ghcr.io:8443`\nmore"),
		[]map[string]any{hp{"host": "registry.npmjs.org", "port": 443}, hp{"host": "ghcr.io", "port": 8443}})
	same(t, "doubled prefix", NetworkEscalateHosts("ESCALATE: ESCALATE: network a.example.com"),
		[]map[string]any{hp{"host": "a.example.com", "port": 443}})
	for _, plain := range []string{"ESCALATE: spec unclear", "ESCALATE: networking code is broken",
		"ESCALATE: network is down", "done"} {
		if got := NetworkEscalateHosts(plain); got != nil {
			t.Errorf("%q: %v", plain, got)
		}
	}
	same(t, "prose after the list", NetworkEscalateHosts("ESCALATE: ESCALATE: network example.com. curl failed with 403, i.e. "+
		"blocked by the proxy (see docs.example.org)"), []map[string]any{hp{"host": "example.com", "port": 443}})
	same(t, "colon after the list", NetworkEscalateHosts("ESCALATE: ESCALATE: network a.example.com, b.example.net: needed for tests"),
		[]map[string]any{hp{"host": "a.example.com", "port": 443}, hp{"host": "b.example.net", "port": 443}})
	same(t, "url", NetworkEscalateHosts("ESCALATE: network https://Files.Example.com/path"),
		[]map[string]any{hp{"host": "files.example.com", "port": 443}})
}

func TestNoPendingApprovalIsExit2WithoutAudit(t *testing.T) {
	f := newFixture(t)
	out, err := f.decide("net:nope.example.com", "allow", "r")
	var de *DecisionError
	if !errors.As(err, &de) || de.Code != 2 || out["error"] != "no pending approval net:nope.example.com" {
		t.Fatalf("%v %v", out, err)
	}
	if len(f.audit()) != 0 {
		t.Error("an unknown item was audited")
	}
}
