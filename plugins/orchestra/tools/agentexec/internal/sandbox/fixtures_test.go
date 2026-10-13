package sandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// fixtures is testdata/sbpl/fixtures.json, generated from the Python
// implementation by gen_fixtures.py (never edited by hand).
type fixtures struct {
	Argv    []string         `json:"argv"`
	Now     string           `json:"now"`
	Cases   []fixtureCase    `json:"cases"`
	Learned []learnedFixture `json:"learned"`
}

type fixtureCase struct {
	Name    string            `json:"name"`
	Kind    string            `json:"kind"`
	Layout  []layoutOp        `json:"layout"`
	Tree    string            `json:"tree"`
	Cwd     string            `json:"cwd"`
	Cfg     json.RawMessage   `json:"cfg"`
	Env     map[string]string `json:"env"`
	Network *struct {
		Srt     *string  `json:"srt"`
		Reason  string   `json:"reason"`
		ID      string   `json:"id"`
		Session string   `json:"session"`
		Domains []string `json:"domains"`
	} `json:"network"`
	Host *struct {
		Repo          string   `json:"repo"`
		WorktreeBase  string   `json:"worktree_base"`
		HerdrHome     string   `json:"herdr_home"`
		ExtraWritable []string `json:"extra_writable"`
	} `json:"host"`
	Want struct {
		Backend         string         `json:"backend"`
		Reason          string         `json:"reason"`
		Limits          []string       `json:"limits"`
		Refuse          bool           `json:"refuse"`
		Argv            []string       `json:"argv"`
		SrtSettings     map[string]any `json:"srt_settings"`
		SrtSettingsPath string         `json:"srt_settings_path"`
	} `json:"want"`
}

type layoutOp struct {
	Op     string   `json:"op"`
	Path   string   `json:"path"`
	Data   string   `json:"data"`
	Target string   `json:"target"`
	Cwd    string   `json:"cwd"`
	Args   []string `json:"args"`
}

type learnedFixture struct {
	After   string `json:"after"`
	Content string `json:"content"`
}

func loadFixtures(t *testing.T) fixtures {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", "sbpl", "fixtures.json"))
	if err != nil {
		t.Fatal(err)
	}
	var f fixtures
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatal(err)
	}
	return f
}

// fixtureRoot is a fresh realpath'd root with HOME inside it and no
// XDG_STATE_HOME, as the generator ran.
func fixtureRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", filepath.Join(root, "home"))
	t.Setenv("XDG_STATE_HOME", "")
	os.Unsetenv("XDG_STATE_HOME")
	return root
}

