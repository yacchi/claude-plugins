package sandbox

import (
	"os"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
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
	return jsonx.NewObject().
		Set("mode", sc.Mode).
		Set("backend", spec.Backend).
		Set("enforced", spec.Enforced).
		Set("would_refuse", spec.Refuse).
		Set("reason", spec.Reason).
		Set("limits", strs(spec.Limits)).
		Set("probes", results.Object()).
		Set("learned", jsonx.NewObject().
			Set("path", LearnedPath()).
			Set("count", int64(len(grants))).
			Set("warning", nullable(warning)))
}

// ListSection is `sandbox list`: the effective writable set of a pi run in
// workdir, the learned grants and domains, deny_read, the backend.
func ListSection(cfg *config.Resolved, workdir string) *jsonx.Object {
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
	return jsonx.NewObject().
		Set("workdir", realPath(workdir)).
		Set("backend", spec.Backend).
		Set("enforced", spec.Enforced).
		Set("reason", spec.Reason).
		Set("limits", strs(spec.Limits)).
		Set("writable", strs(policy.Writable)).
		Set("deny_read", strs(policy.DenyRead)).
		Set("deny_write", strs(policy.DenyWrite)).
		Set("deny_write_regex", strs(policy.DenyWriteRegex)).
		Set("learned", learned).
		Set("learned_path", LearnedPath()).
		Set("domains", doms).
		Set("warning", nullable(warning))
}
