// Package approvals lists and decides the sandbox approvals of a wave
// without an LLM inside the runner. Three kinds of pending item, each with
// a stable id:
//
//   - `net:<host>`  a host the network allowlist blocked for some package (or
//     that a worker named in `ESCALATE: network <host>`), not quiet, not
//     allowed already and not decided before in this wave;
//   - `path:<pkg>:<n>`  denial n of a package's `permission` need;
//   - `dialog:<pkg>`  an interactive session stopped on a permission dialog.
//
// Decide applies one decision -- a domain or path grant, a key sent to the
// dialog, or a note telling the worker to finish without it -- moves the
// affected package back to the runner, and appends the decision to
// `<orchestra state dir>/audit/decisions.jsonl`. The hard refusals of the
// sandbox package apply to every decider. The runner itself never decides.
package approvals

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/host/herdr"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/state"
)

// File names and accepted values.
const (
	ApprovalsName  = "approvals.json"
	DecisionsAudit = "decisions.jsonl"
	DefaultPort    = 443
)

// Bys are the accepted deciders; Decisions the accepted decisions.
var (
	Bys       = []string{"instructor", "user"}
	Decisions = []string{"allow", "deny"}
)

var (
	// First line of a worker answer asking for network hosts:
	// `ESCALATE: network <host>[, <host>...]` (an interactive host may
	// double the ESCALATE prefix).
	netEscalateRe = regexp.MustCompile(`^(?:ESCALATE:\s*)+network\s+(\S.*)$`)
	hostRe        = regexp.MustCompile(`^[a-z0-9*]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+(:[0-9]{1,5})?$`)
	// A host ends in an alphabetic TLD of at least two letters (`i.e` is not one).
	tldRe      = regexp.MustCompile(`\.[a-z]{2,}(:[0-9]{1,5})?$`)
	terminalRe = regexp.MustCompile(`\nterminal: ([^\n]+)`)
	tokenSplit = regexp.MustCompile(`[,\s]+`)
)

// NotePath is the decision notes file for a package's next dispatch (the
// runner sends it along, then sets it aside).
func NotePath(stateDir, pid string) string {
	return filepath.Join(stateDir, "corrections", pid+".approvals.md")
}

// NetworkEscalateHosts parses an `ESCALATE: network <host>[, ...]` first
// line into [{"host", "port"}] objects; nil when the answer is not one (a
// plain ESCALATE stays an escalate). The host list ends at the first token
// that is not a host, so trailing prose never yields a bogus host.
func NetworkEscalateHosts(answer string) []map[string]any {
	first := ""
	for _, line := range pySplitLines(answer) {
		if t := strings.TrimSpace(line); t != "" {
			first = t
			break
		}
	}
	m := netEscalateRe.FindStringSubmatch(first)
	if m == nil {
		return nil
	}
	var out []map[string]any
	for _, token := range tokenSplit.Split(m[1], -1) {
		text := strings.ToLower(strings.Trim(token, "`'\"()[]<>;:."))
		if text == "" {
			continue
		}
		if strings.HasPrefix(text, "https://") || strings.HasPrefix(text, "http://") {
			text = strings.SplitN(strings.SplitN(text, "://", 2)[1], "/", 2)[0]
		}
		if !hostRe.MatchString(text) || !tldRe.MatchString(text) {
			break
		}
		h, port := text, DefaultPort
		if i := strings.LastIndex(text, ":"); i >= 0 {
			h = text[:i]
			port, _ = strconv.Atoi(text[i+1:])
		}
		dup := false
		for _, e := range out {
			if e["host"] == h && e["port"] == port {
				dup = true
			}
		}
		if !dup {
			out = append(out, map[string]any{"host": h, "port": port})
		}
	}
	return out
}

// pySplitLines is str.splitlines for the separators answers carry.
func pySplitLines(s string) []string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	s = strings.TrimSuffix(s, "\n")
	if s == "" {
		return nil
	}
	return strings.Split(s, "\n")
}

// --- config / stores ---------------------------------------------------------

