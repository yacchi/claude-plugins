package sandbox

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
)

// nowISOZ stamps new grants (a seam for the byte-compatibility fixtures).
var nowISOZ = fsutil.NowISOZ

// ErrNotFound is ForgetGrant/ForgetDomain's "nothing to remove".
var ErrNotFound = errors.New("no such grant")

// StateDir is the machine-local orchestra state directory:
// $XDG_STATE_HOME/claude-orchestra when XDG_STATE_HOME is absolute, else
// ~/.local/state/claude-orchestra, realpath'd.
func StateDir() string { return stateDir("") }

func stateDir(home string) string {
	home = homeOf(home)
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" && strings.HasPrefix(xdg, "/") {
		return realPath(pyJoin(xdg, "claude-orchestra"))
	}
	return expand(stateDirRel, home)
}

// LearnedPath is the machine-local grant store.
func LearnedPath() string { return learnedPath("") }

func learnedPath(home string) string {
	return pyJoin(stateDir(home), "sandbox-learned.json")
}

func cacheRoots(home string) []string {
	home = homeOf(home)
	out := make([]string, len(cacheRootsRel))
	for i, r := range cacheRootsRel {
		out[i] = expand(r, home)
	}
	return out
}

// TamperPaths are the paths no worker may write: orchestra's user config,
// executor state, uv's installed tools, the learned store and its dir.
func TamperPaths() []string { return tamperPaths("") }

func tamperPaths(home string) []string {
	home = homeOf(home)
	var out []string
	for _, p := range tamperFiles {
		out = append(out, expand(p, home))
	}
	store := learnedPath(home)
	out = append(out, store, pyDirname(store))
	return dedupe(out)
}

// stateDenies is tamperPaths for a policy whose writable set reaches INTO
// the state dir: the state dir itself is left out (a deny wins over every
// allow) and its protected entries are denied by name.
func stateDenies(home string) []string {
	home = homeOf(home)
	state := realPath(stateDir(home))
	var out []string
	for _, p := range tamperPaths(home) {
		if realPath(p) != state {
			out = append(out, p)
		}
	}
	out = append(out, pyJoin(state, "tools"), pyJoin(state, SrtSettingsDir),
		pyJoin(state, CLISrtDir), pyJoin(state, AuditDir))
	return dedupe(out)
}

// GrantFor is the auto-grant for a denied path, or "" when not eligible:
// the realpath matched (component boundary) against a cache root, granted
// as that root plus ONE further component (or the root itself). A
// candidate overlapping a tamper-protected path is never returned.
func GrantFor(path string) string { return grantFor(path, "") }

func grantFor(path, home string) string {
	if path == "" {
		return ""
	}
	real := realPath(path)
	protected := tamperPaths(home)
	for _, root := range cacheRoots(home) {
		candidate := ""
		if real == root {
			candidate = root
		} else if under(real, root) {
			relative := real[len(strings.TrimRight(root, "/"))+1:]
			first, _, found := strings.Cut(relative, "/")
			if first == "" || first == "." || first == ".." {
				continue
			}
			candidate = root
			if found {
				candidate = pyJoin(root, first)
			}
		}
		if candidate == "" {
			continue
		}
		bad := false
		for _, t := range protected {
			if overlaps(candidate, t) {
				bad = true
				break
			}
		}
		if !bad {
			return candidate
		}
	}
	return ""
}

// UserGrantRefusal is why a user/instructor grant on path is refused ("" =
// allowed). denyRead are realpath'd deny_read entries.
func UserGrantRefusal(path string, denyRead []string) string {
	return userGrantRefusal(path, denyRead, "")
}

func userGrantRefusal(path string, denyRead []string, home string) string {
	home = homeOf(home)
	real := realPath(path)
	if real == "/" {
		return "refusing to grant /"
	}
	if real == home {
		return "refusing to grant the home directory itself"
	}
	lexical := absPath(path)
	for _, claude := range []string{expand("~/.claude", home), pyJoin(home, ".claude")} {
		if overlaps(real, claude) || overlaps(lexical, claude) {
			return "refusing to grant anything under ~/.claude"
		}
	}
	for _, r := range denyRead {
		if overlaps(real, r) {
			return "path overlaps deny_read entry " + r
		}
	}
	for _, t := range tamperPaths(home) {
		if overlaps(t, real) {
			return "path overlaps tamper-protected " + t
		}
	}
	return ""
}

