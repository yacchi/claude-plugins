package sandbox

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// homeCase is a throwaway $HOME so grants never touch the real store.
type homeCase struct {
	t          *testing.T
	home, tree string
}

func newHome(t *testing.T) *homeCase {
	t.Helper()
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	tree := filepath.Join(home, "work", "tree")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	return &homeCase{t: t, home: home, tree: tree}
}

func (h *homeCase) h(rel string) string { return filepath.Join(h.home, rel) }

func (h *homeCase) policy(denyRead ...string) *Policy {
	return &Policy{Writable: []string{h.tree}, DenyRead: denyRead}
}

func (h *homeCase) detect(text string, p *Policy) []Denial {
	if p == nil {
		p = h.policy()
	}
	return detectDenials([]string{text}, "", h.tree, p, writableByUser)
}

func denialPaths(ds []Denial) []string {
	out := []string{}
	for _, d := range ds {
		out = append(out, d.Path)
	}
	return out
}

func TestDetectDenialsRealShapes(t *testing.T) {
	h := newHome(t)
	target := h.h(".pub-cache/probe")
	shapes := map[string]string{
		"bsd":      "mkdir: " + target + ": Operation not permitted",
		"go-open":  "go: writing go env config: open " + target + ": operation not permitted",
		"go-mkdir": "go: could not create module cache: mkdir " + target + ": operation not permitted",
		"go-erofs": "mkdir " + target + ": read-only file system",
		"python":   "PermissionError: [Errno 1] Operation not permitted: '" + target + "'",
		"node": "npm error FetchError: Invalid response body while trying to fetch " +
			"https://registry.npmjs.org/left-pad: EPERM: operation not permitted, mkdir '" + target + "'",
		// GNU coreutils under bwrap (EROFS) / Landlock (EACCES), C and UTF-8 locales.
		"touch-c-erofs":     "touch: cannot touch '" + target + "': Read-only file system",
		"touch-c-eacces":    "touch: cannot touch '" + target + "': Permission denied",
		"mkdir-utf8-erofs":  "mkdir: cannot create directory ‘" + target + "’: Read-only file system",
		"touch-utf8-eacces": "touch: cannot touch ‘" + target + "’: Permission denied",
		"bash-redirect":     "bash: line 1: " + target + ": Read-only file system",
	}
	for name, line := range shapes {
		got := h.detect(line, nil)
		if !reflect.DeepEqual(denialPaths(got), []string{target}) || got[0].Op != "write" || got[0].Source != "tool" {
			t.Errorf("%s: %+v", name, got)
		}
	}
}

func TestDetectDenialsSourcesAndFilters(t *testing.T) {
	h := newHome(t)
	got := detectDenials(nil, "mkdir: ../elsewhere: Operation not permitted\n", h.tree, h.policy(), writableByUser)
	if len(got) != 1 || got[0].Path != h.h("work/elsewhere") || got[0].Source != "stderr" {
		t.Errorf("relative stderr denial: %+v", got)
	}
	if got := h.detect("mkdir: "+h.tree+"/x: Operation not permitted", nil); len(got) != 0 {
		t.Errorf("inside the writable set: %+v", got)
	}
	if got := h.detect("Error: permission denied by policy", nil); len(got) != 0 {
		t.Errorf("no path: %+v", got)
	}
	// A read the sandbox never denies, or a root-only dir: not ours.
	if got := h.detect("touch: /private/var/db/x: Permission denied", nil); len(got) != 0 {
		t.Errorf("ordinary permission error: %+v", got)
	}
	if got := h.detect("find: "+h.h("Library/Mail")+": Operation not permitted", nil); len(got) != 0 {
		t.Errorf("read tool: %+v", got)
	}
	ssh := h.h(".ssh")
	got = h.detect("cat: "+ssh+"/id_rsa: Operation not permitted", h.policy(ssh))
	if len(got) != 1 || !got[0].Intentional || got[0].Op != "read" {
		t.Errorf("deny_read hit: %+v", got)
	}
	real := h.h(".pub-cache/a")
	text := "mkdir: " + real + ": Operation not permitted\nmkdir: " + real + "/../a: Operation not permitted\n"
	if got := h.detect(text, nil); len(got) != 1 {
		t.Errorf("dedupe by realpath: %+v", got)
	}
	p := h.policy()
	p.DenyWrite = []string{h.h(".pi/agent/settings.json")}
	p.DenyWriteRegex = []string{RepoConfigRegex(h.tree)}
	got = h.detect("open "+h.h(".pi/agent/settings.json")+": operation not permitted\n"+
		"open "+h.tree+"/a/.claude/orchestra.local.yaml: operation not permitted", p)
	if len(got) != 2 || !got[0].Intentional || !got[1].Intentional {
		t.Errorf("tamper/regex denies: %+v", got)
	}
}

