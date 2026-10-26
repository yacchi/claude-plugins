package capture

import (
	"context"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/executor/pi"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
)

// Request is one captured executor run. Resume "" starts a fresh session.
// Sandbox is the spec sandbox.Prepare made for (Executor, SandboxTree,
// Cfg, Model, Task); SandboxTree is the tree a re-prepared spec confines
// to (the worktree root when isolated, else the workdir). Class picks the
// watchdog's wall budget ("" = standard).
type Request struct {
	Executor, Model, Effort, Workdir, Prompt, Resume, Class, Task, SandboxTree string
	Sandbox                                                                    sandbox.Spec
	Cfg                                                                        *config.Resolved
}

// GrantCycleCap bounds the auto-grant + resume cycles of one run.
const GrantCycleCap = 2

// GrantResumePrompt is the prompt of a resume after auto-grants (%s: the
// granted paths, ", "-joined).
const GrantResumePrompt = "The sandbox blocked writes to: %s. Those paths are now writable. " +
	"Re-run the step that failed and continue the original task. If nothing " +
	"remains to do, repeat your previous final answer unchanged."

// prepareSpec re-prepares the spec after grants (a seam for tests).
var prepareSpec = sandbox.Prepare

// Refusal is the result a `sandbox.mode: required` refusal reports in place
// of a run.
func Refusal(s sandbox.Spec) map[string]any {
	return map[string]any{
		"status":     "unavailable",
		"answer":     nil,
		"session_id": nil,
		"reason":     "sandbox",
		"exit_code":  nil,
		"usage":      nil,
		"sandbox":    ReportObject(s),
	}
}

// ReportObject is sandbox.Report as an ordered object, so the grant loop
// can add denials, granted, cycles and grant_note.
func ReportObject(s sandbox.Spec) map[string]any {
	v, _ := sandbox.Report(s).(sandbox.ReportView)
	return map[string]any{
		"backend":  v.Backend,
		"enforced": v.Enforced,
		"reason":   v.Reason,
		"limits":   strs(v.Limits),
		"writable": strs(v.Writable),
	}
}

func strs(list []string) []any {
	out := make([]any, 0, len(list))
	for _, s := range list {
		out = append(out, s)
	}
	return out
}

// RunWithGrants runs one capture of r and returns its result (with
// `sandbox`: the spec report, plus denials/granted/cycles[/grant_note] for
// a sandboxed run) and the spec the last spawn used.
//
// A spec with Refuse set is never spawned: the result is Refusal. On
// sandbox denials, harmless cache paths are auto-granted, persisted, and
// the SAME session is resumed (at most GrantCycleCap times). A denial that
// is not auto-grantable makes a run that did not end ok `needs-permission`
// (reason "sandbox"); a run that worked around it stays ok with the
// denials attached as advisory. err is ErrExecutorNotFound when the binary
// does not resolve (exit 127, no result), ErrUnknownExecutor, ctx's error,
// or a spawn failure.
func RunWithGrants(ctx context.Context, r Request) (map[string]any, sandbox.Spec, error) {
	spec := r.Sandbox
	if r.Executor != "pi" {
		return nil, spec, fmt.Errorf("%w: %s", ErrUnknownExecutor, r.Executor)
	}
	if spec.Refuse {
		return Refusal(spec), spec, nil
	}
	if r.Workdir != "" {
		spec.Cwd = r.Workdir
	}
	result, inp, err := runner(ctx, r, spec, r.Prompt, r.Resume)
	if err != nil {
		return nil, spec, err
	}
	if spec.Policy == nil || inp == nil {
		result["sandbox"] = ReportObject(spec)
		return result, spec, nil
	}
	return grantLoop(ctx, r, spec, result, inp)
}

