// Package install implements `agent-exec install`: an interactive script
// that writes a PATH shim exec'ing the plugin's bootstrap.
package install

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/fsutil"
)

var rawBindirs = []string{"~/.local/bin", "/usr/local/bin", "~/bin", "~/.claude/bin"}

type session struct {
	in   *bufio.Reader
	out  io.Writer
	errw io.Writer
}

// prompt prints text and reads one line; EOF or a blank line gives def.
func (s *session) prompt(text, def string) string {
	fmt.Fprint(s.out, text)
	line, err := s.in.ReadString('\n')
	if err != nil && line == "" {
		return def
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return def
	}
	return line
}

func abs(p string) string {
	if a, err := filepath.Abs(p); err == nil {
		return a
	}
	return p
}

func pathDirsAbs() map[string]bool {
	out := map[string]bool{}
	for _, p := range filepath.SplitList(os.Getenv("PATH")) {
		if p != "" {
			out[abs(p)] = true
		}
	}
	return out
}

func isFile(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// resolveBootstrap picks the bootstrap the shim will exec: the single
// marketplace clone, else (several clones) the one in the same marketplace
// as the running bootstrap, else a menu choice; with no clone at all, the
// running bootstrap itself. "" means failure (already reported).
func (s *session) resolveBootstrap(self string) string {
	candidates, _ := filepath.Glob(filepath.Join(fsutil.ExpandUser("~/.claude/plugins/marketplaces"),
		"*", "plugins", "orchestra", "tools", "agent-exec"))
	sort.Strings(candidates)

	var bootstrap string
	switch {
	case len(candidates) == 1:
		bootstrap = candidates[0]
	case len(candidates) > 1:
		preferred := ""
		const marker = "/marketplaces/"
		if idx := strings.Index(self, marker); idx != -1 {
			name, _, _ := strings.Cut(self[idx+len(marker):], "/")
			needle := marker + name + "/"
			for _, c := range candidates {
				if strings.Contains(c, needle) {
					preferred = c
					break
				}
			}
		}
		if preferred != "" {
			bootstrap = preferred
			break
		}
		fmt.Fprintln(s.out, "Multiple orchestra plugin installs found:")
		for i, c := range candidates {
			fmt.Fprintf(s.out, "  %d) %s\n", i+1, c)
		}
		choice := s.prompt("Choose one [1]: ", "1")
		i, err := strconv.Atoi(choice)
		if err != nil || i < 1 || i > len(candidates) {
			i = 1
		}
		bootstrap = candidates[i-1]
	default:
		if self == "" {
			fmt.Fprintln(s.errw, "agent-exec: install: cannot locate the agent-exec bootstrap "+
				"(no marketplace clone found and AGENT_EXEC_BOOTSTRAP is not set)")
			return ""
		}
		bootstrap = self
		if strings.Contains(bootstrap, "/cache/") {
			fmt.Fprintf(s.out, "WARNING: no marketplace clone found; falling back to the "+
				"version-stamped cache path:\n  %s\n"+
				"This shim will BREAK on the next plugin update. Re-run "+
				"'agent-exec install' from the marketplace clone once "+
				"available.\n", bootstrap)
		}
	}

	if !isFile(bootstrap) {
		fmt.Fprintf(s.errw, "agent-exec: install: resolved bootstrap does not exist: %s\n", bootstrap)
		return ""
	}
	return abs(bootstrap)
}

func (s *session) chooseBindir() string {
	onPath := pathDirsAbs()
	candidates := make([]string, len(rawBindirs))
	for i, c := range rawBindirs {
		candidates[i] = fsutil.ExpandUser(c)
	}
	defaultIdx := 0
	found := false
	for i, c := range candidates {
		if onPath[abs(c)] && abs(c) == abs(fsutil.ExpandUser("~/.local/bin")) {
			defaultIdx, found = i, true
			break
		}
	}
	if !found {
		for i, c := range candidates {
			if onPath[abs(c)] {
				defaultIdx = i
				break
			}
		}
	}

	fmt.Fprintln(s.out, "Choose an install directory for the agent-exec shim:")
	for i, c := range candidates {
		tag := "(NOT on PATH)"
		if onPath[abs(c)] {
			tag = "(on PATH)"
		}
		marker := ""
		if i == defaultIdx {
			marker = " [default]"
		}
		fmt.Fprintf(s.out, "  %d) %s %s%s\n", i+1, c, tag, marker)
	}
	fmt.Fprintf(s.out, "  %d) enter a custom absolute path\n", len(candidates)+1)

	choice := s.prompt(fmt.Sprintf("Choice [%d]: ", defaultIdx+1), strconv.Itoa(defaultIdx+1))
	i, err := strconv.Atoi(choice)
	if err != nil {
		i = defaultIdx + 1
	}
	if i == len(candidates)+1 {
		custom := s.prompt("Custom absolute path: ", "")
		if custom == "" {
			return candidates[defaultIdx]
		}
		return fsutil.ExpandUser(custom)
	}
	if i >= 1 && i <= len(candidates) {
		return candidates[i-1]
	}
	return candidates[defaultIdx]
}

// Run is the interactive installer. bootstrap is the running agent-exec
// bootstrap ($AGENT_EXEC_BOOTSTRAP, "" when unknown). It returns the exit
// code: 0 installed, 1 declined or failed.
func Run(in io.Reader, out, errw io.Writer, bootstrap string) int {
	s := &session{in: bufio.NewReader(in), out: out, errw: errw}
	bootstrap = s.resolveBootstrap(bootstrap)
	if bootstrap == "" {
		return 1
	}

	bindir := s.chooseBindir()
	if err := os.MkdirAll(bindir, 0o755); err != nil {
		fmt.Fprintf(errw, "agent-exec: install: cannot create %s: %v\n", bindir, err)
		return 1
	}
	target := filepath.Join(bindir, "agent-exec")

	if _, err := os.Stat(target); err == nil {
		answer := strings.ToLower(s.prompt(fmt.Sprintf("%s already exists. Overwrite? (y/N): ", target), "n"))
		if answer != "y" && answer != "yes" {
			fmt.Fprintf(out, "Aborted: not overwriting existing %s\n", target)
			return 1
		}
	}

	shim := fmt.Sprintf("#!/bin/sh\nexec \"%s\" \"$@\"\n", bootstrap)
	if err := os.WriteFile(target, []byte(shim), 0o644); err != nil {
		fmt.Fprintf(errw, "agent-exec: install: cannot write %s: %v\n", target, err)
		return 1
	}
	if err := os.Chmod(target, 0o755); err != nil {
		fmt.Fprintf(errw, "agent-exec: install: cannot chmod %s: %v\n", target, err)
		return 1
	}

	fmt.Fprintf(out, "installed %s -> %s\n", target, bootstrap)
	if !pathDirsAbs()[abs(bindir)] {
		fmt.Fprintln(out, "")
		fmt.Fprintf(out, "WARNING: %s is not on your PATH.\n", bindir)
		fmt.Fprintln(out, "Add this to your shell rc file:")
		fmt.Fprintf(out, "  export PATH=\"%s:$PATH\"\n", bindir)
	}
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Required: add \"Bash(agent-exec:*)\" to permissions.allow in")
	fmt.Fprintln(out, "  .claude/settings.json (project) or ~/.claude/settings.json (user)")
	fmt.Fprintln(out, "  You can also run /permissions in Claude Code to add it.")
	fmt.Fprintln(out, "")
	fmt.Fprintln(out, "Quick test: agent-exec doctor")
	return 0
}
