package cli

import (
	"os"
	"path/filepath"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/jsonx"
	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

func newSandboxCmd() *cobra.Command {
	sb := groupCommand("sandbox", "the OS sandbox around CLI executors")
	sub := func(name, short string, args cobra.PositionalArgs) *cobra.Command {
		cmd := &cobra.Command{
			Use:   name,
			Short: short,
			Args:  args,
			RunE:  notImplemented("sandbox " + name),
		}
		boolFlag(cmd, "json", "JSON output")
		sb.AddCommand(cmd)
		return cmd
	}
	strFlag(sub("list", "effective writable set, learned grants, deny_read", noArgs), "workdir", "directory (default cwd)")
	strFlag(sub("allow", "remember a user grant for this machine", cobra.ArbitraryArgs), "reason", "why")
	sub("forget", "remove a learned grant", cobra.ArbitraryArgs)
	strFlag(sub("allow-domain", "allow the host network to reach a domain", cobra.ArbitraryArgs), "reason", "why")
	sub("forget-domain", "remove a domain grant", cobra.ArbitraryArgs)
	strFlag(sub("probe", "which backend a run would use, and why", noArgs), "workdir", "directory (default cwd)")
	return sb
}

// learnedGrants is the `sandbox.learned` list `config` prints, plus a warning
// when the store is unreadable, corrupt, or holds entries that are dropped.
// A missing store is an empty list. Entries must be objects with an absolute
// path and a known source; the per-source policy checks (refused paths, auto
// grant rules) belong to the sandbox package.
func learnedGrants() ([]any, string) {
	path := filepath.Join(paths.StateDir(), "sandbox-learned.json")
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return []any{}, ""
	}
	var data any
	if err == nil {
		data, err = jsonx.Decode(raw)
	}
	if err != nil {
		return []any{}, "learned sandbox grants unreadable (" + path + "): " + err.Error()
	}
	root, _ := data.(*jsonx.Object)
	v, _ := root.Get("grants")
	grants, ok := v.([]any)
	if !ok {
		return []any{}, "learned sandbox grants corrupt (" + path + "): no grants list"
	}
	good := []any{}
	for _, g := range grants {
		entry, _ := g.(*jsonx.Object)
		p, _ := entry.Get("path")
		ps, _ := p.(string)
		src, _ := entry.Get("source")
		switch src {
		case "user", "instructor", "auto":
			if filepath.IsAbs(ps) {
				good = append(good, entry)
			}
		}
	}
	if dropped := len(grants) - len(good); dropped > 0 {
		noun := "entries"
		if dropped == 1 {
			noun = "entry"
		}
		return good, "learned sandbox grants: ignored " + strconv.Itoa(dropped) + " invalid " + noun + " in " + path
	}
	return good, ""
}
