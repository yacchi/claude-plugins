package conformance

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// bakeGoDeviations prepares a run for -update: go_swap's volatile object (or
// text line) is left out of the golden, since only its shape is checked, and
// go_replace needs nothing (it accepts a golden that already holds New).
func bakeGoDeviations(t *testing.T, sc *Scenario, got Golden) Golden {
	var out Golden
	b, err := json.Marshal(got)
	must(t, err)
	must(t, json.Unmarshal(b, &out))
	for i, st := range sc.Steps {
		sw := st.GoSwap
		if sw == nil || i >= len(out.Steps) {
			continue
		}
		switch v := out.Steps[i].Stdout.(type) {
		case map[string]any:
			delete(v, sw.Got)
		case string:
			var kept []string
			for _, l := range strings.Split(v, "\n") {
				if !strings.HasPrefix(l, sw.Got+": ") {
					kept = append(kept, l)
				}
			}
			out.Steps[i].Stdout = strings.Join(kept, "\n")
		}
	}
	return out
}

// applyGoDeviations edits the golden (w) and the output (g) of one step
// for the deviations the step declares (go_swap, go_replace), returning
// what did not hold.
func applyGoDeviations(st Step, w, g *GoldenStep) []string {
	var problems []string
	if sw := st.GoSwap; sw != nil {
		switch want := w.Stdout.(type) {
		case map[string]any:
			got, ok := g.Stdout.(map[string]any)
			if !ok {
				return append(problems, "go_swap: Go stdout is not a JSON object")
			}
			delete(want, sw.Golden)
			blk, ok := got[sw.Got].(map[string]any)
			if !ok {
				return append(problems, fmt.Sprintf("go_swap: Go stdout has no object %q", sw.Got))
			}
			keys := sortedKeys(blk)
			wantKeys := append([]string(nil), sw.Keys...)
			sort.Strings(wantKeys)
			if !reflect.DeepEqual(keys, wantKeys) {
				problems = append(problems, fmt.Sprintf("go_swap: %q has keys %v, want %v", sw.Got, keys, wantKeys))
			}
			delete(got, sw.Got)
		case string:
			got, _ := g.Stdout.(string)
			var kept []string
			for _, l := range strings.Split(want, "\n") {
				if !strings.HasPrefix(l, sw.Golden+": ") {
					kept = append(kept, l)
				}
			}
			w.Stdout = strings.Join(kept, "\n")
			kept = nil
			found := 0
			for _, l := range strings.Split(got, "\n") {
				if !strings.HasPrefix(l, sw.Got+": ") {
					kept = append(kept, l)
					continue
				}
				found++
				for _, k := range sw.Keys {
					if !strings.Contains(l, k+"=") {
						problems = append(problems, fmt.Sprintf("go_swap: line %q lacks %s=", l, k))
					}
				}
			}
			if found != 1 {
				problems = append(problems, fmt.Sprintf("go_swap: %d %q lines in Go stdout, want 1", found, sw.Got))
			}
			g.Stdout = strings.Join(kept, "\n")
		default:
			problems = append(problems, "go_swap: golden stdout is neither a JSON object nor text")
		}
	}
	if len(st.GoReplace) > 0 {
		text, ok := w.Stdout.(string)
		if !ok {
			return append(problems, "go_replace: golden stdout is not text")
		}
		for _, r := range st.GoReplace {
			if !strings.Contains(text, r.Old) {
				// A golden rewritten by -update already holds the Go text.
				if !strings.Contains(text, r.New) {
					problems = append(problems, fmt.Sprintf("go_replace: golden lacks %q", r.Old))
				}
				continue
			}
			text = strings.Replace(text, r.Old, r.New, 1)
		}
		w.Stdout = text
	}
	return problems
}
