package tools

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

const errorTail = 500

func installResult(name, status string, installer, version, path any, errMsg any) Item {
	return map[string]any{
		"name":      name,
		"status":    status,
		"installer": installer,
		"version":   version,
		"path":      path,
		"error":     errMsg,
	}
}

func tail(text string) string {
	text = strings.TrimSpace(text)
	if r := []rune(text); len(r) > errorTail {
		return string(r[len(r)-errorTail:])
	}
	return text
}

// MiseEnv is a fully private mise world, so the user's global or project
// mise config is neither read nor modified. It returns the environment and
// the directory to run in.
func MiseEnv(base []string) ([]string, string) {
	root := filepath.Join(Root(), "mise")
	cfgDir := filepath.Join(root, "config")
	return withEnv(base, [][2]string{
		{"MISE_DATA_DIR", filepath.Join(root, "data")},
		{"MISE_CONFIG_DIR", cfgDir},
		{"MISE_GLOBAL_CONFIG_FILE", filepath.Join(cfgDir, "config.toml")},
		{"MISE_CACHE_DIR", filepath.Join(root, "cache")},
		{"MISE_STATE_DIR", filepath.Join(root, "state")},
		{"MISE_CEILING_PATHS", cfgDir},
		{"MISE_YES", "1"},
	}), cfgDir
}

func withEnv(base []string, set [][2]string) []string {
	drop := map[string]bool{}
	for _, kv := range set {
		drop[kv[0]] = true
	}
	var out []string
	for _, e := range base {
		if k, _, _ := strings.Cut(e, "="); !drop[k] {
			out = append(out, e)
		}
	}
	for _, kv := range set {
		out = append(out, kv[0]+"="+kv[1])
	}
	return out
}

// run executes argv capturing output. A failure to start is reported with
// Python's OSError wording, which the install rows carry through.
func run(argv, env []string, dir string) (stdout, stderr string, code int, err error) {
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env, cmd.Dir = env, dir
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if runErr := cmd.Run(); runErr != nil {
		var ee *exec.ExitError
		if errors.As(runErr, &ee) {
			return out.String(), errb.String(), ee.ExitCode(), nil
		}
		if errors.Is(runErr, exec.ErrNotFound) {
			return "", "", 0, fmt.Errorf("[Errno 2] No such file or directory: '%s'", argv[0])
		}
		return "", "", 0, runErr
	}
	return out.String(), errb.String(), 0, nil
}

func installMise(tc ToolConfig, base []string) (string, string) {
	env, cwd := MiseEnv(base)
	if err := os.MkdirAll(cwd, 0o777); err != nil {
		return "", err.Error()
	}
	target := tc.Spec + "@" + tc.Version
	var stdout string
	for _, argv := range [][]string{
		{"mise", "install", target},
		{"mise", "exec", target, "--", "which", tc.Bin},
	} {
		out, errText, code, err := run(argv, env, cwd)
		if err != nil {
			return "", err.Error()
		}
		if code != 0 {
			if t := tail(errText); t != "" {
				return "", t
			}
			return "", fmt.Sprintf("%s exited %d", argv[1], code)
		}
		stdout = out
	}
	path := ""
	for _, ln := range strings.Split(stdout, "\n") {
		if t := strings.TrimSpace(ln); t != "" {
			path = t
		}
	}
	if !executable(path) {
		return "", "mise did not report an executable path for " + tc.Bin
	}
	return path, ""
}

func installNpm(name string, tc ToolConfig, base []string) (string, string) {
	if !strings.HasPrefix(tc.Spec, "npm:") {
		return "", "npm installer only supports npm: specs"
	}
	prefix := filepath.Join(Root(), "npm", name, tc.Version)
	if err := os.MkdirAll(prefix, 0o777); err != nil {
		return "", err.Error()
	}
	argv := []string{"npm", "install", "--prefix", prefix, "--no-fund", "--no-audit",
		strings.TrimPrefix(tc.Spec, "npm:") + "@" + tc.Version}
	_, errText, code, err := run(argv, base, "")
	if err != nil {
		return "", err.Error()
	}
	if code != 0 {
		if t := tail(errText); t != "" {
			return "", t
		}
		return "", fmt.Sprintf("npm exited %d", code)
	}
	path := filepath.Join(prefix, "node_modules", ".bin", tc.Bin)
	if !executable(path) {
		return "", "npm did not produce an executable at " + path
	}
	return path, ""
}

// Install installs each named tool and returns one row per name. The
// manifest is only touched after a successful install. installer "" means
// the configured one (default auto: mise when present, else npm).
func Install(cfg *config.Resolved, names []string, installer string, force bool) []Item {
	base := os.Environ()
	chosen := installer
	if chosen == "" {
		v := section(cfg)["installer"]
		chosen, _ = v.(string)
		if chosen == "" {
			chosen = "auto"
		}
	}
	results := []Item{}
	for _, name := range names {
		tc := ConfigFor(name, cfg, KnownBinaries[name])
		if tc.Spec == "" || tc.Version == "" {
			results = append(results, installResult(name, "skipped", nil, nil, nil, nil))
			continue
		}
		rec := LoadManifest()[name]
		if !force && recordState(rec, tc) == "valid" {
			results = append(results, installResult(name, "already",
				mustGet(rec, "installer"), mustGet(rec, "version"), mustGet(rec, "path"), nil))
			continue
		}
		how := chosen
		if how == "auto" {
			how = "npm"
			if _, err := exec.LookPath("mise"); err == nil {
				how = "mise"
			}
		}
		var path, errMsg string
		switch how {
		case "mise":
			path, errMsg = installMise(tc, base)
		case "npm":
			path, errMsg = installNpm(name, tc, base)
		default:
			errMsg = "unknown installer: " + how
		}
		if errMsg != "" {
			results = append(results, installResult(name, "failed", how, tc.Version, nil, errMsg))
			continue
		}
		saveRecord(name, map[string]any{
			"spec":         tc.Spec,
			"version":      tc.Version,
			"installer":    how,
			"path":         path,
			"installed_at": fsutil.NowISOZ(),
		})
		results = append(results, installResult(name, "installed", how, tc.Version, path, nil))
	}
	return results
}

// saveRecord stores one tool's record and rewrites the manifest (sorted
// keys, indent 2, ASCII-escaped).
func saveRecord(name string, rec map[string]any) {
	tools := map[string]any{}
	records := LoadManifest()
	records[name] = rec
	for n, r := range records {
		tools[n] = r
	}
	manifest := map[string]any{"tools": tools, "version": int64(1)}
	data, err := fsutil.MarshalJSONIndent(manifest)
	if err != nil {
		return
	}
	data = append(asciiEscape(data), '\n')
	if err := fsutil.EnsureDir(Root(), 0o755); err != nil {
		return
	}
	_ = fsutil.AtomicWriteFile(ManifestPath(), data, 0o600)
}

func asciiEscape(b []byte) []byte {
	var out bytes.Buffer
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		switch {
		case r < 0x80:
			out.WriteByte(b[0])
		case r >= 0x10000:
			r -= 0x10000
			fmt.Fprintf(&out, "\\u%04x\\u%04x", 0xd800+(r>>10), 0xdc00+(r&0x3ff))
		default:
			fmt.Fprintf(&out, "\\u%04x", r)
		}
		b = b[size:]
	}
	return out.Bytes()
}
