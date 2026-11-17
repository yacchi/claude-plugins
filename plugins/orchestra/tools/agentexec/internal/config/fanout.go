package config

import "fmt"

// removedFanoutKeys maps the enforcement.fanout keys that counted a child's
// tool calls to the keys that count its model requests since 0.95.0.
var removedFanoutKeys = [][2]string{
	{"scout_calls", "scout_requests"},
	{"checker_calls", "checker_requests"},
	{"writer_calls", "writer_requests"},
}

// checkRemovedFanoutKeys rejects a config layer that still sets a *_calls key
// under enforcement.fanout: the budget counts model requests now, so the old
// number would silently mean something else.
func checkRemovedFanoutKeys(path string, layer map[string]any) error {
	enforcement, ok := layer["enforcement"].(map[string]any)
	if !ok {
		return nil
	}
	fanout, ok := enforcement["fanout"].(map[string]any)
	if !ok {
		return nil
	}
	for _, k := range removedFanoutKeys {
		if _, set := fanout[k[0]]; set {
			return fmt.Errorf("%s: enforcement.fanout.%s was removed in orchestra 0.95.0: the child budget counts model requests, not tool calls. "+
				"Rename it to enforcement.fanout.%s (same defaults: 15 / 8 / 40) and review the number, since one request may make several tool calls", path, k[0], k[1])
		}
	}
	return nil
}
