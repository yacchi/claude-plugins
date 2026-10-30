package checks

import "testing"

func TestCapJestOnTestPool(t *testing.T) {
	if got := capJestOnTestPool("pnpm exec jest && tsc --noEmit", 3); got != "pnpm exec jest --maxWorkers=3 && tsc --noEmit" {
		t.Errorf("test-pool line: got %q", got)
	}
	if got := capJestOnTestPool("pnpm exec jest && tsc --noEmit", 0); got != "pnpm exec jest && tsc --noEmit" {
		t.Errorf("cap 0: got %q", got)
	}
	if got := capJestOnTestPool("pnpm install && pnpm exec jest", 3); got != "pnpm install && pnpm exec jest" {
		t.Errorf("heavy-pool line: got %q, want unchanged", got)
	}
}
