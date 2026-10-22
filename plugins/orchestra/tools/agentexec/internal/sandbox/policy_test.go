package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func gitT(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

// repoWithWorktree is <base>/repo with one commit and a linked worktree
// <base>/wt.
func repoWithWorktree(t *testing.T, base string) (string, string) {
	repo := filepath.Join(base, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "init", "-q")
	if err := os.WriteFile(filepath.Join(repo, "a"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitT(t, repo, "add", "a")
	gitT(t, repo, "commit", "-qm", "i")
	wt := filepath.Join(base, "wt")
	gitT(t, repo, "worktree", "add", "-q", wt)
	return realPath(repo), realPath(wt)
}

func underAny(p string, dirs []string) bool {
	for _, d := range dirs {
		if under(p, d) {
			return true
		}
	}
	return false
}

func tempReal(t *testing.T) string {
	d, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func TestRealPathFollowsPython(t *testing.T) {
	base := tempReal(t)
	target := filepath.Join(base, "elsewhere", "dir")
	if err := os.MkdirAll(target, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, filepath.Join(base, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("loop", filepath.Join(base, "loop")); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		base + "/link/../x":        base + "/elsewhere/x", // the link resolves before ".."
		base + "/missing/../x":     base + "/x",
		base + "/link/new/./a//b/": target + "/new/a/b",
		base + "/loop/x":           base + "/loop/x",
		"/":                        "/",
		"//":                       "/",
		base + "/link/../../" + filepath.Base(base): base + "/" + filepath.Base(base),
	}
	for in, want := range cases {
		if got := realPath(in); got != want {
			t.Errorf("realPath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestBuildPolicyWorktree(t *testing.T) {
	base := tempReal(t)
	home := tempReal(t)
	repo, wt := repoWithWorktree(t, base)
	env := map[string]string{"TMPDIR": "/nonexistent-tmp"}
	p := buildPolicy(wt, "pi", Config{}, policyOpts{home: home, env: env, platform: "linux"})
	common := filepath.Join(repo, ".git")
	var writable []string
	for _, w := range p.Writable {
		if w != "/tmp" && w != "/dev" && w != "/nonexistent-tmp" && !strings.HasPrefix(w, "/private/tmp") {
			writable = append(writable, w)
		}
	}
	for _, want := range []string{wt, filepath.Join(common, "objects"), filepath.Join(common, "worktrees", "wt")} {
		if !contains(writable, want) {
			t.Errorf("%s not writable: %v", want, writable)
		}
	}
	for _, not := range []string{repo, filepath.Join(common, "refs"), filepath.Join(common, "config")} {
		if underAny(not, writable) {
			t.Errorf("%s writable", not)
		}
	}
	main := buildPolicy(repo, "pi", Config{}, policyOpts{home: home, env: env, platform: "linux"})
	if !reflect.DeepEqual(main.Readonly, []string{common}) {
		t.Errorf("checkout carve-out %v", main.Readonly)
	}
	if p.RepoRoot != wt || len(p.DenyWriteRegex) != 1 {
		t.Errorf("repo root %q regex %v", p.RepoRoot, p.DenyWriteRegex)
	}
}

func TestBuildPolicyRealpathsAndPiState(t *testing.T) {
	base := tempReal(t)
	home := tempReal(t)
	real := tempReal(t)
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	p := buildPolicy(link, "pi", Config{}, policyOpts{home: home, env: map[string]string{"TMPDIR": link}, platform: "linux"})
	if p.Writable[0] != real || contains(p.Writable, link) {
		t.Errorf("not realpath'd: %v", p.Writable)
	}
	if !contains(p.Writable, filepath.Join(home, ".pi", "agent")) {
		t.Errorf("pi state dir: %v", p.Writable)
	}
	// A managed agent dir: writable, while orchestra's own state stays denied
	// (srt and Seatbelt let a deny win over every allow).
	t.Setenv("HOME", home)
	t.Setenv("XDG_STATE_HOME", "")
	state := filepath.Join(home, ".local", "state", "claude-orchestra")
	agent := filepath.Join(state, "pi-agent")
	if err := os.MkdirAll(filepath.Join(agent, "sessions"), 0o755); err != nil {
		t.Fatal(err)
	}
	p = buildPolicy(base, "pi", Config{}, policyOpts{home: home,
		env: map[string]string{"PI_CODING_AGENT_DIR": agent}, platform: "linux"})
	if !contains(p.Writable, agent) {
		t.Errorf("managed agent dir not writable: %v", p.Writable)
	}
	srtDeny, _ := SrtSettings(p, nil, nil, nil, false)["filesystem"].(map[string]any)["denyWrite"].([]string)
	for _, deny := range [][]string{p.DenyWrite, srtDeny} {
		if underAny(filepath.Join(agent, "sessions", "s"), deny) {
			t.Errorf("sessions denied: %v", deny)
		}
		for _, want := range []string{filepath.Join(state, "sandbox-learned.json"), filepath.Join(state, "tools")} {
			if !contains(deny, want) {
				t.Errorf("%s not denied", want)
			}
		}
	}
	argv := bwrapArgv([]string{"pi"}, base, p, func(string) bool { return true }, isDir)
	for i, a := range argv {
		if a == "--ro-bind" && argv[i+1] != "/" && under(agent, argv[i+1]) {
			t.Errorf("bwrap re-binds the agent dir read-only via %s", argv[i+1])
		}
	}
}

func TestBackendSelection(t *testing.T) {
	newHome(t)
	tree := tempReal(t)
	no := Probe{Detail: "no"}
	spec := prepare("pi", tree, Config{Mode: "auto"},
		&ProbeReport{Seatbelt: no, Bwrap: no, Landlock: Probe{OK: true, ABI: 4}}, policyOpts{}, nil)
	if spec.Backend != "landlock" || !reflect.DeepEqual(spec.Limits, []string{"deny_read", "signal", "pi-config"}) {
		t.Errorf("landlock fallback: %s %v", spec.Backend, spec.Limits)
	}
	argv := WrapArgv([]string{"pi", "-p"}, tree, spec)
	if argv[1] != "_sandbox-exec" || !reflect.DeepEqual(argv[len(argv)-3:], []string{"--", "pi", "-p"}) {
		t.Errorf("landlock argv %v", argv)
	}
	none := &ProbeReport{Seatbelt: no, Bwrap: no, Landlock: no}
	if s := prepare("pi", tree, Config{Mode: "required"}, none, policyOpts{}, nil); !s.Refuse || s.Policy != nil {
		t.Errorf("required without backend: %+v", s)
	}
	if s := prepare("pi", tree, Config{Mode: "auto"}, none, policyOpts{}, nil); s.Refuse || s.Backend != "none" {
		t.Errorf("auto without backend: %+v", s)
	}
	if s := prepare("pi", tree, Config{Mode: "off"}, nil, policyOpts{}, nil); s.Backend != "none" ||
		!reflect.DeepEqual(WrapArgv([]string{"pi"}, tree, s), []string{"pi"}) {
		t.Errorf("mode off: %+v", s)
	}
	t.Setenv(ForceEnv, "none")
	if r := Probes(); r.Seatbelt.OK || r.Bwrap.OK || r.Landlock.OK {
		t.Errorf("force env ignored: %+v", r)
	}
}

func TestPrepareUnderSrtAndItsFallback(t *testing.T) {
	newHome(t)
	tree := tempReal(t)
	ok := ProbeReport{Seatbelt: Probe{OK: true, Detail: "ok"}, Bwrap: Probe{Detail: "no"}, Landlock: Probe{Detail: "no"}}
	net := &Network{Srt: "/opt/srt", Domains: []string{"api.anthropic.com"}, ID: "t1", Session: "t1"}
	s := prepare("pi", tree, normalizeConfig(nil), &ok, policyOpts{}, net)
	if s.Backend != "srt" || s.Srt == nil || !strings.HasSuffix(s.Srt.SettingsPath, "/cli-srt/t1.json") {
		t.Fatalf("srt spec: %+v", s)
	}
	argv := WrapArgv([]string{"pi", "it's"}, tree, s)
	if !reflect.DeepEqual(argv, []string{"/opt/srt", "-d", "-s", s.Srt.SettingsPath, "--control-fd", "3",
		"-c", `pi 'it'"'"'s'`}) {
		t.Errorf("srt argv %q", argv)
	}
	env := ChildEnv(s, []string{"TMPDIR=/tmp"})
	if !contains(env, "NODE_USE_ENV_PROXY=1") {
		t.Errorf("child env %v", env)
	}
	s = prepare("pi", tree, normalizeConfig(nil), &ok, policyOpts{}, &Network{Reason: "srt not found (x)"})
	if s.Backend != "seatbelt" || !reflect.DeepEqual(s.Limits, []string{"network not restricted: srt not found (x)"}) ||
		!strings.HasSuffix(s.Reason, "; network not restricted: srt not found (x)") {
		t.Errorf("fallback: %+v", s)
	}
}

func TestBwrapArgvCarveOutAndDenyReadOrder(t *testing.T) {
	p := &Policy{Writable: []string{"/r", "/r/.git/objects", "/dev", "/missing"},
		Readonly: []string{"/r/.git"}, DenyRead: []string{"/h/.ssh", "/h/.netrc", "/h/none"}}
	ex := func(p string) bool { return p != "/missing" && p != "/h/none" }
	dir := func(p string) bool { return p != "/h/.netrc" }
	got := bwrapArgv([]string{"pi"}, "/r", p, ex, dir)
	want := []string{"bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc",
		"--bind", "/r", "/r",
		"--ro-bind", "/r/.git", "/r/.git",
		"--bind", "/r/.git/objects", "/r/.git/objects",
		"--tmpfs", "/h/.ssh",
		"--ro-bind", "/dev/null", "/h/.netrc",
		"--unshare-pid", "--die-with-parent", "--chdir", "/r", "--", "pi"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("bwrap argv\n got %q\nwant %q", got, want)
	}
}

func TestChildEnv(t *testing.T) {
	spec := Spec{Backend: "seatbelt", Policy: &Policy{}}
	env := ChildEnv(spec, []string{"TMPDIR=/tmp"})
	if !contains(env, "UV_TOOL_DIR="+filepath.Join(realPath("/tmp"), "orchestra-uv-tools")) {
		t.Errorf("%v", env)
	}
	if env := ChildEnv(spec, []string{"UV_TOOL_DIR=/x"}); len(env) != 1 {
		t.Errorf("explicit UV_TOOL_DIR overridden: %v", env)
	}
	if env := ChildEnv(Spec{Backend: "none"}, nil); len(env) != 0 {
		t.Errorf("unsandboxed child got %v", env)
	}
}

func TestHostPolicy(t *testing.T) {
	base := tempReal(t)
	home := filepath.Join(base, "home")
	shared := filepath.Join(home, ".config", "shared")
	claude := filepath.Join(home, ".claude")
	for _, d := range []string{filepath.Join(shared, "projects"), filepath.Join(shared, "skills"), claude} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(shared, "history.jsonl"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"projects", "history.jsonl", "skills"} {
		if err := os.Symlink(filepath.Join(shared, n), filepath.Join(claude, n)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	repo, _ := repoWithWorktree(t, base)
	wtBase := repo + "-orchestra"
	plugins := filepath.Join(claude, "plugins")
	if err := os.MkdirAll(filepath.Join(plugins, "data", "other-plugin"), 0o755); err != nil {
		t.Fatal(err)
	}
	none := []Grant{}
	userCache := filepath.Join(base, "var-folders", "C")
	o := policyOpts{home: home, env: map[string]string{"TMPDIR": "/nonexistent-tmp"}, platform: "darwin", learned: &none,
		userCache: userCache}
	p := hostPolicy(HostInput{Repo: repo, WorktreeBase: wtBase, HerdrHome: filepath.Join(home, "hx")}, Config{}, o)
	for _, w := range []string{claude, filepath.Join(shared, "projects"), filepath.Join(shared, "history.jsonl"),
		wtBase, filepath.Join(home, ".npm"), filepath.Join(home, "Library", "Caches"), filepath.Join(userCache, "mds")} {
		if !contains(p.Writable, w) {
			t.Errorf("%s not writable", w)
		}
	}
	if underAny(repo, p.Writable) {
		t.Error("the user's own tree is writable")
	}
	for _, d := range []string{filepath.Join(claude, "settings.json"), filepath.Join(claude, "hooks"),
		filepath.Join(shared, "skills"), filepath.Join(StateDir(), "tools"), filepath.Join(StateDir(), "pi-agent")} {
		if !contains(p.DenyWrite, d) {
			t.Errorf("%s not denied", d)
		}
	}
	if contains(p.DenyRead, filepath.Join(home, "Library", "Keychains")) || !contains(p.DenyRead, filepath.Join(home, ".ssh")) {
		t.Errorf("host deny_read %v", p.DenyRead)
	}
	inline := filepath.Join(plugins, "data", "orchestra-inline")
	var underPlugins []string
	for _, w := range p.Writable {
		if under(w, plugins) {
			underPlugins = append(underPlugins, w)
		}
	}
	if !reflect.DeepEqual(underPlugins, []string{inline}) || !contains(p.Readonly, plugins) {
		t.Errorf("plugins carve-out: %v %v", underPlugins, p.Readonly)
	}
	fs := SrtSettings(p, nil, nil, nil, true)["filesystem"].(map[string]any)
	deny := fs["denyWrite"].([]string)
	if underAny(inline, deny) {
		t.Errorf("inline plugin dir denied under srt: %v", deny)
	}
	for _, d := range []string{filepath.Join(plugins, "data", "other-plugin"), filepath.Join(plugins, "installed_plugins.json")} {
		if !contains(deny, d) {
			t.Errorf("%s not denied under srt", d)
		}
	}
	sbpl, _ := SeatbeltProfile(p)
	if strings.Index(sbpl, `claude\.json`) > strings.LastIndex(sbpl, "(deny file-write*") {
		t.Error("the ~/.claude.json allow comes after the tamper denies")
	}
	results := ProbeReport{Seatbelt: Probe{Detail: "no"}, Bwrap: Probe{Detail: "no"}, Landlock: Probe{OK: true, ABI: 6}}
	in := HostInput{Repo: repo, WorktreeBase: wtBase, HerdrHome: filepath.Join(home, "hx")}
	if s := prepareHost(Config{Mode: "auto"}, in, &results, o); s.Enforced || s.Refuse {
		t.Errorf("host on landlock only: %+v", s)
	}
	if s := prepareHost(Config{Mode: "required"}, in, &results, o); !s.Refuse {
		t.Errorf("required host without seatbelt/bwrap: %+v", s)
	}
}

// The pnpm store both policies seed is the one ChildEnv pins, whichever
// of PNPM_HOME / XDG_DATA_HOME / the platform default decides it.
func TestPnpmPinIsTheSeededStore(t *testing.T) {
	base := tempReal(t)
	home := filepath.Join(base, "home")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)
	none := []Grant{}
	cases := []struct {
		platform string
		env      map[string]string
		want     string
	}{
		{"darwin", map[string]string{}, filepath.Join(home, "Library", "pnpm", "store")},
		{"linux", map[string]string{}, filepath.Join(home, ".local", "share", "pnpm", "store")},
		{"darwin", map[string]string{"PNPM_HOME": filepath.Join(base, "ph"), "XDG_DATA_HOME": filepath.Join(base, "xdg")},
			filepath.Join(base, "ph", "store")},
		{"linux", map[string]string{"XDG_DATA_HOME": filepath.Join(base, "xdg")}, filepath.Join(base, "xdg", "pnpm", "store")},
	}
	for _, c := range cases {
		o := policyOpts{home: home, env: c.env, platform: c.platform, learned: &none}
		cli := buildPolicy(filepath.Join(base, "tree"), "codex", Config{}, o)
		host := hostPolicy(HostInput{Repo: filepath.Join(base, "repo"), WorktreeBase: filepath.Join(base, "wt"),
			HerdrHome: filepath.Join(base, "hx")}, Config{}, o)
		var env []string
		for k, v := range c.env {
			env = append(env, k+"="+v)
		}
		for name, p := range map[string]*Policy{"cli": cli, "host": host} {
			if p.PnpmStore != c.want || !contains(p.Writable, c.want) {
				t.Errorf("%s %v %s: store %q, writable %v; want %q seeded", c.platform, c.env, name, p.PnpmStore, p.Writable, c.want)
			}
			for _, w := range []string{filepath.Join(home, ".cargo", "registry"), filepath.Join(home, ".bun", "install", "cache")} {
				if !contains(p.Writable, w) {
					t.Errorf("%s: %s not seeded", name, w)
				}
			}
			got := ChildEnv(Spec{Backend: "seatbelt", Policy: p}, env)
			if !contains(got, "pnpm_config_store_dir="+c.want) {
				t.Errorf("%s %v %s: child env %v lacks pnpm_config_store_dir=%s", c.platform, c.env, name, got, c.want)
			}
			for _, kv := range got {
				if strings.HasPrefix(kv, "npm_config_store_dir=") {
					t.Errorf("%s %v %s: child env sets %s (npm rejects the key)", c.platform, c.env, name, kv)
				}
			}
		}
	}
}

func TestChildEnvKeepsAnExplicitPnpmStore(t *testing.T) {
	spec := Spec{Backend: "seatbelt", Policy: &Policy{PnpmStore: "/seeded"}}
	pins := func(env []string) []string {
		var out []string
		for _, kv := range ChildEnv(spec, env)[len(env):] {
			if strings.Contains(strings.ToLower(kv), "store_dir") {
				out = append(out, kv)
			}
		}
		return out
	}
	cases := []struct {
		env, want []string
	}{
		{[]string{"pnpm_config_store_dir=/mine"}, nil},
		{[]string{"npm_config_store_dir=/mine"}, []string{"pnpm_config_store_dir=/mine"}},
		{[]string{"NPM_CONFIG_STORE_DIR=/mine"}, []string{"pnpm_config_store_dir=/mine"}},
		{[]string{"pnpm_config_store_dir=/a", "npm_config_store_dir=/b"}, nil},
	}
	for _, c := range cases {
		if got := pins(c.env); !reflect.DeepEqual(got, c.want) {
			t.Errorf("env %v: pinned %v, want %v", c.env, got, c.want)
		}
	}
	unsandboxed := Spec{Backend: "none", Policy: &Policy{PnpmStore: "/seeded"}}
	if env := ChildEnv(unsandboxed, nil); len(env) != 0 {
		t.Errorf("unsandboxed child pinned: %v", env)
	}
}

// A seed is a cache, never a toolchain home whose executables run later
// outside any sandbox.
func TestSeedsHoldNoExecutables(t *testing.T) {
	home := "/h"
	execs := []string{"~/.cargo", "~/.cargo/bin", "~/Library/pnpm", "~/Library/pnpm/bin", "~/Library/pnpm/global",
		"~/.local/share/pnpm", "~/.bun", "~/.bun/bin", "~/.bun/install/global", "~/.pub-cache", "~/.pub-cache/bin",
		"~/.gradle", "~/.gradle/wrapper", "~/.local/share/mise", "~/.local/share/uv/tools", "~/go", "~/go/bin"}
	for _, platform := range []string{"darwin", "linux"} {
		for _, env := range []map[string]string{{}, {"PNPM_HOME": "/h/Library/pnpm"}, {"PNPM_HOME": "/h/.local/share/pnpm"}} {
			seeded, _ := seeds(platform, home, func(k string) string { return env[k] })
			for _, s := range seeded {
				for _, e := range execs {
					if under(expand(e, home), s) {
						t.Errorf("%s %v: seed %s holds %s", platform, env, s, e)
					}
				}
			}
		}
	}
}
