package state

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

func stopPath(statePath string) string {
	abs, err := filepath.Abs(statePath)
	if err != nil {
		abs = statePath
	}
	return filepath.Join(filepath.Dir(abs), "STOP")
}

// StopRequested reports whether a STOP file sits beside the state file.
func StopRequested(statePath string) bool {
	st, err := os.Stat(stopPath(statePath))
	return err == nil && st.Mode().IsRegular()
}

// RequestStop writes the STOP file (its content is the reason) the runner
// polls between steps.
func RequestStop(statePath, reason string) error {
	p := stopPath(statePath)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(reason), 0o644)
}

// ClearStop removes the STOP file; a missing one is fine.
func ClearStop(statePath string) error {
	err := os.Remove(stopPath(statePath))
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// Registration is one waves.jsonl line: a wave `wave run` has started.
type Registration struct {
	State        string  `json:"state"`
	Plan         string  `json:"plan"`
	Repo         string  `json:"repo"`
	Into         string  `json:"into"`
	RegisteredAt float64 `json:"registered_at"`
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

// Register appends a registry line (paths made absolute; RegisteredAt 0 =
// now). The line's keys are sorted like Python's sort_keys.
func Register(r Registration) error {
	if r.RegisteredAt == 0 {
		r.RegisteredAt = wallClock()
	}
	rec := map[string]any{
		"state": abs(r.State), "plan": abs(r.Plan), "repo": abs(r.Repo),
		"into": r.Into, "registered_at": r.RegisteredAt,
	}
	line, err := jsonx.Marshal(rec)
	if err != nil {
		return err
	}
	path := paths.WaveRegistryPath()
	if err := os.MkdirAll(filepath.Dir(abs(path)), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(line, '\n'))
	return err
}

// ListWaves returns the registered waves, most recent registration first,
// deduplicated by state (the last line wins). Entries whose state file is
// gone and truncated or malformed lines are skipped.
func ListWaves() []Registration {
	f, err := os.Open(paths.WaveRegistryPath())
	if err != nil {
		return nil
	}
	defer f.Close()
	var order []string
	latest := map[string]Registration{}
	reader := bufio.NewReader(f)
	for {
		line, rerr := reader.ReadString('\n')
		line = strings.TrimSpace(line)
		if line != "" {
			if doc, err := jsonx.Decode([]byte(line)); err == nil {
				if o, ok := doc.(*jsonx.Object); ok {
					g := func(k string) any { v, _ := o.Get(k); return v }
					if st := str(g("state")); st != "" {
						reg := Registration{
							State: st, Plan: str(g("plan")), Repo: str(g("repo")),
							Into: str(g("into")), RegisteredAt: num(g("registered_at")),
						}
						if _, seen := latest[st]; seen {
							for i, k := range order {
								if k == st {
									order = append(order[:i], order[i+1:]...)
									break
								}
							}
						}
						order = append(order, st)
						latest[st] = reg
					}
				}
			}
		}
		if rerr != nil {
			break
		}
	}
	var out []Registration
	for i := len(order) - 1; i >= 0; i-- {
		reg := latest[order[i]]
		if st, err := os.Stat(reg.State); err == nil && st.Mode().IsRegular() {
			out = append(out, reg)
		}
	}
	return out
}