// Grant is one learned path grant. The entry is kept as stored (unknown
// keys and key order survive a rewrite of the store).
type Grant struct {
	Path, Source, Added, Example, Reason string
	raw                                  *jsonx.Object
}

func (g Grant) MarshalJSON() ([]byte, error) { return jsonx.Marshal(g.raw) }

// Object is the entry as stored.
func (g Grant) Object() *jsonx.Object { return g.raw }

// DomainGrant is one learned network domain grant, kept as stored.
type DomainGrant struct {
	Domain, Source, Added, Reason string
	raw                           *jsonx.Object
}

func (d DomainGrant) MarshalJSON() ([]byte, error) { return jsonx.Marshal(d.raw) }

func (d DomainGrant) Object() *jsonx.Object { return d.raw }

// Learned is the machine's learned store. Warning is set when the store is
// unreadable or corrupt, or entries failing their source's rules were
// dropped (never trusted); DomainWarning likewise for the domain list.
type Learned struct {
	Path          string
	Grants        []Grant
	Domains       []DomainGrant
	Warning       string
	DomainWarning string
}

// LoadLearned reads the store. A missing store is empty; an unreadable or
// corrupt one is empty with the reason as both Warning and the error.
func LoadLearned() (Learned, error) {
	home := homeOf("")
	l := Learned{Path: learnedPath(home)}
	var err error
	l.Grants, l.Warning, err = loadGrants(home, nil)
	l.Domains, l.DomainWarning = loadDomains(home)
	return l, err
}

func str(o *jsonx.Object, k string) string {
	v, _ := o.Get(k)
	s, _ := v.(string)
	return s
}

func grantOf(o *jsonx.Object) Grant {
	return Grant{Path: str(o, "path"), Source: str(o, "source"), Added: str(o, "added"),
		Example: str(o, "example"), Reason: str(o, "reason"), raw: o}
}

func domainOf(o *jsonx.Object) DomainGrant {
	return DomainGrant{Domain: str(o, "domain"), Source: str(o, "source"),
		Added: str(o, "added"), Reason: str(o, "reason"), raw: o}
}

func defaultDenyRead(home string) []string {
	out := make([]string, len(DefaultDenyRead))
	for i, p := range DefaultDenyRead {
		out[i] = expand(p, home)
	}
	return out
}

func validGrant(v any, home string, denyRead []string) bool {
	o, ok := v.(*jsonx.Object)
	if !ok || o == nil {
		return false
	}
	p, ok := mustGet(o, "path").(string)
	if !ok || !strings.HasPrefix(p, "/") {
		return false
	}
	src, _ := mustGet(o, "source").(string)
	switch {
	case src == "auto":
		return grantFor(p, home) != ""
	case contains(userSources, src):
		return userGrantRefusal(p, denyRead, home) == ""
	}
	return false
}

func mustGet(o *jsonx.Object, k string) any {
	v, _ := o.Get(k)
	return v
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// loadGrants is load_learned: (grants, warning, error-for-unusable-store).
func loadGrants(home string, denyRead []string) ([]Grant, string, error) {
	home = homeOf(home)
	if denyRead == nil {
		denyRead = defaultDenyRead(home)
	}
	path := learnedPath(home)
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return []Grant{}, "", nil
	}
	var data any
	if err == nil {
		data, err = jsonx.Decode(raw)
	}
	if err != nil {
		w := fmt.Sprintf("learned sandbox grants unreadable (%s): %v", path, err)
		return []Grant{}, w, errors.New(w)
	}
	root, _ := data.(*jsonx.Object)
	var list []any
	ok := false
	if root != nil {
		list, ok = mustGet(root, "grants").([]any)
	}
	if !ok {
		w := fmt.Sprintf("learned sandbox grants corrupt (%s): no grants list", path)
		return []Grant{}, w, errors.New(w)
	}
	good := []Grant{}
	for _, e := range list {
		if validGrant(e, home, denyRead) {
			good = append(good, grantOf(e.(*jsonx.Object).Clone()))
		}
	}
	warning := ""
	if n := len(list) - len(good); n != 0 {
		warning = fmt.Sprintf("learned sandbox grants: ignored %d invalid entr%s in %s", n, plural(n), path)
	}
	return good, warning, nil
}