func grantLoop(ctx context.Context, r Request, spec sandbox.Spec, result map[string]any, inp *denialInput) (map[string]any, sandbox.Spec, error) {
	// The writable set as it stood when this run started: a denial on a
	// path granted DURING the run means the grant did not take.
	base := spec
	var granted []string
	byPath := map[string]map[string]any{} // path -> latest entry
	var pathOrder []string                // paths in first-seen order
	keep := func(path string, entry map[string]any) {
		if _, seen := byPath[path]; !seen {
			pathOrder = append(pathOrder, path)
		}
		byPath[path] = entry
	}
	cycles := 0
	grantNote := ""
	usage := result["usage"]
	for {
		var entries []map[string]any
		for _, d := range sandbox.DetectDenials(base, inp.Stdout, inp.Stderr) {
			entry := map[string]any{"path": d.Path, "op": d.Op}
			if d.Intentional {
				entry["intentional"] = true
				entry["auto"] = false
				entry["grant_candidate"] = nil
				entry["why"] = "denied on purpose (deny_read / tamper protection)"
				keep(d.Path, entry)
				continue
			}
			candidate := sandbox.GrantFor(d.Path)
			if candidate != "" {
				entry["grant_candidate"] = candidate
			} else {
				entry["grant_candidate"] = d.Path
			}
			switch {
			case underAny(d.Path, granted):
				entry["auto"] = false
				entry["why"] = "grant did not resolve the denial"
			case candidate == "":
				entry["auto"] = false
				entry["why"] = "not under an auto-grantable cache root"
			default:
				persistGrant(entry, candidate, d.Path, spec.Backend)
			}
			entries = append(entries, entry)
			keep(d.Path, entry)
		}
		if len(entries) == 0 {
			break
		}
		var newGrants []string
		blocked := false
		for _, e := range entries {
			c := e["grant_candidate"]
			cand, _ := c.(string)
			if g, _ := e["granted"]; g == true && !contains(granted, cand) && !contains(newGrants, cand) {
				newGrants = append(newGrants, cand)
			}
			if a, _ := e["auto"]; a != true {
				blocked = true
			}
		}
		sessionID := result["session_id"]
		sid, _ := sessionID.(string)
		if !blocked && len(newGrants) > 0 && cycles < GrantCycleCap && sid != "" {
			granted = append(granted, newGrants...)
			next := prepareSpec(r.Executor, r.SandboxTree, r.Cfg, r.Model, r.Task)
			if next.Refuse || next.Policy == nil {
				break
			}
			if r.Workdir != "" {
				next.Cwd = r.Workdir
			}
			spec = next
			nxt, nextInp, err := runner(ctx, r, spec, fmt.Sprintf(GrantResumePrompt, strings.Join(newGrants, ", ")), sid)
			cycles++
			if err != nil {
				if err == ErrExecutorNotFound {
					break
				}
				return nil, spec, err
			}
			if s, _ := nxt["session_id"]; s == nil || s == "" {
				// A resumed run that died before naming its session is still
				// the same session: keep it resumable for the next round.
				nxt["session_id"] = sid
			}
			if nextInp == nil {
				nextInp = &denialInput{}
			}
			inp = nextInp
			nu := nxt["usage"]
			usage = mergeUsage(usage, nu)
			nxt["usage"] = usage
			nxt["resumed"] = true
			prev := result["network_denials"]
			cur := nxt["network_denials"]
			if denied := mergeNetworkDenials(prev, cur); len(denied) > 0 {
				nxt["network_denials"] = denied
			}
			result = nxt
			continue
		}
		if !blocked && len(newGrants) > 0 {
			if sid == "" {
				grantNote = "granted; not resumed: no-session"
			} else if cycles >= GrantCycleCap {
				grantNote = "granted; not resumed: cycle-cap"
			}
		}
		if blocked {
			if st, _ := result["status"]; st != "ok" {
				result["status"] = "needs-permission"
				result["reason"] = "sandbox"
			}
		}
		break
	}

	out := ReportObject(spec)
	denials := make([]any, 0, len(byPath))
	var grantedOut []any
	seen := map[string]bool{}
	for _, k := range pathOrder {
		e := byPath[k]
		denials = append(denials, e)
		if g, _ := e["granted"]; g == true {
			c := e["grant_candidate"]
			if s, _ := c.(string); !seen[s] {
				seen[s] = true
				grantedOut = append(grantedOut, s)
			}
		}
	}
	if grantedOut == nil {
		grantedOut = []any{}
	}
	out["denials"] = denials
	out["granted"] = grantedOut
	out["cycles"] = int64(cycles)
	if grantNote != "" {
		out["grant_note"] = grantNote
	}
	result["sandbox"] = out
	return result, spec, nil
}

