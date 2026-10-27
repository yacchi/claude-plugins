package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/isolate"
)

// The reasons `isolate request show` gives when the session has no open request.
const (
	requestReasonNone = "no open request for this session"
)

// requestJSON is the request object of `isolate request show` and `list`.
type requestJSON struct {
	ID      string     `json:"id"`
	Session string     `json:"session"`
	Created string     `json:"created"`
	Dir     string     `json:"dir"`
	Repos   []repoJSON `json:"repos"`
}

type repoJSON struct {
	Checkout    string            `json:"checkout"`
	Common      string            `json:"common"`
	Name        string            `json:"name"`
	Assembly    string            `json:"assembly"`
	Task        string            `json:"task"`
	Uncommitted string            `json:"uncommitted"`
	Files       []string          `json:"files"`
	Alive       bool              `json:"alive"`
	Integration []integrationJSON `json:"integration"`
}

type integrationJSON struct {
	Task     string `json:"task"`
	Path     string `json:"path"`
	Children int    `json:"children"`
}

func newIsolateRequestCmd() *cobra.Command {
	cmd := groupCommand("request", "the session's request workspace: its assemblies and integration trees")
	cmd.AddCommand(
		newIsolateRequestStartCmd(),
		newIsolateRequestShowCmd(),
		newIsolateRequestListCmd(),
	)
	return cmd
}

func newIsolateRequestStartCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "start",
		Short: "start or join the session's request: its assembly, and the --into integration tree",
		Args:  cobra.NoArgs,
	}
	fl := cmd.Flags()
	into := fl.String("into", "", "integration tree task id, cut from the assembly's HEAD")
	uncommitted := fl.String("uncommitted", "", "include|ignore: what to do with the checkout's uncommitted changes")
	repo := repoFlag(cmd)
	f := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		switch *uncommitted {
		case "", isolate.UncommittedInclude, isolate.UncommittedIgnore:
		default:
			return exitf(2, "isolate request start: --uncommitted must be include or ignore, got %q", *uncommitted)
		}
		var r isolate.EnsureResult
		var err error
		if cmd.Flags().Changed("into") {
			r, err = isolate.EnsureIntegration(*repo, *into, *uncommitted)
		} else {
			r, err = isolate.EnsureAssembly(*repo, *uncommitted)
		}
		if err != nil {
			return usageErr(cmd, err)
		}
		if err := writeResult(cmd.OutOrStdout(), f.is("text"), r, func() string {
			return requestStartText(r)
		}); err != nil {
			return err
		}
		switch r.Status {
		case "ready":
			return nil
		case "uncommitted":
			return &ExitError{Code: 1}
		}
		return &ExitError{Code: 3}
	}
	return cmd
}

func newIsolateRequestShowCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "the session's open request, or --request <id>",
		Args:  cobra.NoArgs,
	}
	id := cmd.Flags().String("request", "", "request id (default: the session's open request)")
	f := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		var (
			r      isolate.Request
			ok     bool
			err    error
			reason = requestReasonNone
		)
		if cmd.Flags().Changed("request") {
			r, ok, err = isolate.FindRequest(*id)
			reason = fmt.Sprintf("no open request %s", *id)
		} else {
			r, ok, err = isolate.CurrentRequest()
		}
		if err != nil {
			return requestError(cmd, f, err.Error())
		}
		if !ok {
			if err := writeResult(cmd.OutOrStdout(), f.is("text"), map[string]string{"status": "absent", "reason": reason}, func() string {
				return "absent: " + reason
			}); err != nil {
				return err
			}
			return &ExitError{Code: 1}
		}
		out := struct {
			Status string `json:"status"`
			requestJSON
		}{Status: "open", requestJSON: requestOf(r)}
		return writeResult(cmd.OutOrStdout(), f.is("text"), out, func() string {
			return strings.Join(requestText(out.requestJSON), "\n")
		})
	}
	return cmd
}

func newIsolateRequestListCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "every open request",
		Args:  cobra.NoArgs,
	}
	f := formatFlags(cmd, "json", "text")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		reqs, err := isolate.ListRequests()
		if err != nil {
			return requestError(cmd, f, err.Error())
		}
		out := make([]requestJSON, 0, len(reqs))
		for _, r := range reqs {
			out = append(out, requestOf(r))
		}
		return writeResult(cmd.OutOrStdout(), f.is("text"), map[string]any{"requests": out}, func() string {
			if len(out) == 0 {
				return "no open requests"
			}
			var lines []string
			for _, r := range out {
				lines = append(lines, requestText(r)...)
			}
			return strings.Join(lines, "\n")
		})
	}
	return cmd
}

// requestError reports a request operation that could not run: exit 3, as
// the status-error object in the chosen format.
func requestError(cmd *cobra.Command, f format, reason string) error {
	if err := writeResult(cmd.OutOrStdout(), f.is("text"), map[string]string{"status": "error", "reason": reason}, func() string {
		return "error: " + reason
	}); err != nil {
		return err
	}
	return &ExitError{Code: 3}
}

// requestOf is the JSON view of r. Its lists are never nil, so they encode as [].
func requestOf(r isolate.Request) requestJSON {
	out := requestJSON{
		ID:      r.ID,
		Session: r.Session,
		Created: r.Created,
		Dir:     r.Dir,
		Repos:   []repoJSON{},
	}
	for _, rr := range r.Repos {
		repo := repoJSON{
			Checkout:    rr.Checkout,
			Common:      rr.Common,
			Name:        rr.Name,
			Assembly:    rr.Assembly,
			Task:        rr.Task,
			Uncommitted: rr.Uncommitted,
			Files:       nonNilStrings(rr.Files),
			Alive:       isolate.AssemblyAlive(rr),
			Integration: []integrationJSON{},
		}
		if repo.Alive {
			repo.Integration = integrationOf(rr.Assembly)
		}
		out.Repos = append(out.Repos, repo)
	}
	return out
}

// integrationOf lists the integration trees cut from assembly, each with its
// own open children. A tree that cannot be listed reports none rather than
// failing the whole request view.
func integrationOf(assembly string) []integrationJSON {
	list := []integrationJSON{}
	kids, err := isolate.Children(assembly, assembly)
	if err != nil {
		return list
	}
	for _, c := range kids {
		grand, _ := isolate.Children(assembly, c.Path)
		list = append(list, integrationJSON{Task: c.Task, Path: c.Path, Children: len(grand)})
	}
	return list
}

// requestText is the --text rendering of one request, one line per repository
// and per integration tree.
func requestText(r requestJSON) []string {
	lines := []string{fmt.Sprintf("request %s  %s", r.ID, r.Dir)}
	for _, rr := range r.Repos {
		line := fmt.Sprintf("  %s -> %s [%s]", rr.Checkout, rr.Assembly, rr.Uncommitted)
		if !rr.Alive {
			line += " (gone)"
		}
		lines = append(lines, line)
		for _, in := range rr.Integration {
			lines = append(lines, fmt.Sprintf("    %s  %s  children %d", in.Task, in.Path, in.Children))
		}
	}
	return lines
}

// requestStartText is the --text rendering of an EnsureAssembly or
// EnsureIntegration result.
func requestStartText(r isolate.EnsureResult) string {
	switch r.Status {
	case "ready":
		s := fmt.Sprintf("ready: request %s, assembly %s (%s)", r.Request, r.Assembly, r.Uncommitted)
		if r.Into != "" {
			s += fmt.Sprintf(", integration %s at %s", r.Into, r.Integration)
		}
		if len(r.Created) > 0 {
			s += " [created: " + strings.Join(r.Created, ", ") + "]"
		}
		return s
	case "uncommitted":
		return "uncommitted: " + r.Reason
	}
	return "error: " + r.Reason
}
