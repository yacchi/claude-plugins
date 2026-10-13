package sandbox

import (
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// SeatbeltProfile renders the policy as SBPL plus its -D params ([key,
// path] pairs). Paths travel as params, so they are never quoted into the
// profile.
//
// `(allow default)` comes first, then the denies: SBPL is last-match-wins,
// so each deny overrides the blanket allow, a carve-out's re-allow comes
// after the deny it narrows, and the tamper denies come after every allow.
func SeatbeltProfile(p *Policy) (string, [][2]string) {
	var params [][2]string
	param := func(prefix, path string) string {
		// Numbered per prefix the way the Python implementation counts: by
		// every key STARTING with the prefix (so R continues after RO/RW).
		n := 0
		for _, kv := range params {
			if strings.HasPrefix(kv[0], prefix) {
				n++
			}
		}
		key := prefix + strconv.Itoa(n)
		params = append(params, [2]string{key, path})
		return `(subpath (param "` + key + `"))`
	}
	rules := []string{"(version 1)", "(allow default)"}
	var allow []string
	for _, w := range p.Writable {
		allow = append(allow, param("W", w))
	}
	for _, rx := range p.WritableRegex {
		if !strings.Contains(rx, `"`) {
			allow = append(allow, `(regex #"`+rx+`")`)
		}
	}
	rules = append(rules, "(deny file-write* (require-not (require-any "+strings.Join(allow, " ")+")))")
	for _, ro := range p.Readonly {
		var inner []string
		for _, w := range p.Writable {
			if w != ro && under(w, ro) {
				inner = append(inner, w)
			}
		}
		roRule := param("RO", ro)
		if len(inner) > 0 {
			var rw []string
			for _, w := range inner {
				rw = append(rw, param("RW", w))
			}
			rules = append(rules, "(deny file-write* (require-all "+roRule+
				" (require-not (require-any "+strings.Join(rw, " ")+"))))")
		} else {
			rules = append(rules, "(deny file-write* "+roRule+")")
		}
	}
	if len(p.DenyRead) > 0 {
		var r []string
		for _, d := range p.DenyRead {
			r = append(r, param("R", d))
		}
		rules = append(rules, "(deny file-read* "+strings.Join(r, " ")+")")
	}
	rules = append(rules, "(deny signal (require-not (target same-sandbox)))")
	var deny []string
	for _, d := range p.DenyWrite {
		deny = append(deny, param("D", d))
	}
	for _, rx := range p.DenyWriteRegex {
		if !strings.Contains(rx, `"`) {
			deny = append(deny, `(regex #"`+rx+`")`)
		}
	}
	if len(deny) > 0 {
		rules = append(rules, "(deny file-write* "+strings.Join(deny, " ")+")")
	}
	return strings.Join(rules, " "), params
}

func seatbeltArgv(argv []string, p *Policy) []string {
	sbpl, params := SeatbeltProfile(p)
	out := []string{SandboxExec}
	for _, kv := range params {
		out = append(out, "-D", kv[0]+"="+kv[1])
	}
	out = append(out, "-p", sbpl, "--")
	return append(out, argv...)
}

func bwrapArgv(argv []string, cwd string, p *Policy, exists, isDir func(string) bool) []string {
	out := []string{"bwrap", "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc"}
	var late []string
	for _, w := range p.Writable {
		if w == "/dev" || !exists(w) {
			continue
		}
		inCarve := false
		for _, ro := range p.Readonly {
			if under(w, ro) && w != ro {
				inCarve = true
				break
			}
		}
		if inCarve {
			late = append(late, w)
		} else {
			out = append(out, "--bind", w, w)
		}
	}
	for _, ro := range p.Readonly {
		if exists(ro) {
			out = append(out, "--ro-bind", ro, ro)
		}
	}
	for _, w := range late {
		out = append(out, "--bind", w, w)
	}
	protected := append([]string(nil), p.DenyWrite...)
	if p.RepoRoot != "" && len(p.DenyWriteRegex) > 0 {
		protected = append(protected, repoConfigFiles(p.RepoRoot, 200)...)
	}
	for _, d := range protected {
		if exists(d) {
			out = append(out, "--ro-bind", d, d)
		}
	}
	for _, r := range p.DenyRead {
		if !exists(r) {
			continue
		}
		if isDir(r) {
			out = append(out, "--tmpfs", r)
		} else {
			out = append(out, "--ro-bind", "/dev/null", r)
		}
	}
	out = append(out, "--unshare-pid", "--die-with-parent", "--chdir", cwd, "--")
	return append(out, argv...)
}

// selfExe is the agent-exec binary the Landlock child re-executes.
var selfExe = func() string {
	exe, err := os.Executable()
	if err != nil {
		return "agent-exec"
	}
	return exe
}

func landlockArgv(argv []string, p *Policy) []string {
	out := []string{selfExe(), "_sandbox-exec"}
	for _, w := range p.Writable {
		out = append(out, "--write", w)
	}
	tamper := p.Tamper
	if len(tamper) == 0 {
		tamper = tamperPaths("")
	}
	for _, t := range tamper {
		out = append(out, "--protect", t)
	}
	out = append(out, "--")
	return append(out, argv...)
}

var shUnsafe = regexp.MustCompile(`[^A-Za-z0-9_@%+=:,./-]`)

// ShellQuote is Python's shlex.quote.
func ShellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if !shUnsafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// SrtArgv runs argv under srt: debug output on (blocked connections are
// parsed from it) and live config on the control fd.
func SrtArgv(srt, settingsPath string, argv []string) []string {
	quoted := make([]string, len(argv))
	for i, a := range argv {
		quoted[i] = ShellQuote(a)
	}
	return []string{srt, "-d", "-s", settingsPath, "--control-fd", strconv.Itoa(SrtControlFD),
		"-c", strings.Join(quoted, " ")}
}

// WrapArgv is the argv to actually spawn for argv under s. Every spawn path
// (capture, exec, DRYRUN preview) goes through here.
func WrapArgv(argv []string, cwd string, s Spec) []string {
	p := s.Policy
	if s.Backend == "srt" && p != nil && s.Srt != nil {
		// srt confines by itself: never nested in Seatbelt/bwrap/Landlock.
		return SrtArgv(s.Srt.Path, s.Srt.SettingsPath, argv)
	}
	if p == nil {
		return append([]string(nil), argv...)
	}
	switch s.Backend {
	case "seatbelt":
		return seatbeltArgv(argv, p)
	case "bwrap":
		if cwd == "" {
			cwd, _ = os.Getwd()
		}
		return bwrapArgv(argv, realPath(cwd), p, exists, isDir)
	case "landlock":
		return landlockArgv(argv, p)
	}
	return append([]string(nil), argv...)
}

var regexGlobRe = regexp.MustCompile(`^\^(.*)/\(\.\*/\)\?\\\.claude/orchestra`)
var unescapeRe = regexp.MustCompile(`\\(.)`)

// regexGlobs turns the repo-config regex into srt globs.
func regexGlobs(rx string) []string {
	m := regexGlobRe.FindStringSubmatch(rx)
	if m == nil {
		return nil
	}
	root := unescapeRe.ReplaceAllString(m[1], "$1")
	return []string{root + "/.claude/orchestra*.y*ml", root + "/**/.claude/orchestra*.y*ml"}
}

// carveOut lists the entries of the read-only dir ro to deny so that only
// the writable paths inner below it stay writable.
func carveOut(ro string, inner []string) []string {
	entries, err := os.ReadDir(ro)
	names := []string{}
	if err == nil {
		for _, e := range entries {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	var out []string
	for _, name := range names {
		entry := pyJoin(ro, name)
		var holders []string
		for _, w := range inner {
			if under(w, entry) {
				holders = append(holders, w)
			}
		}
		switch {
		case len(holders) == 0:
			out = append(out, realIfExists(entry))
		case !contains(holders, entry) && isDir(entry) && !isLink(entry):
			out = append(out, carveOut(entry, holders)...)
		}
	}
	return out
}

// SrtSettings is the sandbox-runtime settings for a policy. Paths are
// realpaths where they exist (srt follows no symlink for an allow);
// WritableRegex becomes the glob <home>/.claude.json*; a Readonly carve-out
// becomes a deny of its entries that hold no writable path.
func SrtSettings(p *Policy, sockets, allowed, denied []string, allowPty bool) map[string]any {
	return srtSettings(p, sockets, allowed, denied, "", allowPty)
}

func srtSettings(p *Policy, sockets, allowed, denied []string, home string, allowPty bool) map[string]any {
	home = homeOf(home)
	var allow []string
	for _, w := range p.Writable {
		allow = append(allow, realIfExists(w))
	}
	allow = dedupe(allow)
	if len(p.WritableRegex) > 0 {
		allow = append(allow, pyJoin(home, ".claude.json*"))
	}
	var deny []string
	for _, d := range p.DenyWrite {
		deny = append(deny, realIfExists(d))
	}
	for _, rx := range p.DenyWriteRegex {
		deny = append(deny, regexGlobs(rx)...)
	}
	for _, ro := range p.Readonly {
		var inner []string
		for _, w := range p.Writable {
			if w != ro && under(w, ro) {
				inner = append(inner, w)
			}
		}
		deny = append(deny, carveOut(ro, inner)...)
	}
	var denyRead []string
	for _, r := range p.DenyRead {
		denyRead = append(denyRead, realIfExists(r))
	}
	if sockets == nil {
		sockets = []string{}
	}
	return map[string]any{
		"network": map[string]any{
			"allowedDomains":      dedupe(allowed),
			"deniedDomains":       dedupe(denied),
			"allowUnixSockets":    append([]string{}, sockets...),
			"allowAllUnixSockets": false,
		},
		"filesystem": map[string]any{
			"allowWrite": allow,
			"denyWrite":  dedupe(deny),
			"denyRead":   dedupe(denyRead),
		},
		"allowPty": allowPty,
	}
}
