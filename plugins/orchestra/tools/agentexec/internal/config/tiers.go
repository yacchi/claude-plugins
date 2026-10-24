package config

import (
	"fmt"
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
	return nil
}
