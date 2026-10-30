package plan

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// maxJestConfigDepth caps how deep the repo walk for jest configs descends.
const maxJestConfigDepth = 6

const (
	tsJestHeader  = "ts-jest type-checks every test in every jest worker; tests must only transform and types are checked once by tsc --noEmit after integration:"
	tsJestUnowned = "no package owns it; add a package that switches it to transform-only (isolatedModules: true, or @swc/jest) and moves type checking to tsc --noEmit after integration"
)

var (
	tsJestWord = regexp.MustCompile(`\bts-jest\b`)
	jestConfig = regexp.MustCompile(`^jest\.config\.(js|cjs|mjs|ts)$`)
	// tsconfigRef matches a string literal ending in .json; exactly one group is set.
	tsconfigRef    = regexp.MustCompile(`"([^"\n]*\.json)"|'([^'\n]*\.json)'|` + "`([^`\\n]*\\.json)`")
	isolatedTrueJS = regexp.MustCompile(`"isolatedModules"\s*:\s*true`)
)

// CheckTsJest enforces that tests only transform under ts-jest: jest type-checks
// every test in every worker, which exhausted host memory with a large
// dependency. skeleton=true reads each package's brief; skeleton=false reads
// its spec when readable, else its brief. Both read each distinct preamble.
// With a non-empty root, each jest config under it that type-checks must be
// owned by some package. It returns nil or an *Error listing every finding.
func CheckTsJest(p *Plan, root string, skeleton bool) error {
	var findings []string
	for _, pkg := range p.Packages {
		text, kind := pkg.Brief, "brief"
		if !skeleton {
			if data, err := os.ReadFile(pkg.Spec); err == nil {
				text, kind = string(data), "spec"
			}
		}
		if tsJestUnisolated(text) {
			findings = append(findings, pkg.ID+": "+kind+" uses ts-jest without isolatedModules")
		}
	}
	seen := map[string]bool{}
	for _, pre := range p.Preamble {
		if seen[pre] {
			continue
		}
		seen[pre] = true
		data, err := os.ReadFile(pre)
		if err != nil {
			continue
		}
		if tsJestUnisolated(string(data)) {
			findings = append(findings, "preamble "+pre+": uses ts-jest without isolatedModules")
		}
	}
	if root != "" {
		for _, rel := range typeCheckingJestConfigs(root) {
			if !ownedByPackage(p, rel) {
				findings = append(findings, rel+": "+tsJestUnowned)
			}
		}
	}
	if len(findings) == 0 {
		return nil
	}
	var b strings.Builder
	b.WriteString(tsJestHeader)
	for _, f := range findings {
		b.WriteString("\n- " + f)
	}
	return errf("%s", b.String())
}

// tsJestUnisolated reports text that names ts-jest without ever mentioning
// isolatedModules.
func tsJestUnisolated(text string) bool {
	return tsJestWord.MatchString(text) && !strings.Contains(text, "isolatedModules")
}

// ownedByPackage reports whether some package's files_owned covers the
// repo-relative path: an exact match, or an entry ending in "/" that prefixes it.
func ownedByPackage(p *Plan, rel string) bool {
	for _, pkg := range p.Packages {
		for _, f := range pkg.FilesOwned {
			if f == rel || (strings.HasSuffix(f, "/") && strings.HasPrefix(rel, f)) {
				return true
			}
		}
	}
	return false
}

// typeCheckingJestConfigs returns, sorted, the repo-relative slash paths of the
// jest.config.{js,cjs,mjs,ts} files under root that mention ts-jest without
// isolatedModules, unless a tsconfig they name sets "isolatedModules": true.
// node_modules, .git and dot directories are skipped, and the walk stops
// descending past maxJestConfigDepth.
func typeCheckingJestConfigs(root string) []string {
	var found []string
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == root {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		depth := strings.Count(rel, string(filepath.Separator)) + 1
		if d.IsDir() {
			name := d.Name()
			if name == "node_modules" || name == ".git" || strings.HasPrefix(name, ".") || depth > maxJestConfigDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if !jestConfig.MatchString(d.Name()) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		text := string(data)
		if !strings.Contains(text, "ts-jest") || strings.Contains(text, "isolatedModules") {
			return nil
		}
		if tsconfigSetsIsolated(filepath.Dir(path), text) {
			return nil
		}
		found = append(found, filepath.ToSlash(rel))
		return nil
	})
	sort.Strings(found)
	return found
}

// tsconfigSetsIsolated reports whether a tsconfig named by a .json string
// literal in a jest config (with <rootDir> bound to the config's dir) sets
// "isolatedModules": true. A missing tsconfig counts as not set.
func tsconfigSetsIsolated(dir, text string) bool {
	for _, m := range tsconfigRef.FindAllStringSubmatch(text, -1) {
		ref := m[1] + m[2] + m[3] // exactly one group is non-empty
		ref = strings.ReplaceAll(ref, "<rootDir>", dir)
		if !filepath.IsAbs(ref) {
			ref = filepath.Join(dir, ref)
		}
		data, err := os.ReadFile(ref)
		if err == nil && isolatedTrueJS.Match(data) {
			return true
		}
	}
	return false
}
