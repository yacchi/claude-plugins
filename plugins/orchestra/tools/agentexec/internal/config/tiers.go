package config

import (
	"fmt"
	"slices"
	"strings"
)

// tierEfforts are the reasoning-effort levels a tier may pin.
var tierEfforts = []string{"low", "medium", "high", "xhigh", "max"}

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
// {model, effort} object, an object with an unknown key, or an effort
// outside tierEfforts. It runs once, in Resolve, so every reader of tiers
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
		switch v := tiers[cls].(type) {
		case string:
		case map[string]any:
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
	}
	return nil
}
