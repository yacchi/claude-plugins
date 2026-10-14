package cli

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/sandbox"
	// srt registers sandbox's network planner: linked into every binary, so
	// sandbox.Prepare puts CLI executor runs under srt wherever it applies.
	_ "github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/srt"
)

func newSandboxCmd() *cobra.Command {
	sb := groupCommand("sandbox", "the OS sandbox around CLI executors")
	sub := func(name, short string, args cobra.PositionalArgs) (*cobra.Command, *bool) {
		cmd := &cobra.Command{Use: name, Short: short, Args: args}
		asJSON := cmd.Flags().Bool("json", false, "JSON output")
		sb.AddCommand(cmd)
		return cmd, asJSON
	}
	onePositional := func(what string) cobra.PositionalArgs {
		return func(cmd *cobra.Command, args []string) error {
			if len(args) != 1 {
				return exitf(2, "%s: expected one %s", cmdName(cmd), what)
			}
			return nil
		}
	}

	list, listJSON := sub("list", "effective writable set, learned grants, deny_read", cobra.NoArgs)
	wd, _ := os.Getwd()
	listWorkdir := list.Flags().String("workdir", wd, "directory (default cwd)")
	list.RunE = func(cmd *cobra.Command, _ []string) error {
		info := sandbox.ListSection(configOrDefaults(), *listWorkdir)
		if *listJSON {
			return writeJSON(cmd.OutOrStdout(), info)
		}
		fmt.Fprint(cmd.OutOrStdout(), sbFormatList(info))
		return nil
	}

	allow, allowJSON := sub("allow", "remember a user grant for this machine", onePositional("path"))
	allowReason := allow.Flags().String("reason", "", "why")
	allow.RunE = func(cmd *cobra.Command, args []string) error {
		cfg := configOrDefaults()
		path := sandbox.RealPath(sbExpandUser(args[0]))
		if refusal := sandbox.UserGrantRefusal(path, sandbox.DenyRead(cfg)); refusal != "" {
			return exitf(1, "sandbox allow: %s", refusal)
		}
		entry, err := sandbox.AddGrantEntry(path, "user", "", *allowReason)
		if err != nil {
			return exitf(1, "sandbox allow: %s", err)
		}
		if *allowJSON {
			return writeJSONLine(cmd.OutOrStdout(), entry.Object())
		}
		fmt.Fprintf(cmd.OutOrStdout(), "granted: %s\n", entry.Path)
		return nil
	}

	forget, forgetJSON := sub("forget", "remove a learned grant", onePositional("path"))
	forget.RunE = func(cmd *cobra.Command, args []string) error {
		configOrDefaults()
		path := sandbox.RealPath(sbExpandUser(args[0]))
		err := sandbox.ForgetGrant(path)
		if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return exitf(1, "sandbox forget: %s", err)
		}
		return sbRemovedResult(cmd, *forgetJSON, "path", path, err == nil)
	}

	allowDomain, allowDomainJSON := sub("allow-domain", "allow the host network to reach a domain", onePositional("domain"))
	domainReason := allowDomain.Flags().String("reason", "", "why")
	allowDomain.RunE = func(cmd *cobra.Command, args []string) error {
		configOrDefaults()
		domain := config.PyLower(strings.TrimSpace(args[0]))
		entry, err := sandbox.AddDomainEntry(domain, "user", *domainReason)
		if err != nil {
			return exitf(1, "sandbox allow-domain: %s", err)
		}
		if *allowDomainJSON {
			return writeJSONLine(cmd.OutOrStdout(), entry.Object())
		}
		fmt.Fprintf(cmd.OutOrStdout(),
			"allowed domain: %s (running herdr hosts and CLI runs pick it up live)\n", entry.Domain)
		return nil
	}

	forgetDomain, forgetDomainJSON := sub("forget-domain", "remove a domain grant", onePositional("domain"))
	forgetDomain.RunE = func(cmd *cobra.Command, args []string) error {
		configOrDefaults()
		domain := config.PyLower(strings.TrimSpace(args[0]))
		err := sandbox.ForgetDomain(domain)
		if err != nil && !errors.Is(err, sandbox.ErrNotFound) {
			return exitf(1, "sandbox forget-domain: %s", err)
		}
		return sbRemovedResult(cmd, *forgetDomainJSON, "domain", domain, err == nil)
	}

	probe, probeJSON := sub("probe", "which backend a run would use, and why", cobra.NoArgs)
	// The backend choice does not depend on the directory; --workdir is
	// accepted so probe takes the same arguments as list.
	probe.Flags().String("workdir", "", "directory (default cwd; does not affect the result)")
	probe.RunE = func(cmd *cobra.Command, _ []string) error {
		info := sandbox.DoctorSection(configOrDefaults()).(map[string]any)
		if *probeJSON {
			return writeJSON(cmd.OutOrStdout(), info)
		}
		out := cmd.OutOrStdout()
		fmt.Fprintf(out, "mode=%s backend=%s enforced=%s (%s)\n", sbStr(info["mode"]),
			sbStr(info["backend"]), sbStr(info["enforced"]), sbStr(info["reason"]))
		probes, _ := info["probes"].(map[string]any)
		for _, name := range config.OrderedKeys(probes, sandbox.ProbeOrder) {
			p := config.ObjectAt(probes, name)
			state := "unavailable"
			if ok, _ := p["ok"].(bool); ok {
				state = "ok"
			}
			fmt.Fprintf(out, "  - %s: %s (%s)\n", name, state, sbStr(p["detail"]))
		}
		return nil
	}
	return sb
}

