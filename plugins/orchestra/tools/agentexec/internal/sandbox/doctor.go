package sandbox

import (
	"os"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func strs(list []string) []any {
	out := make([]any, len(list))
	for i, s := range list {
		out[i] = s
	}
	return out
}

// DoctorSection is what this machine would use for a pi run in the current
// directory, and why (doctor's `sandbox` and `sandbox probe`).
func DoctorSection(cfg *config.Resolved) any {
	sc := ConfigOf(cfg)
	results := Probes()
	cwd, _ := os.Getwd()
	spec := prepare("pi", cwd, sc, &results, policyOpts{}, nil)
	grants, warning, _ := loadGrants("", nil)
	return map[string]any{
		"mode":         sc.Mode,
		"backend":      spec.Backend,
		"enforced":     spec.Enforced,
		"would_refuse": spec.Refuse,
		"reason":       spec.Reason,
		"limits":       strs(spec.Limits),
		"probes":       results.Object(),
		"learned": map[string]any{
			"path":    LearnedPath(),
			"count":   int64(len(grants)),
			"warning": nullable(warning),
		},
	}
}

// ListSection is `sandbox list`: the effective writable set of a pi run in
// workdir, the learned grants and domains, deny_read, the backend.
func ListSection(cfg *config.Resolved, workdir string) map[string]any {
	sc := ConfigOf(cfg)
	spec := prepare("pi", workdir, sc, nil, policyOpts{}, nil)
	policy := spec.Policy
	if policy == nil {
		policy = buildPolicy(workdir, "pi", sc, policyOpts{})
	}
	grants, warning, _ := loadGrants("", nil)
	learned := make([]any, len(grants))
	for i, g := range grants {
		learned[i] = g.raw
	}
	domains, _ := loadDomains("")
	doms := make([]any, len(domains))
	for i, d := range domains {
		doms[i] = d.raw
	}
	return map[string]any{
		"workdir":          realPath(workdir),
		"backend":          spec.Backend,
		"enforced":         spec.Enforced,
		"reason":           spec.Reason,
		"limits":           strs(spec.Limits),
		"writable":         strs(policy.Writable),
		"deny_read":        strs(policy.DenyRead),
		"deny_write":       strs(policy.DenyWrite),
		"deny_write_regex": strs(policy.DenyWriteRegex),
		"learned":          learned,
		"learned_path":     LearnedPath(),
		"domains":          doms,
		"warning":          nullable(warning),
	}
}
