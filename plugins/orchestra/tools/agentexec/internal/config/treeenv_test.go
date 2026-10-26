package config

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// isMiseTrustEntry reports whether kv is the MISE_TRUSTED_CONFIG_PATHS entry.
func isMiseTrustEntry(kv string) bool {
	name, _, _ := strings.Cut(kv, "=")
	return name == MiseTrustedEnv
}

// isPnpmStoreEntry reports whether kv is the pnpm store pin entry.
func isPnpmStoreEntry(kv string) bool {
	name, _, _ := strings.Cut(kv, "=")
	return name == PnpmStorePinName
}

// mapGetenv is a getenv over a map, for PnpmStoreDir.
func mapGetenv(m map[string]string) func(string) string {
	return func(name string) string { return m[name] }
}

// The store root is pnpm's own default: PNPM_HOME, else XDG_DATA_HOME, else
// the platform's data dir. A relative PNPM_HOME or XDG_DATA_HOME is ignored.
func TestPnpmStoreDir(t *testing.T) {
	cases := []struct {
		name     string
		platform string
		env      map[string]string
		want     string
	}{
		{"darwin default", "darwin", nil, "/h/Library/pnpm/store"},
		{"linux default", "linux", nil, "/h/.local/share/pnpm/store"},
		{"PNPM_HOME", "darwin", map[string]string{"PNPM_HOME": "/ph"}, "/ph/store"},
		{"relative PNPM_HOME ignored", "darwin", map[string]string{"PNPM_HOME": "rel", "XDG_DATA_HOME": "/x"}, "/x/pnpm/store"},
		{"relative PNPM_HOME alone ignored", "darwin", map[string]string{"PNPM_HOME": "rel"}, "/h/Library/pnpm/store"},
		{"relative XDG_DATA_HOME ignored", "linux", map[string]string{"XDG_DATA_HOME": "rel"}, "/h/.local/share/pnpm/store"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := PnpmStoreDir(c.platform, "/h", mapGetenv(c.env)); got != c.want {
				t.Errorf("PnpmStoreDir = %q, want %q", got, c.want)
			}
		})
	}
}

// The pin is the one variable pnpm 12 reads. A store the user set under either
// name, in any case, wins and gets no pin; an explicit npm name is carried over
// as the pin's value, and never written as an entry of its own.
func TestPnpmStorePin(t *testing.T) {
	cases := []struct {
		name string
		env  []string
		want []string
	}{
		{"no explicit store", nil, []string{PnpmStorePinName + "=/store"}},
		{"pnpm name", []string{"pnpm_config_store_dir=/mine"}, nil},
		{"pnpm name, any case", []string{"PNPM_CONFIG_STORE_DIR=/mine"}, nil},
		{"npm name", []string{"npm_config_store_dir=/mine"}, []string{PnpmStorePinName + "=/mine"}},
		{"npm name, any case", []string{"NPM_CONFIG_STORE_DIR=/mine"}, []string{PnpmStorePinName + "=/mine"}},
		{"explicit pnpm name beats npm name", []string{"npm_config_store_dir=/mine", "pnpm_config_store_dir=/x"}, nil},
		{"empty npm name is no choice", []string{"npm_config_store_dir="}, []string{PnpmStorePinName + "=/store"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := PnpmStorePin(c.env, "/store")
			if !slices.Equal(got, c.want) {
				t.Errorf("PnpmStorePin = %q, want %q", got, c.want)
			}
			if slices.ContainsFunc(got, func(kv string) bool { return strings.HasPrefix(strings.ToLower(kv), "npm_") }) {
				t.Errorf("PnpmStorePin wrote an npm entry: %q", got)
			}
		})
	}
}

// A linked worktree gets the pin first, so the GVS switch still sits right
// before the command. A main checkout and "" get none, and so does a tree
// whose project chose its own store in .npmrc or pnpm-workspace.yaml.
func TestTreeEntriesPinsTheStore(t *testing.T) {
	main, linked := miseTrees(t)
	env := []string{"HOME=/h", "PNPM_HOME=/ph"}
	const pin = PnpmStorePinName + "=/ph/store"

	t.Run("linked tree pins first", func(t *testing.T) {
		writeTestFile(t, filepath.Join(linked, "pnpm-lock.yaml"), "lockfileVersion: 9\n")
		got := TreeEntries(true, linked, env)
		if len(got) < 2 || got[0] != pin || got[1] != PnpmGVSEnv {
			t.Errorf("entries = %q, want %q then %q", got, pin, PnpmGVSEnv)
		}
	})
	t.Run("main checkout", func(t *testing.T) {
		if slices.ContainsFunc(TreeEntries(false, main, env), isPnpmStoreEntry) {
			t.Error("main checkout got a store pin")
		}
	})
	t.Run("no tree", func(t *testing.T) {
		if slices.ContainsFunc(TreeEntries(false, "", env), isPnpmStoreEntry) {
			t.Error("empty tree got a store pin")
		}
	})
	t.Run("project store-dir in .npmrc", func(t *testing.T) {
		writeTestFile(t, filepath.Join(linked, ".npmrc"), "# keep\n  store-dir = /mine\n")
		if slices.ContainsFunc(TreeEntries(false, linked, env), isPnpmStoreEntry) {
			t.Error("tree with .npmrc store-dir got a store pin")
		}
	})
	t.Run("project storeDir in pnpm-workspace.yaml", func(t *testing.T) {
		if err := os.Remove(filepath.Join(linked, ".npmrc")); err != nil {
			t.Fatal(err)
		}
		writeTestFile(t, filepath.Join(linked, "pnpm-workspace.yaml"), "packages:\n  - .\nstoreDir: /mine\n")
		if slices.ContainsFunc(TreeEntries(false, linked, env), isPnpmStoreEntry) {
			t.Error("tree with pnpm-workspace.yaml storeDir got a store pin")
		}
	})
}

// WithTreeEnv sets the pin once, replacing nothing else.
func TestWithTreeEnvSetsThePinOnce(t *testing.T) {
	_, linked := miseTrees(t)
	out := WithTreeEnv(false, linked, []string{"HOME=/h"})
	n := 0
	for _, kv := range out {
		if isPnpmStoreEntry(kv) {
			n++
		}
	}
	if n != 1 || !slices.Contains(out, "HOME=/h") {
		t.Errorf("env = %q, want one store pin beside HOME", out)
	}
}

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
	if want := MiseTrustedEnv + "=/keep:" + linked; !slices.Contains(TreeEntries(false, linked, env), want) {
		t.Errorf("entries lack %q", want)
	}
	out := WithTreeEnv(false, linked, env)
	if want := MiseTrustedEnv + "=/keep:" + linked; !slices.Contains(out, want) || !slices.Contains(out, "HOME=/h") {
		t.Errorf("env = %q, want %q kept beside HOME", out, want)
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
	if entries := TreeEntries(false, linked, nil); slices.ContainsFunc(entries, isMiseTrustEntry) {
		t.Errorf("entries = %q without a probe seam", entries)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("mise ran without a probe seam")
	}
}