// rawStore is the store's top-level object, or empty when unusable.
func rawStore(home string) *jsonx.Object {
	raw, err := os.ReadFile(learnedPath(home))
	if err != nil {
		return jsonx.NewObject()
	}
	data, err := jsonx.Decode(raw)
	if err != nil {
		return jsonx.NewObject()
	}
	if o, ok := data.(*jsonx.Object); ok && o != nil {
		return o
	}
	return jsonx.NewObject()
}

// saveLearned writes the store atomically (0600 file in a 0700 dir). A nil
// list keeps that list of the store as it is.
func saveLearned(home string, grants, domains []any) error {
	path := learnedPath(home)
	if err := os.MkdirAll(pyDirname(path), 0o700); err != nil {
		return err
	}
	raw := rawStore(home)
	if grants == nil {
		grants, _ = mustGet(raw, "grants").([]any)
		if grants == nil {
			grants = []any{}
		}
	}
	if domains == nil {
		domains, _ = mustGet(raw, "domains").([]any)
	}
	data := jsonx.NewObject().Set("version", int64(1)).Set("grants", grants)
	if len(domains) > 0 {
		data.Set("domains", domains)
	}
	payload, err := jsonx.MarshalIndent(data)
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(path, append(payload, '\n'), 0o600)
}

// AddGrant persists one grant (idempotent per realpath). source is auto
// (must be an eligible cache grant) or user/instructor (the user rules).
func AddGrant(path, reason, source string) error {
	_, err := AddGrantEntry(path, source, "", reason)
	return err
}

// AddGrantEntry is AddGrant returning the stored entry; example is the
// denied path an auto grant was made for.
func AddGrantEntry(path, source, example, reason string) (Grant, error) {
	return addGrant(path, source, example, reason, "")
}

func addGrant(path, source, example, reason, home string) (Grant, error) {
	home = homeOf(home)
	real := realPath(path)
	switch {
	case source == "auto":
		if grantFor(real, home) == "" {
			return Grant{}, fmt.Errorf("not an eligible cache grant: %s", real)
		}
	case contains(userSources, source):
		if r := userGrantRefusal(real, defaultDenyRead(home), home); r != "" {
			return Grant{}, errors.New(r)
		}
	default:
		return Grant{}, fmt.Errorf("unknown grant source: %s", source)
	}
	grants, _, _ := loadGrants(home, nil)
	for _, g := range grants {
		if g.Path == real {
			return g, nil
		}
	}
	ex := real
	if example != "" {
		ex = realPath(example)
	}
	entry := jsonx.NewObject().Set("path", real).Set("added", nowISOZ()).
		Set("source", source).Set("example", ex)
	if reason != "" {
		entry.Set("reason", reason)
	}
	list := make([]any, 0, len(grants)+1)
	for _, g := range grants {
		list = append(list, g.raw)
	}
	list = append(list, entry)
	if err := saveLearned(home, list, nil); err != nil {
		return Grant{}, err
	}
	return grantOf(entry), nil
}

// ForgetGrant removes the grant on path's realpath; ErrNotFound if none.
func ForgetGrant(path string) error { return forgetGrant(path, "") }

func forgetGrant(path, home string) error {
	home = homeOf(home)
	real := realPath(path)
	grants, _, _ := loadGrants(home, nil)
	kept := []any{}
	for _, g := range grants {
		if g.Path != real {
			kept = append(kept, g.raw)
		}
	}
	if len(kept) == len(grants) {
		return ErrNotFound
	}
	return saveLearned(home, kept, nil)
}