// removedResult prints a forget's outcome; exit 1 when nothing was removed.
func sbRemovedResult(cmd *cobra.Command, asJSON bool, key, value string, removed bool) error {
	if asJSON {
		if err := writeJSONLine(cmd.OutOrStdout(), map[string]any{key: value, "removed": removed}); err != nil {
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

// sbStr renders a JSON value for the text output: strings as is, null as
// "-", anything else as JSON.
func sbStr(v any) string {
	switch x := v.(type) {
	case nil:
		return "-"
	case string:
		return x
	}
	b, err := fsutil.MarshalJSON(v)
	if err != nil {
		return fmt.Sprint(v)
	}
	return string(b)
}

func sbFormatList(info map[string]any) string {
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }
	strsOf := func(k string) []string {
		var out []string
		list, _ := info[k].([]any)
		for _, v := range list {
			out = append(out, sbStr(v))
		}
		return out
	}
	objsOf := func(k string) []map[string]any {
		var out []map[string]any
		list, _ := info[k].([]any)
		for _, v := range list {
			if o, ok := v.(map[string]any); ok {
				out = append(out, o)
			}
		}
		return out
	}
	line("backend: %s (%s)", sbStr(info["backend"]), sbStr(info["reason"]))
	if limits := strsOf("limits"); len(limits) > 0 {
		line("limits: %s", strings.Join(limits, ", "))
	}
	line("writable (%s):", sbStr(info["workdir"]))
	for _, w := range strsOf("writable") {
		line("  %s", w)
	}
	line("learned grants (%s):", sbStr(info["learned_path"]))
	grants := objsOf("learned")
	for _, g := range grants {
		line("  %s  [%s, %s]", sbStr(g["path"]), sbStr(g["source"]), sbStr(g["added"]))
	}
	if len(grants) == 0 {
		line("  (none)")
	}
	line("domain grants:")
	domains := objsOf("domains")
	for _, d := range domains {
		source := d["source"]
		if s, ok := source.(string); !ok || s == "" {
			source = "user"
		}
		text := fmt.Sprintf("  %s  [%s, %s]", sbStr(d["domain"]), sbStr(source), sbStr(d["added"]))
		if r, _ := d["reason"].(string); r != "" {
			text += "  " + r
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
	if w := info["warning"]; w != nil && w != "" {
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
