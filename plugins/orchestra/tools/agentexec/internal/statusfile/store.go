package statusfile

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

var (
	runKeyPattern = regexp.MustCompile(`^[0-9a-f]{16}$`)
	planIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
)

// Store is the status directory under Root (normally ~/.claude/orchestra):
// Root/status.json, Root/status/runs/<key>.json, Root/status/plans/<plan id>.json
// and the rebuild lock Root/status/.lock.
type Store struct{ Root string }

// Default is the store under ~/.claude/orchestra.
func Default() Store {
	return Store{Root: paths.OrchestraHome()}
}

// Path is the machine-wide status.json.
func (s Store) Path() string {
	return filepath.Join(s.Root, "status.json")
}

// RunsDir holds one file per wave run.
func (s Store) RunsDir() string {
	return filepath.Join(s.Root, "status", "runs")
}

// PlansDir holds one file per plan.
func (s Store) PlansDir() string {
	return filepath.Join(s.Root, "status", "plans")
}

// RunKey identifies a wave run by its state file. It hashes the resolved
// path, so a symlink and its target give the same key.
func RunKey(statePath string) string {
	sum := sha1.Sum([]byte(paths.RealPath(statePath)))
	return hex.EncodeToString(sum[:])[:16]
}

// WriteRun writes r to its run file. It takes no lock.
func (s Store) WriteRun(r Run) error {
	if !runKeyPattern.MatchString(r.Key) {
		return fmt.Errorf("statusfile: invalid run key %q", r.Key)
	}
	return writeJSON(s.RunsDir(), r.Key+".json", r)
}

// ReadRun reads the run file for key; false when it is missing or does not decode.
func (s Store) ReadRun(key string) (Run, bool) {
	var r Run
	if !runKeyPattern.MatchString(key) {
		return r, false
	}
	if err := fsutil.ReadJSONFile(filepath.Join(s.RunsDir(), key+".json"), &r); err != nil {
		return Run{}, false
	}
	return r, true
}

// WritePlan writes p to its plan file. It takes no lock.
func (s Store) WritePlan(p Plan) error {
	if !planIDPattern.MatchString(p.PlanID) {
		return fmt.Errorf("statusfile: invalid plan id %q", p.PlanID)
	}
	return writeJSON(s.PlansDir(), p.PlanID+".json", p)
}

// ReadPlan reads the plan file for id; false when it is missing or does not decode.
func (s Store) ReadPlan(id string) (Plan, bool) {
	var p Plan
	if !planIDPattern.MatchString(id) {
		return p, false
	}
	if err := fsutil.ReadJSONFile(filepath.Join(s.PlansDir(), id+".json"), &p); err != nil {
		return Plan{}, false
	}
	return p, true
}

// Rebuild reads every run and plan file, prunes the expired ones and writes
// status.json from the rest.
func (s Store) Rebuild(now time.Time) (Doc, error) {
	statusDir := filepath.Join(s.Root, "status")
	if err := os.MkdirAll(statusDir, 0o755); err != nil {
		return Doc{}, err
	}
	unlock, err := fsutil.Lock(filepath.Join(statusDir, ".lock"), fsutil.Exclusive, 10*time.Second)
	if err != nil {
		return Doc{}, err
	}
	defer unlock()

	runs := loadDir(s.RunsDir(), now, func(r Run) int64 { return r.UpdatedAt })
	plans := loadDir(s.PlansDir(), now, func(p Plan) int64 { return p.UpdatedAt })

	doc := Build(runs, plans, now.Unix(), fsutil.PIDAlive)
	if err := fsutil.AtomicWriteJSON(s.Path(), doc, 0o644); err != nil {
		return Doc{}, err
	}
	return doc, nil
}

// loadDir decodes the *.json files in dir. A file that does not decode is
// skipped and deleted once its mtime is past FileKeepSeconds; a decoded entry
// whose updated time is past it is deleted too. Dotfiles (atomic-write temp
// files) are left alone. A missing dir holds nothing.
func loadDir[T any](dir string, now time.Time, updated func(T) int64) []T {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	cutoff := now.Unix() - FileKeepSeconds
	var out []T
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || strings.HasPrefix(name, ".") || filepath.Ext(name) != ".json" {
			continue
		}
		file := filepath.Join(dir, name)
		var v T
		if err := fsutil.ReadJSONFile(file, &v); err != nil {
			if info, statErr := os.Stat(file); statErr == nil && info.ModTime().Unix() < cutoff {
				os.Remove(file)
			}
			continue
		}
		if updated(v) < cutoff {
			os.Remove(file)
			continue
		}
		out = append(out, v)
	}
	return out
}

func writeJSON(dir, name string, v any) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return fsutil.AtomicWriteJSON(filepath.Join(dir, name), v, 0o644)
}
