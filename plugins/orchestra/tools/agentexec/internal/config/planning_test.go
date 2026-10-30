package config

import (
	"reflect"
	"testing"
)

// frozenPlanning is the planning block the spec freezes, built here rather
// than read from Defaults, which another change owns.
func frozenPlanning() map[string]any {
	return map[string]any{
		"tiers": map[string]any{
			"planner": map[string]any{"model": "opus", "effort": "xhigh"},
		},
		"planning": map[string]any{
			"redo": map[string]any{"model": "opus", "effort": "max"},
			"spec_writers": map[string]any{
				"light":    map[string]any{"model": "sonnet", "effort": "medium"},
				"standard": map[string]any{"model": "opus", "effort": "high"},
				"deep":     map[string]any{"model": "opus", "effort": "max"},
			},
			"writers_concurrent": int64(8),
			"review":             true,
			"redo_when": map[string]any{
				"structural_findings": int64(1),
				"deep_packages":       int64(3),
				"interface_users":     int64(5),
			},
		},
	}
}

func TestPlanningAcceptsFrozenBlock(t *testing.T) {
	values := frozenPlanning()
	if err := validateTiers(values); err != nil {
		t.Fatalf("frozen block rejected: %v", err)
	}
	want := PlanningConfig{
		Skeleton: PlanningTier{Model: "opus", Effort: "xhigh"},
		Redo:     PlanningTier{Model: "opus", Effort: "max"},
		SpecWriters: map[string]PlanningTier{
			"light":    {Model: "sonnet", Effort: "medium"},
			"standard": {Model: "opus", Effort: "high"},
			"deep":     {Model: "opus", Effort: "max"},
		},
		WritersConcurrent: 8,
		Review:            true,
		RedoWhen:          RedoWhen{StructuralFindings: 1, DeepPackages: 3, InterfaceUsers: 5},
	}
	if got := Planning(&Resolved{Values: values}); !reflect.DeepEqual(got, want) {
		t.Fatalf("Planning = %+v, want %+v", got, want)
	}
}

func TestPlanningRejectsMalformedKeys(t *testing.T) {
	cases := map[string]struct {
		planning any
		want     string
	}{
		"not an object": {
			planning: "yes",
			want:     "invalid planning: must be an object",
		},
		"unknown key": {
			planning: map[string]any{"fanout": int64(1)},
			want:     `invalid planning: unknown key "fanout" (allowed: redo, spec_writers, writers_concurrent, review, redo_when)`,
		},
		"tier without effort": {
			planning: map[string]any{"redo": map[string]any{"model": "opus"}},
			want:     "invalid planning.redo.effort <nil>: must be one of low, medium, high, xhigh, max",
		},
		"unlisted spec writer model": {
			planning: map[string]any{"spec_writers": map[string]any{
				"light": map[string]any{"model": "haiku", "effort": "low"},
			}},
			want: "invalid planning.spec_writers.light.model haiku: must be one of opus, fable, sonnet",
		},
		"unknown class": {
			planning: map[string]any{"spec_writers": map[string]any{
				"heavy": map[string]any{"model": "opus", "effort": "max"},
			}},
			want: `invalid planning.spec_writers: unknown class "heavy" (allowed: light, standard, deep)`,
		},
		"fractional writers_concurrent": {
			planning: map[string]any{"writers_concurrent": 2.5},
			want:     "invalid planning.writers_concurrent 2.5: must be a non-negative integer",
		},
		"string review": {
			planning: map[string]any{"review": "true"},
			want:     "invalid planning.review true: must be true or false",
		},
		"negative redo_when threshold": {
			planning: map[string]any{"redo_when": map[string]any{"deep_packages": int64(-1)}},
			want:     "invalid planning.redo_when.deep_packages -1: must be a non-negative integer",
		},
	}
	for name, tc := range cases {
		err := validateTiers(map[string]any{"planning": tc.planning})
		if err == nil || err.Error() != tc.want {
			t.Fatalf("%s: err=%v, want %q", name, err, tc.want)
		}
	}
}

func TestPlanningReadsWholeFloats(t *testing.T) {
	values := map[string]any{"planning": map[string]any{
		"writers_concurrent": float64(8),
		"redo_when":          map[string]any{"deep_packages": float64(3)},
	}}
	if err := validateTiers(values); err != nil {
		t.Fatalf("whole floats rejected: %v", err)
	}
	got := Planning(&Resolved{Values: values})
	if got.WritersConcurrent != 8 || got.RedoWhen.DeepPackages != 3 {
		t.Fatalf("whole floats read as %d and %d, want 8 and 3",
			got.WritersConcurrent, got.RedoWhen.DeepPackages)
	}
}

func TestPlanningWithoutSectionHasZeroValues(t *testing.T) {
	values := map[string]any{"tiers": map[string]any{
		"planner": map[string]any{"model": "sonnet", "effort": "high"},
	}}
	want := PlanningConfig{
		Skeleton:    PlanningTier{Model: "sonnet", Effort: "high"},
		SpecWriters: map[string]PlanningTier{},
	}
	if got := Planning(&Resolved{Values: values}); !reflect.DeepEqual(got, want) {
		t.Fatalf("Planning = %+v, want %+v", got, want)
	}
	if got := Planning(&Resolved{}); !reflect.DeepEqual(got, PlanningConfig{SpecWriters: map[string]PlanningTier{}}) {
		t.Fatalf("empty Resolved: Planning = %+v", got)
	}
}
