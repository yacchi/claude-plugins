package watch

import (
	"errors"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"time"
)

// PlanRetention is how long a plan directory stays after its last change.
// Plans live in the durable state dir, so nothing else ever removes them; a
// week is long enough to come back to a plan and short enough that the
// directory does not grow forever.
const PlanRetention = 7 * 24 * time.Hour

// PlanSweep is one plan directory `isolate sweep` reclaimed, would reclaim
// under --dry-run, or could not reclaim.
type PlanSweep struct {
	Dir      string  `json:"dir"`
	Status   string  `json:"status"`           // removed | would-remove | error
	IdleDays float64 `json:"idle_days"`        // rounded to 0.1
	Reason   string  `json:"reason,omitempty"` // error only
}

// SweepPlans removes (os.RemoveAll), or under dryRun only reports, each
// subdirectory of dir whose newest modification time - the directory itself
// and everything under it, symlinks not followed - is at least PlanRetention
// before now, and that no live runner holds. A deep write keeps a plan: the
// age is the newest change anywhere in it, not the directory's own mtime. A
// live runner keeps its plan however idle it looks. Files and symlinks
// directly in dir are ignored, and so is a missing dir; kept directories are
// not listed. The result is sorted by Dir.
func SweepPlans(dir string, now time.Time, dryRun bool) []PlanSweep {
	out := []PlanSweep{}
	entries, err := os.ReadDir(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return out
	}
	if err != nil {
		return append(out, PlanSweep{Dir: dir, Status: "error", Reason: err.Error()})
	}
	for _, e := range entries {
		if e.Type()&fs.ModeSymlink != 0 || !e.IsDir() {
			continue
		}
		sub := filepath.Join(dir, e.Name())
		newest, found, err := sweepPlansNewest(sub)
		if err != nil {
			out = append(out, PlanSweep{Dir: sub, Status: "error", Reason: err.Error()})
			continue
		}
		if !found {
			continue // removed since ReadDir listed it
		}
		idle := now.Sub(newest)
		if idle < PlanRetention {
			continue
		}
		statePath := filepath.Join(sub, "state.json")
		if rec, ok := ReadRecord(statePath); ok && rec.Alive(statePath) {
			continue
		}
		entry := PlanSweep{Dir: sub, Status: "would-remove", IdleDays: math.Round(idle.Hours()/24*10) / 10}
		if !dryRun {
			entry.Status = "removed"
			if err := os.RemoveAll(sub); err != nil {
				entry = PlanSweep{Dir: sub, Status: "error", Reason: err.Error()}
			}
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Dir < out[j].Dir })
	return out
}

// sweepPlansNewest is the newest modification time in the tree at root, the
// root included. WalkDir lstats and never descends through a symlink, so a
// link into another tree neither keeps the plan alive nor gets removed with
// its target. found is false when root vanished before it was walked.
func sweepPlansNewest(root string) (newest time.Time, found bool, err error) {
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil // removed mid-walk
			}
			return err
		}
		info, err := d.Info()
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if !found || info.ModTime().After(newest) {
			newest, found = info.ModTime(), true
		}
		return nil
	})
	return newest, found, err
}
