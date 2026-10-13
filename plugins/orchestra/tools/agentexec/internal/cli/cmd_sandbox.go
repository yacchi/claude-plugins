package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	// srt registers sandbox's network planner: linked into every binary, so
	// sandbox.Prepare puts CLI executor runs under srt wherever it applies.
	_ "github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/srt"
)

func newSandboxCmd() *cobra.Command {
	sb := groupCommand("sandbox", "the OS sandbox around CLI executors")
	sub := func(name, short string, args cobra.PositionalArgs) (*cobra.Command, *optBool) {
		cmd := &cobra.Command{Use: name, Short: short, Args: args}
		asJSON := boolFlag(cmd, "json", "JSON output")
		sb.AddCommand(cmd)
		return cmd, asJSON
	}

	list, listJSON := sub("list", "effective writable set, learned grants, deny_read", noArgs)
	listWorkdir := strFlag(list, "workdir", "directory (default cwd)")
	// Python took `--workdir` only together with its value.
	list.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if strings.HasPrefix(err.Error(), "flag needs an argument: ") {
			return exitf(2, "%s: unknown option: --workdir", errPrefix(cmd))
		}
		return flagError(cmd, err)
	})
	list.RunE = func(cmd *cobra.Command, _ []string) error {
		workdir, _ := os.Getwd()
		if v, ok := listWorkdir.Get(); ok {
			workdir = v
		}
		info := sandbox.ListSection(configOrDefaults(), workdir)
		if listJSON.On() {
			return writeJSON(cmd.OutOrStdout(), info)
		}
		fmt.Fprint(cmd.OutOrStdout(), sbFormatList(info))
		return nil
	}

	allow, allowJSON := sub("allow", "remember a user grant for this machine", cobra.ArbitraryArgs)
	allowReason := strFlag(allow, "reason", "why")
	allow.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return exitf(2, "sandbox allow: expected one path")
		}
		cfg := configOrDefaults()
		path := sandbox.RealPath(sbExpandUser(args[0]))
		if refusal := sandbox.UserGrantRefusal(path, sandbox.DenyRead(cfg)); refusal != "" {
			return exitf(1, "sandbox allow: %s", refusal)
		}
		entry, err := sandbox.AddGrantEntry(path, "user", "", allowReason.Or(""))
		if err != nil {
			return exitf(1, "sandbox allow: %s", err)
		}
		if allowJSON.On() {
			return writeJSONLine(cmd.OutOrStdout(), entry.Object())
		}
		fmt.Fprintf(cmd.OutOrStdout(), "granted: %s\n", entry.Path)
		return nil
	}

	forget, forgetJSON := sub("forget", "remove a learned grant", cobra.ArbitraryArgs)
	forget.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return exitf(2, "sandbox forget: expected one path")
		}
		configOrDefaults()
		path := sandbox.RealPath(sbExpandUser(args[0]))
		err := sandbox.ForgetGrant(path)
		if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return exitf(1, "sandbox forget: %s", err)
		}
		return sbRemovedResult(cmd, forgetJSON.On(), "path", path, err == nil)
	}

	allowDomain, allowDomainJSON := sub("allow-domain", "allow the host network to reach a domain", cobra.ArbitraryArgs)
	domainReason := strFlag(allowDomain, "reason", "why")
	allowDomain.SetFlagErrorFunc(func(cmd *cobra.Command, err error) error {
		if strings.HasPrefix(err.Error(), "flag needs an argument: ") {
			return exitf(2, "sandbox allow-domain: --reason needs a value")
		}
		return flagError(cmd, err)
	})
	allowDomain.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return exitf(2, "sandbox allow-domain: expected one domain")
		}
		configOrDefaults()
		domain := strings.ToLower(strings.TrimSpace(args[0]))
		entry, err := sandbox.AddDomainEntry(domain, "user", domainReason.Or(""))
		if err != nil {
			return exitf(1, "sandbox allow-domain: %s", err)
		}
		if allowDomainJSON.On() {
			return writeJSONLine(cmd.OutOrStdout(), entry.Object())
		}
		fmt.Fprintf(cmd.OutOrStdout(),
			"allowed domain: %s (running herdr hosts and CLI runs pick it up live)\n", entry.Domain)
		return nil
	}

	forgetDomain, forgetDomainJSON := sub("forget-domain", "remove a domain grant", cobra.ArbitraryArgs)
	forgetDomain.RunE = func(cmd *cobra.Command, args []string) error {
		if len(args) != 1 {
			return exitf(2, "sandbox forget-domain: expected one domain")
		}
		configOrDefaults()
		domain := strings.ToLower(strings.TrimSpace(args[0]))
		err := sandbox.ForgetDomain(domain)
		if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return exitf(1, "sandbox forget-domain: %s", err)
		}
		return sbRemovedResult(cmd, forgetDomainJSON.On(), "domain", domain, err == nil)
	}

	probe, probeJSON := sub("probe", "which backend a run would use, and why", noArgs)
	strFlag(probe, "workdir", "directory (default cwd)") // accepted, ignored (as in Python)
	probe.RunE = func(cmd *cobra.Command, _ []string) error {
		info := sandbox.DoctorSection(configOrDefaults()).(*jsonx.Object)
		if probeJSON.On() {
			return writeJSON(cmd.OutOrStdout(), info)
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "mode=%s backend=%s enforced=%s (%s)\n", sbStr(sbGet(info, "mode")),
			sbStr(sbGet(info, "backend")), sbStr(sbGet(info, "enforced")), sbStr(sbGet(info, "reason")))
		probes, _ := sbGet(info, "probes").(*jsonx.Object)
		for _, name := range probes.Keys() {
			p := probes.Object(name)
			state := "unavailable"
			if ok, _ := sbGet(p, "ok").(bool); ok {
				state = "ok"
			}
			fmt.Fprintf(out, "  - %s: %s (%s)\n", name, state, sbStr(sbGet(p, "detail")))
		}
		return nil
	}
	return sb
}

