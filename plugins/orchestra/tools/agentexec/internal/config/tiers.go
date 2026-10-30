package config

import (
	"fmt"
	"maps"
	"math"
	"slices"
	"strings"
)

// tierEfforts are the reasoning-effort levels a tier may pin.
var tierEfforts = []string{"low", "medium", "high", "xhigh", "max"}

// plannerModels are the models tiers.planner may name: the planner is a
// Claude Code subagent that designs a whole programme, so only the aliases
// that plan well (and that the Agent tool accepts) are allowed.
var plannerModels = []string{"opus", "fable", "sonnet"}

// Tier resolves tiers.<cls> to its model and effort. A tier is either a
// string (the model; effort unset) or an object {model, effort}. Anything
// else, or a missing class, yields empty strings.
func Tier(r *Resolved, cls string) (model, effort string) {
	switch v := r.Section("tiers")[cls].(type) {
	case string:
		return v, ""
	case map[string]any:
		model, _ = v["model"].(string)
		effort, _ = v["effort"].(string)
		return model, effort
	}
	return "", ""
}

// validateTiers rejects a tier that is neither a model string nor a
// {model, effort} object, an object with an unknown key, an effort
// outside tierEfforts, or a planner model outside plannerModels. It runs once, in Resolve, so every reader of tiers
// (route, doctor, config) reports the same error.
func validateTiers(values map[string]any) error {
	tiers := ObjectAt(values, "tiers")
	keys := make([]string, 0, len(tiers))
	for k := range tiers {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, cls := range keys {
		where := "tiers." + cls
		var model any
		switch v := tiers[cls].(type) {
		case string:
			model = v
		case map[string]any:
			model = v["model"]
			subkeys := make([]string, 0, len(v))
			for k := range v {
				subkeys = append(subkeys, k)
			}
			slices.Sort(subkeys)
			for _, k := range subkeys {
				if k != "model" && k != "effort" {
					return fmt.Errorf("invalid %s: unknown key %q (allowed: model, effort)", where, k)
				}
			}
			if e, ok := v["effort"]; ok {
				s, isString := e.(string)
				if !isString || !slices.Contains(tierEfforts, s) {
					return fmt.Errorf("invalid %s.effort %v: must be one of %s",
						where, e, strings.Join(tierEfforts, ", "))
				}
			}
		default:
			return fmt.Errorf("invalid %s: must be a model string or an object {model, effort}", where)
		}
		if cls == "planner" {
			if s, isString := model.(string); !isString || !slices.Contains(plannerModels, s) {
				return fmt.Errorf("invalid %s.model %v: must be one of %s",
					where, model, strings.Join(plannerModels, ", "))
			}
		}
	}
	return validatePlanning(values)
}

// planningKeys are the keys the planning section may hold, in the order the
// allowed-keys message lists them.
var planningKeys = []string{"redo", "spec_writers", "writers_concurrent", "review", "redo_when"}

// planningClasses are the package classes planning.spec_writers may key.
var planningClasses = []string{"light", "standard", "deep"}

// redoWhenKeys are the redo gate thresholds planning.redo_when may hold.
var redoWhenKeys = []string{"structural_findings", "deep_packages", "interface_users"}

// PlanningTier is one {model, effort} pin of a planning stage.
type PlanningTier struct {
	Model  string
	Effort string
}

// RedoWhen are the redo gate's thresholds; 0 turns a trigger off.
type RedoWhen struct {
	StructuralFindings int
	DeepPackages       int
	InterfaceUsers     int
}

// PlanningConfig is tiers.planner plus the planning section.
type PlanningConfig struct {
	Skeleton          PlanningTier            // tiers.planner
	Redo              PlanningTier            // planning.redo
	SpecWriters       map[string]PlanningTier // planning.spec_writers, keyed light/standard/deep
	WritersConcurrent int                     // planning.writers_concurrent; 0 = no cap
	Review            bool                    // planning.review
	RedoWhen          RedoWhen                // planning.redo_when
}

// Planning reads tiers.planner and the planning section from the resolved
// values. validateTiers has already vouched for them, and absent keys give
// zero values: there are no defaults here, they live in defaults.go.
func Planning(r *Resolved) PlanningConfig {
	model, effort := Tier(r, "planner")
	review, _ := r.Bool("planning", "review")
	cfg := PlanningConfig{
		Skeleton:          PlanningTier{Model: model, Effort: effort},
		Redo:              planningTier(r, "planning", "redo"),
		SpecWriters:       map[string]PlanningTier{},
		WritersConcurrent: planningIntAt(r, "planning", "writers_concurrent"),
		Review:            review,
		RedoWhen: RedoWhen{
			StructuralFindings: planningIntAt(r, "planning", "redo_when", "structural_findings"),
			DeepPackages:       planningIntAt(r, "planning", "redo_when", "deep_packages"),
			InterfaceUsers:     planningIntAt(r, "planning", "redo_when", "interface_users"),
		},
	}
	for _, cls := range planningClasses {
		if _, ok := r.Lookup("planning", "spec_writers", cls); ok {
			cfg.SpecWriters[cls] = planningTier(r, "planning", "spec_writers", cls)
		}
	}
	return cfg
}

// EffortRank is the effort's index in low, medium, high, xhigh, max; -1 when
// unknown.
func EffortRank(effort string) int {
	return slices.Index(tierEfforts, effort)
}

// planningTier is the {model, effort} object at path; anything else reads as
// empty strings.
func planningTier(r *Resolved, path ...string) PlanningTier {
	v, _ := r.Lookup(path...)
	obj, _ := v.(map[string]any)
	model, _ := obj["model"].(string)
	effort, _ := obj["effort"].(string)
	return PlanningTier{Model: model, Effort: effort}
}

// planningIntAt is the whole number at path, or 0.
func planningIntAt(r *Resolved, path ...string) int {
	v, _ := r.Lookup(path...)
	n, _ := planningInt(v)
	return n
}

// planningInt is the whole number v holds. Loaders produce int64, but a layer
// may spell 8.0, which is a float64 that is still whole.
func planningInt(v any) (int, bool) {
	switch x := v.(type) {
	case int64:
		return int(x), true
	case int:
		return x, true
	case float64:
		if x == math.Trunc(x) && !math.IsInf(x, 0) {
			return int(x), true
		}
	}
	return 0, false
}

// validatePlanning checks the planning section in the order its keys are
// read, and returns the first error. Planning tiers are Claude subagents
// too, so they share plannerModels. An absent section is valid.
func validatePlanning(values map[string]any) error {
	v, ok := values["planning"]
	if !ok {
		return nil
	}
	planning, isObj := v.(map[string]any)
	if !isObj {
		return fmt.Errorf("invalid planning: must be an object")
	}
	for _, k := range slices.Sorted(maps.Keys(planning)) {
		if !slices.Contains(planningKeys, k) {
			return fmt.Errorf("invalid planning: unknown key %q (allowed: %s)",
				k, strings.Join(planningKeys, ", "))
		}
	}
	if redo, ok := planning["redo"]; ok {
		if err := validatePlanningTier("redo", redo); err != nil {
			return err
		}
	}
	if writers, ok := planning["spec_writers"]; ok {
		if err := validateSpecWriters(writers); err != nil {
			return err
		}
	}
	if wc, ok := planning["writers_concurrent"]; ok {
		if n, isInt := planningInt(wc); !isInt || n < 0 {
			return fmt.Errorf("invalid planning.writers_concurrent %v: must be a non-negative integer", wc)
		}
	}
	if review, ok := planning["review"]; ok {
		if _, isBool := review.(bool); !isBool {
			return fmt.Errorf("invalid planning.review %v: must be true or false", review)
		}
	}
	if when, ok := planning["redo_when"]; ok {
		return validateRedoWhen(when)
	}
	return nil
}

// validatePlanningTier checks one planning tier at path, which is both the
// key under planning and the name used in messages. Unlike tiers.*, both
// model and effort are required.
func validatePlanningTier(path string, v any) error {
	where := "planning." + path
	obj, isObj := v.(map[string]any)
	if !isObj {
		return fmt.Errorf("invalid %s: must be an object {model, effort}", where)
	}
	for _, k := range slices.Sorted(maps.Keys(obj)) {
		if k != "model" && k != "effort" {
			return fmt.Errorf("invalid %s: unknown key %q (allowed: model, effort)", where, k)
		}
	}
	model, isString := obj["model"].(string)
	if !isString || !slices.Contains(plannerModels, model) {
		return fmt.Errorf("invalid %s.model %v: must be one of %s",
			where, obj["model"], strings.Join(plannerModels, ", "))
	}
	effort, isString := obj["effort"].(string)
	if !isString || !slices.Contains(tierEfforts, effort) {
		return fmt.Errorf("invalid %s.effort %v: must be one of %s",
			where, obj["effort"], strings.Join(tierEfforts, ", "))
	}
	return nil
}

// validateSpecWriters checks planning.spec_writers: every class is known
// before any tier is checked.
func validateSpecWriters(v any) error {
	writers, isObj := v.(map[string]any)
	if !isObj {
		return fmt.Errorf("invalid planning.spec_writers: must be an object")
	}
	classes := slices.Sorted(maps.Keys(writers))
	for _, cls := range classes {
		if !slices.Contains(planningClasses, cls) {
			return fmt.Errorf("invalid planning.spec_writers: unknown class %q (allowed: %s)",
				cls, strings.Join(planningClasses, ", "))
		}
	}
	for _, cls := range classes {
		if err := validatePlanningTier("spec_writers."+cls, writers[cls]); err != nil {
			return err
		}
	}
	return nil
}

// validateRedoWhen checks planning.redo_when: unknown keys first, then the
// thresholds, each a non-negative integer (0 turns that trigger off).
func validateRedoWhen(v any) error {
	when, isObj := v.(map[string]any)
	if !isObj {
		return fmt.Errorf("invalid planning.redo_when: must be an object")
	}
	keys := slices.Sorted(maps.Keys(when))
	for _, k := range keys {
		if !slices.Contains(redoWhenKeys, k) {
			return fmt.Errorf("invalid planning.redo_when: unknown key %q (allowed: %s)",
				k, strings.Join(redoWhenKeys, ", "))
		}
	}
	for _, k := range keys {
		if n, isInt := planningInt(when[k]); !isInt || n < 0 {
			return fmt.Errorf("invalid planning.redo_when.%s %v: must be a non-negative integer", k, when[k])
		}
	}
	return nil
}
