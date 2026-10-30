package planning

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingReturnsZeroState(t *testing.T) {
	planPath := filepath.Join(t.TempDir(), "plan.json")

	s, err := Load(planPath)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if s.Version != 1 || s.Plan != planPath || s.Specs == nil || len(s.Specs) != 0 {
		t.Fatalf("zero state = %+v, want Version 1, Plan %s, empty non-nil Specs", s, planPath)
	}
	if _, err := os.Stat(StatePath(planPath)); !os.IsNotExist(err) {
		t.Fatalf("Load created %s: %v", StatePath(planPath), err)
	}
}

func TestLoadCorruptStateIsError(t *testing.T) {
	planPath := filepath.Join(t.TempDir(), "plan.json")
	if err := os.WriteFile(StatePath(planPath), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(planPath); err == nil {
		t.Fatal("Load of a corrupt state file returned no error")
	}
}