func buildLayout(t *testing.T, root string, ops []layoutOp) {
	t.Helper()
	sub := func(s string) string { return strings.ReplaceAll(s, "<ROOT>", root) }
	for _, op := range ops {
		path := filepath.Join(root, op.Path)
		var err error
		switch op.Op {
		case "mkdir":
			err = os.MkdirAll(path, 0o755)
		case "write":
			if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				err = os.WriteFile(path, []byte(sub(op.Data)), 0o644)
			}
		case "symlink":
			if err = os.MkdirAll(filepath.Dir(path), 0o755); err == nil {
				err = os.Symlink(sub(op.Target), path)
			}
		case "git":
			args := make([]string, len(op.Args))
			for i, a := range op.Args {
				args[i] = sub(a)
			}
			cmd := exec.Command("git", args...)
			cmd.Dir = filepath.Join(root, op.Cwd)
			cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
				"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t",
				"GIT_COMMITTER_EMAIL=t@t")
			if out, gerr := cmd.CombinedOutput(); gerr != nil {
				t.Fatalf("git %v: %v\n%s", args, gerr, out)
			}
		default:
			t.Fatalf("unknown layout op %q", op.Op)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestLearnedStoreBytesMatchPython(t *testing.T) {
	f := loadFixtures(t)
	root := fixtureRoot(t)
	home := filepath.Join(root, "home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	old := nowISOZ
	nowISOZ = func() string { return f.Now }
	t.Cleanup(func() { nowISOZ = old })

	check := func(i int, label string) {
		t.Helper()
		if f.Learned[i].After != label {
			t.Fatalf("fixture step %d is %q, want %q", i, f.Learned[i].After, label)
		}
		got, err := os.ReadFile(LearnedPath())
		if err != nil {
			t.Fatal(err)
		}
		want := strings.ReplaceAll(f.Learned[i].Content, "<ROOT>", root)
		if string(got) != want {
			t.Errorf("after %s:\n got %q\nwant %q", label, got, want)
		}
	}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	x := filepath.Join(home, ".cache/uv/archive/x")
	_, err := AddGrantEntry(x, "auto", x, "")
	must(err)
	check(0, "auto")
	_, err = AddGrantEntry(filepath.Join(home, "Données ü"), "user", "", "naïve – ünïcode")
	must(err)
	check(1, "user")
	_, err = AddDomainEntry(" Registry.NPMJS.org ", "user", " npm ")
	must(err)
	check(2, "domain")
	must(AddGrant(filepath.Join(home, "approved"), "approved", "instructor"))
	must(ForgetGrant(x))
	check(3, "forget")
	must(ForgetDomain("registry.npmjs.org"))
	check(4, "forget-domain")
	if st, err := os.Stat(LearnedPath()); err != nil || st.Mode().Perm() != 0o600 {
		t.Errorf("store mode: %v %v", st, err)
	}
}

// replayCase builds the case's layout and returns what Go makes of it,
// with the root spelled <ROOT>.
func replayCase(t *testing.T, f fixtures, c fixtureCase) (Spec, []string, map[string]any, string) {
	root := fixtureRoot(t)
	buildLayout(t, root, c.Layout)
	sub := func(s string) string { return strings.ReplaceAll(s, "<ROOT>", root) }
	env := map[string]string{}
	for k, v := range c.Env {
		env[k] = sub(v)
	}
	cfgRaw, err := jsonx.Decode([]byte(sub(string(c.Cfg))))
	if err != nil {
		t.Fatal(err)
	}
	cfgObj, _ := cfgRaw.(*jsonx.Object)
	sc := normalizeConfig(cfgObj)
	results := ProbeReport{
		Seatbelt: Probe{OK: true, Detail: "ok"},
		Bwrap:    Probe{Detail: "not Linux"},
		Landlock: Probe{Detail: "not Linux"},
	}
	o := policyOpts{env: env}
	var spec Spec
	var settings map[string]any
	if c.Kind == "host" {
		var extra []string
		for _, e := range c.Host.ExtraWritable {
			extra = append(extra, sub(e))
		}
		o.platform = "darwin"
		spec = prepareHost(sc, HostInput{
			Repo:          filepath.Join(root, c.Host.Repo),
			WorktreeBase:  filepath.Join(root, c.Host.WorktreeBase),
			HerdrHome:     filepath.Join(root, c.Host.HerdrHome),
			ExtraWritable: extra,
		}, &results, o)
		settings = SrtSettings(spec.Policy, []string{"/tmp/x.sock"}, []string{"api.anthropic.com"},
			[]string{"bad.example.org"}, true)
	} else {
		var network *Network
		if c.Network != nil {
			network = &Network{Reason: c.Network.Reason, ID: c.Network.ID,
				Session: c.Network.Session, Domains: c.Network.Domains}
			if c.Network.Srt != nil {
				network.Srt = sub(*c.Network.Srt)
			}
		}
		spec = prepare("pi", filepath.Join(root, c.Tree), sc, &results, o, network)
		if spec.Srt != nil {
			settings = spec.Srt.Settings
		}
	}
	argv := WrapArgv(f.Argv, filepath.Join(root, c.Cwd), spec)
	unsub := func(s string) string { return strings.ReplaceAll(s, root, "<ROOT>") }
	for i := range argv {
		argv[i] = unsub(argv[i])
	}
	if settings != nil {
		b, _ := json.Marshal(settings)
		settings = nil
		if err := json.Unmarshal([]byte(unsub(string(b))), &settings); err != nil {
			t.Fatal(err)
		}
	}
	path := ""
	if spec.Srt != nil {
		path = unsub(spec.Srt.SettingsPath)
	}
	spec.Reason = unsub(spec.Reason)
	return spec, argv, settings, path
}
