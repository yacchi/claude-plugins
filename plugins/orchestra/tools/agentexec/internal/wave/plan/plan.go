// Package plan loads and lints a wave's plan.json and holds the scheduler's
// pure functions (Select, TopoOrder, Blocked, Summary). No I/O beyond
// reading the plan, spec and preamble files; no git, no subprocesses.
package plan

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/slot"
)

// ValidCls are the dispatch classes a package may name.
var ValidCls = []string{"light", "standard", "deep"}

// Package statuses (shared-schema.md).
var Statuses = []string{
	"pending", "implementing", "verifying", "fixing", "ready",
	"integrated", "needs", "failed", "blocked",
}

var (
	inFlight     = map[string]bool{"implementing": true, "verifying": true, "fixing": true, "ready": true}
	blockingStat = map[string]bool{"failed": true, "needs": true, "blocked": true}
)

// InFlight reports whether a status counts as in flight.
func InFlight(status string) bool { return inFlight[status] }

// Error is any structurally or semantically invalid plan.json.
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errf(format string, args ...any) error { return &Error{Msg: fmt.Sprintf(format, args...)} }

// Rule is a `rules` entry: a package owning a file matching If must also own
// something matching each Require glob.
type Rule struct {
	If      string
	Require []string
}

// Interface is a `interfaces` entry: a frozen contract file that packages
// name by id and that their specs must paste verbatim; Path is absolute.
type Interface struct {
	ID   string
	Path string
}

// Package is one normalized plan package; Spec is an absolute path.
type Package struct {
	ID           string
	Spec         string
	Cls          string
	DependsOn    []string
	FilesOwned   []string
	Prio         float64
	Verification string
	Brief        string
	Risks        []string
	Interfaces   []string
	// EstSeconds is the plan's `est_seconds` (0 = not given).
	EstSeconds float64
}

// Plan is a normalized plan: packages in file order, relative preamble/spec
// paths resolved against the plan file's directory.
type Plan struct {
	WarmRead     []string
	Preamble     []string
	ExternalDone []string
	Interfaces   []Interface
	Rules        []Rule
	Packages     []Package

	// chain caches the dependent chains for the first chainN packages (the
	// runner only ever appends to Packages).
	chain  *chains
	chainN int
}

// Package returns the package with the given id.
func (p *Plan) Package(id string) (Package, bool) {
	for _, pkg := range p.Packages {
		if pkg.ID == id {
			return pkg, true
		}
	}
	return Package{}, false
}

// Interface returns the interface with the given id.
func (p *Plan) Interface(id string) (Interface, bool) {
	for _, iface := range p.Interfaces {
		if iface.ID == id {
			return iface, true
		}
	}
	return Interface{}, false
}

func resolve(p, base string) string {
	if filepath.IsAbs(p) {
		return p
	}
	return filepath.Join(base, p)
}

