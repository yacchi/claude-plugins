package isolate

import (
	"path"
	"strings"
	"sync"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/config"
)

// ChangeFilter keeps dependency and cache directories out of a task's change
// set (isolation.exclude_paths) and bounds how many new files one task may
// add (isolation.max_added_files; 0 = no limit). An Exclude entry names a
// directory and also a file or symlink of that name: one without an inner "/"
// ("node_modules/") matches that name at any depth; one with an inner "/"
// ("packages/a/.cache/") is relative to the repo root.
type ChangeFilter struct {
	Exclude  []string
	MaxAdded int
}

var (
	currentFilterOnce sync.Once
	currentFilter     ChangeFilter
)

// CurrentFilter is the configured filter, resolved once per process (config
// resolution stays off the hot paths); the defaults when the config cannot
// be resolved.
func CurrentFilter() ChangeFilter {
	currentFilterOnce.Do(func() {
		currentFilter = ChangeFilter{Exclude: config.DefaultExcludePaths, MaxAdded: config.DefaultMaxAddedFiles}
		cfg, err := config.Resolve()
		if err != nil {
			return
		}
		if list, ok := lookupList(cfg, "isolation", "exclude_paths"); ok {
			currentFilter.Exclude = list
		}
		v, _ := cfg.Lookup("isolation", "max_added_files")
		switch x := v.(type) {
		case int64:
			currentFilter.MaxAdded = int(max(x, 0))
		case float64:
			currentFilter.MaxAdded = int(max(x, 0))
		}
	})
	return currentFilter
}

func lookupList(cfg *config.Resolved, keys ...string) ([]string, bool) {
	v, _ := cfg.Lookup(keys...)
	list, ok := v.([]any)
	if !ok {
		return nil, false
	}
	out := []string{}
	for _, e := range list {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out, true
}

// entries is Exclude without trailing slashes, empty entries dropped.
func (f ChangeFilter) entries() []string {
	out := make([]string, 0, len(f.Exclude))
	for _, e := range f.Exclude {
		if e = strings.Trim(strings.TrimSpace(e), "/"); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// globs are the git glob patterns for each entry: the entry itself (a file or
// symlink of that name) and everything under it.
func (f ChangeFilter) globs() []string {
	var out []string
	for _, e := range f.entries() {
		if strings.Contains(e, "/") {
			out = append(out, e, e+"/**")
		} else {
			out = append(out, "**/"+e, "**/"+e+"/**")
		}
	}
	return out
}

// Pathspec is the git pathspec of the whole tree minus the excluded
// directories, "--" first.
func (f ChangeFilter) Pathspec() []string {
	out := []string{"--", "."}
	for _, g := range f.globs() {
		out = append(out, ":(exclude,glob)"+g)
	}
	return out
}

// ExcludedPathspec is the git pathspec of the excluded paths only ("--" first,
// ":(glob)" entries, both forms per entry); nil when nothing is excluded.
// Callers unstage or list exactly what the filter keeps out.
func (f ChangeFilter) ExcludedPathspec() []string {
	return f.includeSpec()
}

// includeSpec is the pathspec of the excluded directories only; nil when
// nothing is excluded.
func (f ChangeFilter) includeSpec() []string {
	globs := f.globs()
	if len(globs) == 0 {
		return nil
	}
	out := []string{"--"}
	for _, g := range globs {
		out = append(out, ":(glob)"+g)
	}
	return out
}

// Excluded reports whether the repo-relative path lies under an excluded
// directory, or is one.
func (f ChangeFilter) Excluded(p string) bool {
	_, ok := f.match(p)
	return ok
}

// match returns the entry (with its trailing "/") that excludes p. The last
// segment counts too: it is the file itself, or a symlink or file named like
// an entry.
func (f ChangeFilter) match(p string) (string, bool) {
	parts := strings.Split(p, "/")
	for _, e := range f.entries() {
		pat := strings.Split(e, "/")
		if len(pat) == 1 {
			for _, seg := range parts {
				if ok, _ := path.Match(e, seg); ok {
					return e + "/", true
				}
			}
			continue
		}
		if len(parts) < len(pat) {
			continue
		}
		matched := true
		for i, seg := range pat {
			if ok, _ := path.Match(seg, parts[i]); !ok {
				matched = false
				break
			}
		}
		if matched {
			return e + "/", true
		}
	}
	return "", false
}
