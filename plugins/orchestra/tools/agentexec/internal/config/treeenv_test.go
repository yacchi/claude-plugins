package config

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// miseTrees is a linked worktree with a mise config at its root, and the main
// checkout it belongs to, as git lays them out.
func miseTrees(t *testing.T) (main, linked string) {
	t.Helper()
	main = t.TempDir()
	writeTestFile(t, filepath.Join(main, ".git", "HEAD"), "ref: refs/heads/main\n")
	linked = t.TempDir()
	writeTestFile(t, filepath.Join(linked, ".git"), "gitdir: "+filepath.Join(main, ".git", "worktrees", "l")+"\n")
	writeTestFile(t, filepath.Join(linked, "mise.toml"), "[env]\n")
	return main, linked
}

// setMiseShow replaces the `mise trust --show` probe for the rest of t.
func setMiseShow(t *testing.T, probe func(dir string) ([]byte, error)) {
	t.Helper()
	was := MiseShowSeam
	MiseShowSeam = probe
	t.Cleanup(func() { MiseShowSeam = was })
}

// A tree gets MISE_TRUSTED_CONFIG_PATHS only when the probe of its main
// checkout says trusted: an untrusted, mixed, empty or failing answer grants
// nothing.
func TestMiseTrustNeverEscalates(t *testing.T) {
	cases := []struct {
		name  string
		probe func(dir string) ([]byte, error)
		want  bool
	}{
		{"trusted", func(dir string) ([]byte, error) {
			return []byte(dir + "/mise.toml: trusted\n"), nil
		}, true},
		{"untrusted", func(dir string) ([]byte, error) {
			return []byte(dir + "/mise.toml: untrusted\n"), nil
		}, false},
		{"mixed", func(dir string) ([]byte, error) {
			return []byte(dir + "/mise.toml: trusted\n" + dir + "/.tool-versions: untrusted\n"), nil
		}, false},
		{"empty", func(dir string) ([]byte, error) { return nil, nil }, false},
		{"no project config", func(dir string) ([]byte, error) {
			return []byte("mise No trusted config files found.\n"), nil
		}, false},
		{"global config trusted, checkout not listed", func(dir string) ([]byte, error) {
			return []byte("/Users/someone/.config/mise/config.toml: trusted\n"), nil
		}, false},
		{"error", func(dir string) ([]byte, error) { return []byte("x: trusted\n"), os.ErrNotExist }, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, linked := miseTrees(t)
			setMiseShow(t, c.probe)
			got := slices.ContainsFunc(TreeEntries(false, linked, nil), func(kv string) bool {
				return kv == MiseTrustedEnv+"="+linked
			})
			if got != c.want {
				t.Errorf("entry granted = %v, want %v", got, c.want)
			}
		})
	}
}

// The tree is appended to the value the process already has, never replacing
// it, and WithTreeEnv keeps the name once.
func TestMiseTrustAppendsToTheExistingValue(t *testing.T) {
	main, linked := miseTrees(t)
	setMiseShow(t, func(dir string) ([]byte, error) { return []byte(main + "/mise.toml: trusted\n"), nil })
	env := []string{"HOME=/h", MiseTrustedEnv + "=/keep"}
	if got, want := TreeEntries(false, linked, env), []string{MiseTrustedEnv + "=/keep:" + linked}; !slices.Equal(got, want) {
		t.Errorf("entries = %q, want %q", got, want)
	}
	out := WithTreeEnv(false, linked, env)
	if got, want := out, []string{"HOME=/h", MiseTrustedEnv + "=/keep:" + linked}; !slices.Equal(got, want) {
		t.Errorf("env = %q, want %q", got, want)
	}
}

// Inside a test binary the probe is off without a seam: mise is not run, and
// no tree gets the entry.
func TestMiseProbeIsOffInTestsWithoutASeam(t *testing.T) {
	_, linked := miseTrees(t)
	log := filepath.Join(t.TempDir(), "probes")
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "mise"), []byte("#!/bin/sh\necho probe >> '"+log+"'\necho \"$PWD/mise.toml: trusted\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	if entries := TreeEntries(false, linked, nil); len(entries) != 0 {
		t.Errorf("entries = %q without a probe seam", entries)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("mise ran without a probe seam")
	}
}
