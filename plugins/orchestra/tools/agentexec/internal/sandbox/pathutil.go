package sandbox

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/paths"
)

// realPath is Python's os.path.realpath (non-strict), step for step: the
// components are walked left to right, every symlink met is resolved
// before a following ".." is applied, and a component that does not exist
// is kept as given. The policy's paths must be spelled exactly as the
// Python implementation spelled them (a Seatbelt rule on another spelling
// silently never matches), so this does not use filepath.EvalSymlinks,
// which cleans ".." lexically first.
func realPath(name string) string {
	if name == "" {
		name = "."
	}
	rest := reversed(strings.Split(name, "/"))
	path := "/"
	if !strings.HasPrefix(name, "/") {
		wd, err := os.Getwd()
		if err != nil {
			wd = "/"
		}
		path = wd
	}
	type marker struct{ link string }
	// stack holds unresolved parts; a marker records that the symlink below
	// it has been fully resolved once it is popped.
	stack := make([]any, 0, len(rest))
	for _, r := range rest {
		stack = append(stack, r)
	}
	parts := len(rest)
	seen := map[string]*string{}
	for parts > 0 {
		top := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if m, ok := top.(marker); ok {
			resolved := path
			seen[m.link] = &resolved
			continue
		}
		part := top.(string)
		parts--
		if part == "" || part == "." {
			continue
		}
		if part == ".." {
			if i := strings.LastIndex(path, "/"); i > 0 {
				path = path[:i]
			} else {
				path = "/"
			}
			continue
		}
		newpath := path + "/" + part
		if path == "/" {
			newpath = "/" + part
		}
		info, err := os.Lstat(newpath)
		if err != nil || info.Mode()&fs.ModeSymlink == 0 {
			path = newpath
			continue
		}
		if cached, ok := seen[newpath]; ok {
			if cached != nil {
				path = *cached
				continue
			}
			path = newpath // a symlink loop
			continue
		}
		target, err := os.Readlink(newpath)
		if err != nil {
			path = newpath
			continue
		}
		if strings.HasPrefix(target, "/") {
			path = "/"
		}
		seen[newpath] = nil
		stack = append(stack, marker{newpath})
		tparts := reversed(strings.Split(target, "/"))
		for _, p := range tparts {
			stack = append(stack, p)
		}
		parts += len(tparts)
	}
	return path
}

// RealPath is Python's os.path.realpath (see realPath).
func RealPath(p string) string { return realPath(p) }

func reversed(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[len(in)-1-i] = s
	}
	return out
}

// pyJoin is os.path.join for two parts: no cleaning, an absolute second
// part replaces the first.
func pyJoin(a string, more ...string) string {
	out := a
	for _, b := range more {
		switch {
		case strings.HasPrefix(b, "/"):
			out = b
		case out == "" || strings.HasSuffix(out, "/"):
			out += b
		default:
			out += "/" + b
		}
	}
	return out
}

// pyDirname is os.path.dirname.
func pyDirname(p string) string {
	i := strings.LastIndex(p, "/") + 1
	head := p[:i]
	if head != "" && strings.Trim(head, "/") != "" {
		head = strings.TrimRight(head, "/")
	}
	return head
}

// absPath is os.path.abspath: lexical, no symlinks.
func absPath(p string) string {
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// expand resolves a leading "~" against home, then realpaths.
func expand(p, home string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = home + p[1:]
	}
	return realPath(p)
}

// expandUser is os.path.expanduser for "~" and "~/..." against $HOME.
func expandUser(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		return paths.Home() + p[1:]
	}
	return p
}

// dedupe keeps the first occurrence of every non-empty entry.
func dedupe(list []string) []string {
	out := []string{}
	seen := map[string]bool{}
	for _, p := range list {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}

// under: path is dir or lies below it (component boundary).
func under(path, dir string) bool {
	return path == dir || strings.HasPrefix(path, strings.TrimRight(dir, "/")+"/")
}

func overlaps(a, b string) bool { return under(a, b) || under(b, a) }

// homeOf is the realpath of home, or of $HOME when home is empty.
func homeOf(home string) string {
	if home == "" {
		home = paths.Home()
	}
	return realPath(home)
}

func lexists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isLink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&fs.ModeSymlink != 0
}

// realIfExists is realpath when something (even a dangling link) is there,
// else the path as given (srt follows no symlink for an allow).
func realIfExists(p string) string {
	if lexists(p) {
		return realPath(p)
	}
	return p
}