// jsonText renders a decoded JSON value as compact JSON for error messages.
func jsonText(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func stringList(v any) ([]string, bool) {
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		s, ok := e.(string)
		if !ok {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// Load reads plan.json, fills defaults, validates, and returns the
// normalized plan.
func Load(file string) (*Plan, error) {
	return load(file, false)
}

// LoadSkeleton is Load for a planning skeleton: the spec files need not exist
// yet, and every package must carry the fields a spec writer works from.
func LoadSkeleton(file string) (*Plan, error) {
	return load(file, true)
}

func load(file string, skeleton bool) (*Plan, error) {
	abs, err := filepath.Abs(file)
	if err != nil {
		abs = file
	}
	base := filepath.Dir(abs)

	data, err := os.ReadFile(file)
	if err != nil {
		return nil, errf("cannot read plan %s: %s", file, err)
	}
	doc, err := fsutil.DecodeJSON(data)
	if err != nil {
		return nil, errf("invalid JSON in plan %s: %s", file, err)
	}
	raw, ok := doc.(map[string]any)
	var rawPackages any
	if ok {
		rawPackages, ok = raw["packages"]
	}
	if !ok {
		return nil, errf("plan %s: missing required key 'packages'", file)
	}

	p := &Plan{}
	rulesV, has := raw["rules"]
	if !has {
		rulesV = []any{}
	}
	rules, isList := rulesV.([]any)
	if !isList {
		return nil, errf("plan rules must be a list")
	}
	for _, r := range rules {
		bad := errf("plan rules entries must be {'if': <glob>, 'require': [<glob>...]}")
		obj, isObj := r.(map[string]any)
		if !isObj {
			return nil, bad
		}
		ifV := obj["if"]
		ifS, isStr := ifV.(string)
		reqV := obj["require"]
		req, isReq := stringList(reqV)
		if !isStr || !isReq {
			return nil, bad
		}
		p.Rules = append(p.Rules, Rule{If: ifS, Require: req})
	}

	warmV, has := raw["warm_read"]
	if !has {
		warmV = []any{}
	}
	warm, isList := stringList(warmV)
	for _, w := range warm {
		if w == "" {
			isList = false
		}
	}
	if !isList {
		return nil, errf("plan warm_read must be a list of repo-relative paths")
	}
	p.WarmRead = []string{}
	for _, item := range warm {
		norm := path.Clean(strings.ReplaceAll(item, "\\", "/"))
		if filepath.IsAbs(item) || strings.HasPrefix(norm, "/") || norm == ".." || strings.HasPrefix(norm, "../") {
			return nil, errf("warm_read path must be repo-relative and stay inside the repo: %s", item)
		}
		if !contains(p.WarmRead, norm) {
			p.WarmRead = append(p.WarmRead, norm)
		}
	}

	p.Preamble = []string{}
	if v, has := raw["preamble"]; has {
		list, isList := stringList(v)
		if !isList {
			return nil, errf("plan preamble must be a list of paths")
		}
		for _, e := range list {
			p.Preamble = append(p.Preamble, resolve(e, base))
		}
	}
	p.ExternalDone = []string{}
	if v, has := raw["external_done"]; has {
		list, isList := stringList(v)
		if !isList {
			return nil, errf("plan external_done must be a list of package ids")
		}
		p.ExternalDone = list
	}
	for _, pre := range p.Preamble {
		if _, err := os.Stat(pre); err != nil {
			return nil, errf("preamble path does not exist: %s", pre)
		}
	}

	p.Interfaces = []Interface{}
	if v, has := raw["interfaces"]; has {
		list, isList := v.([]any)
		if !isList {
			return nil, errf("plan interfaces must be a list of {'id': <string>, 'path': <path>} objects")
		}
		for _, e := range list {
			obj, isObj := e.(map[string]any)
			id, isID := obj["id"].(string)
			ifacePath, isPath := obj["path"].(string)
			if !isObj || !isID || !isPath || id == "" || ifacePath == "" {
				return nil, errf("plan interfaces must be a list of {'id': <string>, 'path': <path>} objects")
			}
			if _, dup := p.Interface(id); dup {
				return nil, errf("duplicate interface id: %s", id)
			}
			ifaceAbs := resolve(ifacePath, base)
			if _, err := os.Stat(ifaceAbs); err != nil {
				return nil, errf("interface %s: path does not exist: %s", id, ifaceAbs)
			}
			p.Interfaces = append(p.Interfaces, Interface{ID: id, Path: ifaceAbs})
		}
	}

	entries, isList := rawPackages.([]any)
	if !isList {
		return nil, errf("plan %s: 'packages' must be a list", file)
	}
	seen := map[string]bool{}
	for _, e := range entries {
		entry, isObj := e.(map[string]any)
		if !isObj {
			return nil, errf("plan packages entries must be objects: %s", jsonText(e))
		}
		idV := entry["id"]
		id, isStr := idV.(string)
		if !isStr || id == "" {
			return nil, errf("package missing required field 'id': %s", jsonText(entry))
		}
		if seen[id] {
			return nil, errf("duplicate package id: %s", id)
		}
		seen[id] = true

		specV := entry["spec"]
		spec, _ := specV.(string)
		if spec == "" {
			return nil, errf("%s: missing required field 'spec'", id)
		}
		clsV := entry["cls"]
		cls, _ := clsV.(string)
		if clsV == nil || clsV == "" {
			return nil, errf("%s: missing required field 'cls'", id)
		}
		if !contains(ValidCls, cls) {
			return nil, errf("%s: invalid cls %s (must be one of %s)", id, jsonText(clsV), strings.Join(ValidCls, ", "))
		}
		specPath := resolve(spec, base)
		// A skeleton's spec is written after it, so the file may not exist yet.
		if _, err := os.Stat(specPath); err != nil && !skeleton {
			return nil, errf("%s: spec path does not exist: %s", id, specPath)
		}
		pkg := Package{
			ID: id, Spec: specPath, Cls: cls, DependsOn: []string{}, FilesOwned: []string{},
			Risks: []string{}, Interfaces: []string{},
		}
		if v, has := entry["depends_on"]; has {
			list, isList := stringList(v)
			if !isList {
				return nil, errf("%s: depends_on must be a list of package ids", id)
			}
			pkg.DependsOn = list
		}
		if v, has := entry["files_owned"]; has {
			list, isList := stringList(v)
			if !isList {
				return nil, errf("%s: files_owned must be a list of paths", id)
			}
			pkg.FilesOwned = list
		}
		if v, has := entry["prio"]; has {
			switch n := v.(type) {
			case int64:
				pkg.Prio = float64(n)
			case float64:
				pkg.Prio = n
			}
		}
		// A package's verification runs in its worker's tree, many at once:
		// the whole suite there is the cost the run after integration exists
		// to pay once.
		if v, has := entry["verification"]; has {
			verification, isStr := v.(string)
			if !isStr {
				return nil, errf("%s: verification must be a string", id)
			}
			if slot.Classify(verification, nil).Scope == slot.ScopeFull {
				return nil, errf("%s: verification runs the whole suite: %s; name the changed packages' tests — "+
					"the full suite runs after integration", id, verification)
			}
			pkg.Verification = verification
		}
		switch n := entry["est_seconds"].(type) {
		case int64:
			pkg.EstSeconds = max(float64(n), 0)
		case float64:
			pkg.EstSeconds = max(n, 0)
		}
		if v, has := entry["brief"]; has {
			brief, isStr := v.(string)
			if !isStr {
				return nil, errf("%s: brief must be a string", id)
			}
			pkg.Brief = brief
		}
		if v, has := entry["risks"]; has {
			list, isList := stringList(v)
			if !isList {
				return nil, errf("%s: risks must be a list of strings", id)
			}
			pkg.Risks = list
		}
		if v, has := entry["interfaces"]; has {
			list, isList := stringList(v)
			if !isList {
				return nil, errf("%s: interfaces must be a list of interface ids", id)
			}
			for _, ifaceID := range list {
				if _, known := p.Interface(ifaceID); !known {
					return nil, errf("%s: unknown interface %s", id, strconv.Quote(ifaceID))
				}
			}
			pkg.Interfaces = list
		}
		if skeleton {
			_, hasRisks := entry["risks"]
			switch {
			case pkg.Brief == "":
				return nil, errf("%s: missing required field 'brief'", id)
			case !hasRisks:
				return nil, errf("%s: missing required field 'risks'", id)
			case pkg.EstSeconds < 1 || pkg.EstSeconds > 300:
				return nil, errf("%s: est_seconds must be between 1 and 300; split a larger package by file ownership", id)
			case len(pkg.FilesOwned) == 0:
				return nil, errf("%s: files_owned must name at least one path", id)
			case pkg.Verification == "":
				return nil, errf("%s: missing required field 'verification'", id)
			}
		}
		p.Packages = append(p.Packages, pkg)
	}

	external := map[string]bool{}
	for _, d := range p.ExternalDone {
		external[d] = true
	}
	for _, pkg := range p.Packages {
		for _, dep := range pkg.DependsOn {
			if !seen[dep] && !external[dep] {
				return nil, errf("%s: depends_on %s is unknown", pkg.ID, strconv.Quote(dep))
			}
		}
	}
	if err := checkCycles(p.Packages); err != nil {
		return nil, err
	}
	return p, nil
}

func contains(list []string, s string) bool {
	for _, e := range list {
		if e == s {
			return true
		}
	}
	return false
}

func checkCycles(packages []Package) error {
	deps := map[string][]string{}
	for _, p := range packages {
		deps[p.ID] = p.DependsOn
	}
	const (
		white = iota
		gray
		black
	)
	color := map[string]int{}
	var stack []string
	var visit func(id string) error
	visit = func(id string) error {
		c, known := color[id]
		if !known {
			c = black
			if _, isPkg := deps[id]; isPkg {
				c = white
			}
		}
		if c == black {
			return nil
		}
		if c == gray {
			start := 0
			for i, s := range stack {
				if s == id {
					start = i
					break
				}
			}
			cycle := append(append([]string(nil), stack[start:]...), id)
			return errf("dependency cycle: %s", strings.Join(cycle, " -> "))
		}
		color[id] = gray
		stack = append(stack, id)
		for _, dep := range deps[id] {
			if _, isPkg := deps[dep]; isPkg {
				if err := visit(dep); err != nil {
					return err
				}
			}
		}
		stack = stack[:len(stack)-1]
		color[id] = black
		return nil
	}
	for _, p := range packages {
		if color[p.ID] == white {
			if err := visit(p.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