// persistGrant stores the auto grant of candidate for the denied path and
// records the outcome on entry. bwrap and Landlock can only open what
// exists, so a granted directory is created for them.
func persistGrant(entry map[string]any, candidate, denied, backend string) {
	if _, err := sandbox.AddGrantEntry(candidate, "auto", denied, ""); err != nil {
		entry["auto"] = false
		entry["why"] = "could not persist the grant: " + err.Error()
		return
	}
	entry["auto"] = true
	entry["granted"] = true
	if backend != "bwrap" && backend != "landlock" {
		return
	}
	if candidate == denied && isFile(denied) {
		return
	}
	if err := os.MkdirAll(candidate, 0o700); err != nil {
		entry["auto"] = false
		entry["why"] = "could not persist the grant: " + err.Error()
	}
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

func validUTF8(b []byte) bool { return utf8.Valid(b) }

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// underAny: path is one of dirs or lies below one (component boundary).
func underAny(path string, dirs []string) bool {
	for _, d := range dirs {
		if path == d || strings.HasPrefix(path, strings.TrimRight(d, "/")+"/") {
			return true
		}
	}
	return false
}

// mergeUsage sums two normalized usage objects across grant cycles: tokens
// key by key, other non-negative integers (cost_micro_usd) summed, anything
// else taken from b unless null. last_context_tokens is the size of the last
// request, not a total, so the later cycle wins rather than summing.
func mergeUsage(a, b any) any {
	ao, aok := a.(map[string]any)
	bo, bok := b.(map[string]any)
	if !aok || ao == nil {
		if bok && bo != nil {
			return config.CloneValue(bo)
		}
		return b
	}
	if !bok || bo == nil {
		return a
	}
	keys := slices.Sorted(maps.Keys(ao))
	for _, k := range slices.Sorted(maps.Keys(bo)) {
		if _, ok := ao[k]; !ok {
			keys = append(keys, k)
		}
	}
	out := map[string]any{}
	for _, k := range keys {
		va := ao[k]
		vb := bo[k]
		if k == "tokens" {
			tokens := map[string]any{}
			if t, ok := va.(map[string]any); ok && t != nil {
				tokens = maps.Clone(t)
			}
			if t, ok := vb.(map[string]any); ok && t != nil {
				for _, tk := range slices.Sorted(maps.Keys(t)) {
					v := t[tk]
					pi.AccumulateInt(tokens, tk, v)
				}
			}
			out[k] = tokens
			continue
		}
		na, aInt := va.(int64)
		nb, bInt := vb.(int64)
		switch {
		case k == "last_context_tokens" && vb != nil:
			out[k] = vb
		case aInt && bInt && na >= 0 && nb >= 0:
			out[k] = na + nb
		case vb != nil:
			out[k] = vb
		default:
			out[k] = va
		}
	}
	return out
}

// mergeNetworkDenials is a then the entries of b not already in it.
func mergeNetworkDenials(a, b any) []any {
	var out []any
	if l, ok := a.([]any); ok {
		out = append(out, l...)
	}
	key := func(v any) string {
		raw, _ := fsutil.MarshalJSON(v)
		return string(raw)
	}
	have := map[string]bool{}
	for _, d := range out {
		have[key(d)] = true
	}
	if l, ok := b.([]any); ok {
		for _, d := range l {
			if k := key(d); !have[k] {
				have[k] = true
				out = append(out, d)
			}
		}
	}
	return out
}