func TestDetectDenialsReadsPiToolEvents(t *testing.T) {
	h := newHome(t)
	target := h.h(".npm/_cacache")
	stdout := `{"type":"message_update"}` + "\n" +
		`{"type":"tool_execution_end","result":{"content":[{"type":"text","text":"mkdir: ` + target +
		`: Operation not permitted"}]}}` + "\nnot json\n"
	got := DetectDenials(Spec{Tree: h.tree, Policy: h.policy()}, stdout, "")
	if !reflect.DeepEqual(denialPaths(got), []string{target}) || got[0].Source != "tool" {
		t.Errorf("%+v", got)
	}
}

func TestGrantFor(t *testing.T) {
	h := newHome(t)
	cases := map[string]string{
		h.h(".cache/uv/archive-v0/x"):         h.h(".cache/uv"),
		h.h(".pub-cache"):                     h.h(".pub-cache"),
		h.h(".pnpm-store/x.txt"):              h.h(".pnpm-store"),
		h.h(".local/share/pnpm/store/v3/pkg"): h.h(".local/share/pnpm/store/v3"),
		h.h(".cache-evil/x"):                  "",
		h.h(".cache/../.ssh/x"):               "",
		h.h(".local/share/mise/installs/x"):   "",
		h.h(".local/share/uv/tools/tool/bin"): "",
		"":                                    "",
	}
	for in, want := range cases {
		if got := grantFor(in, ""); got != want {
			t.Errorf("grantFor(%q) = %q, want %q", in, got, want)
		}
	}
	// A symlink out of a cache root resolves to its target first.
	if err := os.MkdirAll(h.h(".cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(h.h(".ssh"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(h.h(".ssh"), h.h(".cache/x")); err != nil {
		t.Fatal(err)
	}
	if got := grantFor(h.h(".cache/x/authorized_keys"), ""); got != "" {
		t.Errorf("symlinked out of the cache: %q", got)
	}
}

func TestUserGrantRefusalsAndUVTools(t *testing.T) {
	h := newHome(t)
	tools := h.h(".local/share/uv/tools")
	for _, bad := range []string{"/", h.home, h.h(".claude/x"), h.h(".ssh/x"), h.h(".local/share"),
		tools, tools + "/tool", h.h(".local")} {
		if _, err := AddGrantEntry(bad, "user", "", ""); err == nil {
			t.Errorf("user grant on %s accepted", bad)
		}
	}
	if _, err := AddGrantEntry(h.h(".config/x"), "auto", "", ""); err == nil {
		t.Error("ineligible auto grant accepted")
	}
	if _, err := AddGrantEntry(h.h(".config/x"), "nobody", "", ""); err == nil {
		t.Error("unknown source accepted")
	}
	if _, err := AddGrantEntry(h.h(".config/x"), "user", "", ""); err != nil {
		t.Errorf("plain user grant refused: %v", err)
	}
	for _, platform := range []string{"darwin", "linux"} {
		p := buildPolicy(h.tree, "pi", Config{AllowWrite: []string{"~/.local/share"}},
			policyOpts{home: h.home, env: map[string]string{}, platform: platform})
		if !contains(p.DenyWrite, tools) {
			t.Errorf("%s: uv tools not denied: %v", platform, p.DenyWrite)
		}
		ll, _ := landlockPolicy(&Policy{Writable: append(p.Writable, tools+"/x"), Tamper: p.Tamper,
			DenyWrite: p.DenyWrite}, h.tree)
		if contains(ll.Writable, h.h(".local/share")) || contains(ll.Writable, tools+"/x") {
			t.Errorf("landlock keeps a root holding a tamper path: %v", ll.Writable)
		}
	}
}

func TestLearnedStore(t *testing.T) {
	h := newHome(t)
	if got := LearnedPath(); got != h.h(".local/state/claude-orchestra/sandbox-learned.json") {
		t.Errorf("default store %q", got)
	}
	t.Setenv("XDG_STATE_HOME", "relative-state")
	if got := LearnedPath(); got != h.h(".local/state/claude-orchestra/sandbox-learned.json") {
		t.Errorf("relative XDG_STATE_HOME honored: %q", got)
	}
	t.Setenv("XDG_STATE_HOME", h.h("state"))
	if got := LearnedPath(); got != h.h("state/claude-orchestra/sandbox-learned.json") {
		t.Errorf("XDG store %q", got)
	}
	if !contains(TamperPaths(), h.h("state/claude-orchestra")) {
		t.Errorf("active store dir not protected: %v", TamperPaths())
	}
	t.Setenv("XDG_STATE_HOME", "")

	if _, err := AddGrantEntry(h.h(".cache/uv"), "auto", h.h(".cache/uv/x"), ""); err != nil {
		t.Fatal(err)
	}
	l, err := LoadLearned()
	if err != nil || l.Warning != "" || len(l.Grants) != 1 || l.Grants[0].Path != h.h(".cache/uv") ||
		l.Grants[0].Example != h.h(".cache/uv/x") {
		t.Errorf("roundtrip: %+v %v", l, err)
	}
	entries, _ := os.ReadDir(filepath.Dir(LearnedPath()))
	for _, e := range entries {
		if e.Name() != "sandbox-learned.json" && e.Name() != "sandbox-learned.json.lock" {
			t.Errorf("temp file left: %s", e.Name())
		}
	}
	if err := ForgetGrant(h.h(".cache/uv")); err != nil {
		t.Fatal(err)
	}
	if err := ForgetGrant(h.h(".cache/uv")); !errors.Is(err, ErrNotFound) {
		t.Errorf("second forget: %v", err)
	}

	if err := os.WriteFile(LearnedPath(), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err = LoadLearned()
	if err == nil || len(l.Grants) != 0 || !strings.Contains(l.Warning, "unreadable") {
		t.Errorf("corrupt store: %+v %v", l, err)
	}
	buildPolicy(h.tree, "pi", Config{}, policyOpts{home: h.home, env: map[string]string{}}) // still builds

	data := `{"version": 1, "grants": [{"path": "` + h.h(".config") + `", "source": "auto"},` +
		`{"path": "` + h.home + `", "source": "user"}, {"path": "` + h.h(".claude") + `", "source": "user"}]}`
	if err := os.WriteFile(LearnedPath(), []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	l, _ = LoadLearned()
	if len(l.Grants) != 0 || !strings.Contains(l.Warning, "ignored 3 invalid entries") {
		t.Errorf("invalid entries trusted: %+v", l)
	}
}

func TestGrantsJoinEveryBackendPolicy(t *testing.T) {
	h := newHome(t)
	root := h.h(".cache")
	if _, err := AddGrantEntry(root, "auto", "", ""); err != nil {
		t.Fatal(err)
	}
	base := buildPolicy(h.tree, "pi", Config{}, policyOpts{home: h.home, env: map[string]string{}})
	if !contains(base.Writable, root) {
		t.Fatalf("grant not writable: %v", base.Writable)
	}
	_, params := SeatbeltProfile(base)
	found := false
	for _, kv := range params {
		found = found || kv[1] == root
	}
	ll, _ := landlockPolicy(base, h.tree)
	bw := bwrapArgv([]string{"pi"}, h.tree, base, func(string) bool { return true }, isDir)
	if !found || !contains(ll.Writable, root) || !contains(bw, root) {
		t.Errorf("seatbelt %v landlock %v bwrap %v", found, contains(ll.Writable, root), contains(bw, root))
	}
}

func TestDomainRefusals(t *testing.T) {
	newHome(t)
	for _, d := range []string{"", " ", "*", "*.com", "*.dev:443", "1.2.3.4", "10.0.0.1:443",
		"::1", "[::1]:443", "fe80::1", "localhost", "LOCALHOST:8080", "*.localhost", "a b.com",
		"http://x.com", "1.2.3"} {
		if DomainRefusal(d) == "" {
			t.Errorf("%q not refused", d)
		}
		if err := AddDomain(d); err == nil {
			t.Errorf("%q stored", d)
		}
	}
	for _, d := range []string{"registry.npmjs.org", "*.github.com", "pypi.org:443"} {
		if r := DomainRefusal(d); r != "" {
			t.Errorf("%q refused: %s", d, r)
		}
	}
}

func TestDomainsKeptApartFromPathGrants(t *testing.T) {
	h := newHome(t)
	if err := AddGrant(h.h(".cache/uv"), "", "user"); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{"Registry.npmjs.org", "registry.npmjs.org"} {
		if err := AddDomain(d); err != nil {
			t.Fatal(err)
		}
	}
	l, _ := LoadLearned()
	if len(l.Grants) != 1 || len(l.Domains) != 1 || l.Domains[0].Domain != "registry.npmjs.org" {
		t.Fatalf("%+v", l)
	}
	if err := ForgetGrant(h.h(".cache/uv")); err != nil {
		t.Fatal(err)
	}
	if got := LearnedDomains(); !reflect.DeepEqual(got, []string{"registry.npmjs.org"}) {
		t.Errorf("domains lost with a path forget: %v", got)
	}
	if err := ForgetDomain("registry.npmjs.org"); err != nil {
		t.Fatal(err)
	}
	if err := ForgetDomain("registry.npmjs.org"); !errors.Is(err, ErrNotFound) {
		t.Errorf("second forget: %v", err)
	}
	planted := `{"version": 1, "grants": [], "domains": [{"domain": "*"}, {"domain": "ok.example.org"}]}`
	if err := os.WriteFile(LearnedPath(), []byte(planted), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := LearnedDomains(); !reflect.DeepEqual(got, []string{"ok.example.org"}) {
		t.Errorf("planted domain trusted: %v", got)
	}
	if l, _ := LoadLearned(); len(l.Grants) != 0 {
		t.Errorf("domains reached the grants: %+v", l.Grants)
	}
}

func TestParseSrtDenial(t *testing.T) {
	cases := map[string][2]any{
		"[SandboxDebug] Connection blocked to example.com:443": {"example.com", 443},
		"Connection blocked to sub.example.co.uk:8080":         {"sub.example.co.uk", 8080},
		"Connection blocked to [2001:db8::1]:443":              {"2001:db8::1", 443},
		"Connection blocked to 2001:db8::1:443":                {"2001:db8::1", 443},
		"Connection blocked to 10.0.0.1:22":                    {"10.0.0.1", 22},
		"Connection blocked to example.com":                    {"example.com", -1},
		"Connection blocked to example.com:443.":               {"example.com", 443},
	}
	for line, want := range cases {
		host, port, ok := ParseSrtDenial(line)
		if !ok || host != want[0] || port != want[1] {
			t.Errorf("%q: %q %d %v", line, host, port, ok)
		}
	}
	if _, _, ok := ParseSrtDenial("[SandboxDebug] No matching config rule, denying: example.com:443"); ok {
		t.Error("non-blocked line parsed")
	}
}

func TestTamperDeniesComeLast(t *testing.T) {
	h := newHome(t)
	p := buildPolicy(h.tree, "pi", Config{AllowWrite: []string{"~/.claude", "~/.pi"}},
		policyOpts{home: h.home, env: map[string]string{}, platform: "darwin"})
	sbpl, params := SeatbeltProfile(p)
	parts := strings.Split(sbpl, "(deny file-write*")
	last := parts[len(parts)-1]
	for _, path := range []string{LearnedPath(), h.h(".claude/orchestra.yaml"),
		h.h(".pi/agent/extensions"), h.h(".pi/agent/settings.json")} {
		key := ""
		for _, kv := range params {
			if kv[1] == path && strings.HasPrefix(kv[0], "D") {
				key = kv[0]
			}
		}
		if key == "" || !strings.Contains(last, `(param "`+key+`")`) {
			t.Errorf("%s not in the final deny (key %q)", path, key)
		}
	}
	lp := buildPolicy(h.tree, "pi", Config{AllowWrite: []string{"~/.claude"}},
		policyOpts{home: h.home, env: map[string]string{}, platform: "linux"})
	out, limits := landlockPolicy(lp, h.tree)
	if contains(out.Writable, h.h(".claude")) || !contains(out.Writable, h.h(".pi/agent")) ||
		!contains(limits, "pi-config") {
		t.Errorf("landlock: %v %v", out.Writable, limits)
	}
}

func TestExecMainFailsClosed(t *testing.T) {
	h := newHome(t)
	var stderr bytes.Buffer
	ran := false
	deps := execDeps{
		restrict: func([]string) error { t.Error("restricted"); return nil },
		exec:     func([]string) error { ran = true; return nil },
		stderr:   &stderr,
	}
	tools := h.h(".local/share/uv/tools")
	for _, args := range [][]string{
		{"--write", tools + "/x", "--protect", tools, "--", "true"},
		{"--write", h.h(".local"), "--protect", LearnedPath(), "--", "true"},
	} {
		if rc := execMain(args, deps); rc != 126 || ran {
			t.Errorf("%v: rc %d ran %v", args, rc, ran)
		}
	}
	var seen []string
	rc := execMain([]string{"--write", "/w", "--", "pi", "-p"}, execDeps{
		restrict: func(w []string) error { seen = w; return nil },
		exec:     func(a []string) error { seen = append(seen, a...); return nil },
		stderr:   &stderr,
	})
	if rc != 127 || !reflect.DeepEqual(seen, []string{"/w", "pi", "-p"}) {
		t.Errorf("child: rc %d seen %v", rc, seen)
	}
	rc = execMain([]string{"--", "pi"}, execDeps{
		restrict: func([]string) error { return errors.New("nope") },
		exec:     func([]string) error { ran = true; return nil },
		stderr:   &stderr,
	})
	if rc != 126 || ran {
		t.Errorf("restriction failure: rc %d ran %v", rc, ran)
	}
	for _, args := range [][]string{{"pi"}, {"--write", "/w"}, {"--"}} {
		if rc := execMain(args, deps); rc != 2 {
			t.Errorf("%v: rc %d", args, rc)
		}
	}
}

func TestLandlockAttrBytes(t *testing.T) {
	handled := landlockFSRights(6)
	want := make([]byte, 24)
	binary.NativeEndian.PutUint64(want[0:], handled)
	binary.NativeEndian.PutUint64(want[16:], 2)
	if got := rulesetAttrBytes(handled, scopeSignal, 6); !bytes.Equal(got, want) {
		t.Errorf("abi 6 attr %x", got)
	}
	if got := rulesetAttrBytes(landlockFSRights(1), 0, 1); len(got) != 8 ||
		binary.NativeEndian.Uint64(got) != (1<<13)-1 {
		t.Errorf("abi 1 attr %x", got)
	}
	if n := len(rulesetAttrBytes(0, 0, 4)); n != 16 {
		t.Errorf("abi 4 size %d", n)
	}
	rule := pathBeneathBytes(0b1101, 3)
	if len(rule) != 12 || binary.NativeEndian.Uint64(rule) != 0b1101 ||
		binary.NativeEndian.Uint32(rule[8:]) != 3 {
		t.Errorf("rule %x", rule)
	}
	rules := landlockRules([]string{"/w"}, handled)
	if len(rules) != 2 || rules[0].access != fsExecute|fsReadFile|fsReadDir || rules[1].access != handled {
		t.Errorf("rules %+v", rules)
	}
}