// resolveConfig is the resolved config, or the defaults when a layer is
// broken (approvals keep working on a broken config, like the Python tool).
func resolveConfig() *config.Resolved {
	cfg, err := config.Resolve()
	if err != nil || cfg == nil {
		return &config.Resolved{Values: config.Defaults()}
	}
	return cfg
}

func stateDirOf(statePath string) string {
	abs, err := filepath.Abs(statePath)
	if err != nil {
		abs = statePath
	}
	return filepath.Dir(abs)
}

func absOf(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func approvalsPath(statePath string) string {
	return filepath.Join(stateDirOf(statePath), ApprovalsName)
}

// record is approvals.json: {"allowed": {host: {...}}, "declined": {...}}
// plus whatever else the file holds.
type record map[string]any

func loadApprovals(statePath string) record {
	data := record{}
	if raw, err := os.ReadFile(approvalsPath(statePath)); err == nil {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil && m != nil {
			data = m
		}
	}
	for _, k := range []string{"allowed", "declined"} {
		if _, ok := data[k].(map[string]any); !ok {
			data[k] = map[string]any{}
		}
	}
	return data
}

func (r record) has(key, host string) bool {
	_, ok := r[key].(map[string]any)[host]
	return ok
}

func saveApprovals(statePath string, data record) error {
	b, err := jsonx.MarshalIndent(map[string]any(data))
	if err != nil {
		return err
	}
	return fsutil.AtomicWriteFile(approvalsPath(statePath), append(b, '\n'), 0o644)
}

// DecisionsAuditPath is the machine-wide decision log.
func DecisionsAuditPath() string {
	return filepath.Join(sandbox.StateDir(), sandbox.AuditDir, DecisionsAudit)
}

func audit(rec map[string]any) error {
	path := DecisionsAuditPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return fsutil.AppendJSONLine(path, rec, 0o644)
}

// specs maps package ids to spec paths from the plan and the runner's
// fix-ups.
func specs(st *state.State, statePath string) map[string]any {
	out := map[string]any{}
	if p, err := plan.Load(st.Plan); err == nil {
		for _, pkg := range p.Packages {
			out[pkg.ID] = pkg.Spec
		}
	}
	raw, err := os.ReadFile(filepath.Join(stateDirOf(statePath), "fixups.json"))
	if err != nil {
		return out
	}
	var fixups struct {
		Packages []any `json:"packages"`
	}
	if json.Unmarshal(raw, &fixups) != nil {
		return out
	}
	for _, e := range fixups.Packages {
		m, ok := e.(map[string]any)
		if !ok {
			continue
		}
		id, _ := m["id"].(string)
		if _, seen := out[id]; !seen {
			out[id] = m["spec"]
		}
	}
	return out
}

func needJSON(n state.Need) map[string]any {
	var m map[string]any
	if json.Unmarshal([]byte(n.Detail), &m) != nil {
		return nil
	}
	return m
}

// --- listing -----------------------------------------------------------------

// PkgRef is one package a network item concerns.
type PkgRef struct {
	ID     string
	Status string
	Spec   any
}

func (p PkgRef) MarshalJSON() ([]byte, error) {
	return jsonx.Marshal(jsonx.NewObject().Set("id", p.ID).Set("status", p.Status).Set("spec", p.Spec))
}

// Item is one pending approval. Kind net uses Host, Ports, Packages and
// FirstSeen; path uses Pkg, Path, GrantCandidate and Why; dialog uses Pkg,
// Dialog, Terminal and HostName. Values copied from need details stay as
// decoded (a null path is nil).
type Item struct {
	ID, Kind  string
	Host      string
	Ports     []any
	Packages  []PkgRef
	FirstSeen any

	Pkg                       string
	Path, GrantCandidate, Why any

	Dialog   string
	Terminal any
	HostName string

	Spec any
}

func (it Item) MarshalJSON() ([]byte, error) {
	o := jsonx.NewObject().Set("id", it.ID).Set("kind", it.Kind)
	switch it.Kind {
	case "net":
		pkgs := it.Packages
		if pkgs == nil {
			pkgs = []PkgRef{}
		}
		o.Set("host", it.Host).Set("ports", it.Ports).Set("packages", pkgs).Set("first_seen", it.FirstSeen)
	case "path":
		o.Set("pkg", it.Pkg).Set("path", it.Path).Set("grant_candidate", it.GrantCandidate).Set("why", it.Why)
	default:
		o.Set("pkg", it.Pkg).Set("dialog", it.Dialog).Set("terminal", it.Terminal).Set("host", it.HostName)
	}
	return jsonx.Marshal(o.Set("spec", it.Spec))
}

// hostState classifies a host: "quiet", "decided" (in this wave),
// "allowed" (configured or learned), or "" (pending).
func hostState(net sandbox.NetworkConfig, ap record) func(string) string {
	allowed := append(append([]string{}, net.AllowedDomains...), sandbox.LearnedDomains()...)
	return func(h string) string {
		switch {
		case sandbox.DomainMatches(h, net.QuietDomains):
			return "quiet"
		case ap.has("allowed", h) || ap.has("declined", h):
			return "decided"
		case sandbox.DomainMatches(h, allowed):
			return "allowed"
		}
		return ""
	}
}

func firstSeen(statePath string) map[string]any {
	seen := map[string]any{}
	for _, ev := range state.ReadEvents(statePath, -1) {
		if kind, _ := ev.Get("event"); kind != "network-denied" {
			continue
		}
		detail, _ := ev.Get("detail")
		text, _ := detail.(string)
		var d struct {
			Denials []any `json:"denials"`
		}
		if json.Unmarshal([]byte(text), &d) != nil {
			continue
		}
		at, _ := ev.Get("at")
		for _, e := range d.Denials {
			m, _ := e.(map[string]any)
			h, _ := m["host"].(string)
			if _, ok := seen[h]; h != "" && !ok {
				seen[h] = at
			}
		}
	}
	return seen
}

// dialogOf is the (dialog text, terminal) of a dialog-stalled need.
func dialogOf(n state.Need) (text string, terminal any, ok bool) {
	var detail string
	switch n.Kind {
	case "stalled":
		detail = n.Detail
	case "delegate":
		data := needJSON(n)
		if data == nil || data["orca"] != "stalled" {
			return "", nil, false
		}
		detail = pyStr(data["detail"])
	default:
		return "", nil, false
	}
	if !strings.HasPrefix(detail, "dialog:") {
		return "", nil, false
	}
	text = detail[len("dialog:"):]
	if loc := terminalRe.FindStringSubmatchIndex(detail); loc != nil {
		text = detail[len("dialog:"):max(loc[0], len("dialog:"))]
		terminal = strings.TrimSpace(detail[loc[2]:loc[3]])
	}
	return strings.Trim(text, "\n"), terminal, true
}

func dialogHost(stateDir, pid string, p *state.Package) string {
	if p != nil && p.Executor != nil && (*p.Executor == "herdr" || *p.Executor == "orca") {
		return *p.Executor
	}
	if raw, err := os.ReadFile(filepath.Join(stateDir, herdr.RegistryName)); err == nil {
		var m map[string]any
		if json.Unmarshal(raw, &m) == nil {
			if _, ok := m[pid]; ok {
				return "herdr"
			}
		}
	}
	return "orca"
}

// Pending is every pending approval of the wave at statePath, in a stable
// order (hosts sorted, then the needs in recorded order), under the
// resolved config.
func Pending(statePath string) ([]Item, error) { return PendingIn(statePath, resolveConfig()) }

// PendingIn is Pending under cfg.
func PendingIn(statePath string, cfg *config.Resolved) ([]Item, error) {
	store, err := state.Open(statePath)
	if err != nil {
		return nil, err
	}
	st, err := store.Load()
	if err != nil {
		return nil, err
	}
	sp := specs(&st, statePath)
	stateOf := hostState(sandbox.ConfigOf(cfg).Network, loadApprovals(statePath))
	items := []Item{}

	type hostEntry struct {
		ports []any
		pkgs  []PkgRef
	}
	hosts := map[string]*hostEntry{}
	ids := make([]string, 0, len(st.Packages))
	for id := range st.Packages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, pid := range ids {
		p := st.Packages[pid]
		for _, d := range p.NetworkDenials {
			m := asMap(d)
			if m == nil {
				continue
			}
			h := strings.TrimRight(strings.ToLower(strings.TrimSpace(pyStrOr(m["host"]))), ".")
			if h == "" || stateOf(h) != "" {
				continue
			}
			e := hosts[h]
			if e == nil {
				e = &hostEntry{}
				hosts[h] = e
			}
			if !containsAny(e.ports, m["port"]) {
				e.ports = append(e.ports, m["port"])
			}
			known := false
			for _, ref := range e.pkgs {
				known = known || ref.ID == pid
			}
			if !known {
				e.pkgs = append(e.pkgs, PkgRef{ID: pid, Status: p.Status, Spec: sp[pid]})
			}
		}
	}
	var seen map[string]any
	if len(hosts) > 0 {
		seen = firstSeen(statePath)
	}
	names := make([]string, 0, len(hosts))
	for h := range hosts {
		names = append(names, h)
	}
	sort.Strings(names)
	for _, h := range names {
		e := hosts[h]
		ports := append([]any{}, e.ports...)
		sort.SliceStable(ports, func(i, j int) bool { return pyLess(ports[i], ports[j]) })
		items = append(items, Item{
			ID: "net:" + h, Kind: "net", Host: h, Ports: ports, Packages: e.pkgs,
			FirstSeen: seen[h], Spec: e.pkgs[0].Spec,
		})
	}

	stateDir := stateDirOf(statePath)
	for _, n := range st.Needs {
		if n.ID == nil {
			continue
		}
		pid := *n.ID
		if n.Kind == "permission" {
			data := needJSON(n)
			denials, _ := data["denials"].([]any)
			for i, d := range denials {
				m, ok := d.(map[string]any)
				if !ok {
					continue
				}
				items = append(items, Item{
					ID: fmt.Sprintf("path:%s:%d", pid, i), Kind: "path", Pkg: pid,
					Path: m["path"], GrantCandidate: m["grant_candidate"], Why: m["why"], Spec: sp[pid],
				})
			}
			continue
		}
		if text, terminal, ok := dialogOf(n); ok {
			items = append(items, Item{
				ID: "dialog:" + pid, Kind: "dialog", Pkg: pid, Dialog: text, Terminal: terminal,
				HostName: dialogHost(stateDir, pid, st.Packages[pid]), Spec: sp[pid],
			})
		}
	}
	return items, nil
}

// FormatLine is the one-line text form of an item.
func FormatLine(it Item) string {
	switch it.Kind {
	case "net":
		ports := make([]string, len(it.Ports))
		for i, p := range it.Ports {
			ports[i] = pyStr(p)
		}
		pkgs := make([]string, len(it.Packages))
		for i, p := range it.Packages {
			pkgs[i] = fmt.Sprintf("%s(%s)", p.ID, p.Status)
		}
		return fmt.Sprintf("%s  ports=%s  packages=%s", it.ID, strings.Join(ports, ","), strings.Join(pkgs, ","))
	case "path":
		return fmt.Sprintf("%s  %s  grant=%s  why=%s", it.ID, pyStr(it.Path), pyStr(it.GrantCandidate), pyStr(it.Why))
	}
	var tail []string
	for _, ln := range pySplitLines(it.Dialog) {
		if strings.TrimSpace(ln) != "" {
			tail = append(tail, ln)
		}
	}
	if len(tail) > 3 {
		tail = tail[len(tail)-3:]
	}
	return fmt.Sprintf("%s  %s terminal=%s  %s", it.ID, it.HostName, pyStr(it.Terminal), strings.Join(tail, " / "))
}