// removedResult prints a forget's outcome; exit 1 when nothing was removed.
func sbRemovedResult(cmd *cobra.Command, asJSON bool, key, value string, removed bool) error {
	if asJSON {
		if err := writeJSONLine(cmd.OutOrStdout(), jsonx.NewObject().Set(key, value).Set("removed", removed)); err != nil {
			return err
		}
	} else if removed {
		fmt.Fprintf(cmd.OutOrStdout(), "removed: %s\n", value)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "no grant for: %s\n", value)
	}
	if !removed {
		return &ExitError{Code: 1}
	}
	return nil
}

func sbExpandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return home + p[1:]
		}
	}
	return p
}

func sbGet(o *jsonx.Object, k string) any {
	if o == nil {
		return nil
	}
	v, _ := o.Get(k)
	return v
}

// pyStr is Python's "%s" of a JSON value.
func sbStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "None"
	case string:
		return x
	case bool:
		if x {
			return "True"
		}
		return "False"
	}
	b, err := jsonx.Marshal(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func sbFormatList(info *jsonx.Object) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	strsOf := func(k string) []string {
		var out []string
		list, _ := sbGet(info, k).([]any)
		for _, v := range list {
			out = append(out, sbStr(v))
		}
		return out
	}
	objsOf := func(k string) []*jsonx.Object {
		var out []*jsonx.Object
		list, _ := sbGet(info, k).([]any)
		for _, v := range list {
			if o, ok := v.(*jsonx.Object); ok {
				out = append(out, o)
			}
		}
		return out
	}
	line("backend: %s (%s)", sbStr(sbGet(info, "backend")), sbStr(sbGet(info, "reason")))
	if limits := strsOf("limits"); len(limits) > 0 {
		line("limits: %s", strings.Join(limits, ", "))
	}
	line("writable (%s):", sbStr(sbGet(info, "workdir")))
	for _, w := range strsOf("writable") {
		line("  %s", w)
	}
	line("learned grants (%s):", sbStr(sbGet(info, "learned_path")))
	grants := objsOf("learned")
	for _, g := range grants {
		line("  %s  [%s, %s]", sbStr(sbGet(g, "path")), sbStr(sbGet(g, "source")), sbStr(sbGet(g, "added")))
	}
	if len(grants) == 0 {
		line("  (none)")
	}
	line("domain grants:")
	domains := objsOf("domains")
	for _, d := range domains {
		source := sbGet(d, "source")
		if s, ok := source.(string); !ok || s == "" {
			source = "user"
		}
		text := fmt.Sprintf("  %s  [%s, %s]", sbStr(sbGet(d, "domain")), sbStr(source), sbStr(sbGet(d, "added")))
		if r := sbGet(d, "reason"); r != nil && r != "" && r != false {
			text += "  " + sbStr(r)
		}
		line("%s", text)
	}
	if len(domains) == 0 {
		line("  (none)")
	}
	line("deny_read:")
	for _, r := range strsOf("deny_read") {
		line("  %s", r)
	}
	if w := sbGet(info, "warning"); w != nil && w != "" {
		line("WARNING: %s", sbStr(w))
	}
	return b.String()
}

// learnedGrants is the `sandbox.learned` list `config` prints, plus a
// warning when the store is unreadable, corrupt, or holds entries that are
// dropped (never trusted).
func learnedGrants() ([]any, string) {
	l, _ := sandbox.LoadLearned()
	out := make([]any, len(l.Grants))
	for i, g := range l.Grants {
		out[i] = g.Object()
	}
	return out, l.Warning
}
