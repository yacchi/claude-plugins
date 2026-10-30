// Package advise proposes permanent repository fixes for settings that only
// cost something under parallel development. The advice holds until the repo
// is fixed: there is no setting to silence it and no dismissed state.
package advise

import "github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/wave/plan"

// Advice is one proposed repo fix. Problem names the setting, Why says what it
// costs under parallel work, and Fix is the concrete change to propose.
type Advice struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Problem string `json:"problem"`
	Why     string `json:"why"`
	Fix     string `json:"fix"`
}

// finders are the repo-wide checks, in the order their advice is listed. Each
// returns the advice for the repo at root, in a deterministic order. A new
// piece of repo advice is one more finder here.
var finders = []func(root string) []Advice{
	tsJestAdvice,
}

// Repo is the advice for the repository at root, empty when there is none. It
// never returns nil, so JSON output is a list, not null. An empty root (no
// repository) has no advice.
func Repo(root string) []Advice {
	out := []Advice{}
	if root == "" {
		return out
	}
	for _, find := range finders {
		out = append(out, find(root)...)
	}
	return out
}

// tsJestAdvice proposes making ts-jest transform-only for each jest config
// that type-checks every test file in every worker.
func tsJestAdvice(root string) []Advice {
	var out []Advice
	for _, rel := range plan.TypeCheckingJestConfigs(root) {
		out = append(out, Advice{
			ID:      "ts-jest-typecheck",
			Path:    rel,
			Problem: "ts-jest type-checks every test file in every jest worker",
			Why:     "jest is a test runner: type checking per worker multiplies a full tsc program by workers x parallel trees and exhausted host memory; it adds nothing a single tsc run does not",
			Fix:     "make the tests transform-only (ts-jest with isolatedModules: true, or @swc/jest) and add a typecheck script running tsc --noEmit once",
		})
	}
	return out
}
