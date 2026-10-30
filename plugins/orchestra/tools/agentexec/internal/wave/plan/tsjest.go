package plan

import (
	"encoding/json"
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
	jestWord   = regexp.MustCompile(`\bjest\b`)
	jestConfig = regexp.MustCompile(`^jest\.config\.(js|cjs|mjs|ts|json)$`)
	// scriptSep splits a package.json script into its shell segments.
	scriptSep = regexp.MustCompile(`[;&|]+`)
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
// jest configs under root that type-check tests. Those are the
// jest.config.{js,cjs,mjs,ts,json} files, the top-level "jest" object of each
// package.json, and each JS/JSON config that a package.json script passes to a
// jest command with --config or -c (see typeChecksText). node_modules, .git and
// dot directories are skipped, and the walk stops descending past
// maxJestConfigDepth. A referenced config that does not exist is ignored.
func typeCheckingJestConfigs(root string) []string {
	found := map[string]bool{}
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
		switch {
		case jestConfig.MatchString(d.Name()):
			if text, ok := readText(path); ok && typeChecksText(filepath.Dir(path), text) {
				found[filepath.ToSlash(rel)] = true
			}
		case d.Name() == "package.json":
			addPackageJSON(root, path, rel, found)
		}
		return nil
	})
	out := make([]string, 0, len(found))
	for rel := range found {
		out = append(out, rel)
	}
	sort.Strings(out)
	return out
}

// addPackageJSON records into found the jest configs that the package.json at
// path makes: its own top-level "jest" object when that type-checks, and each
// JS/JSON config a script passes to a jest command.
func addPackageJSON(root, path, rel string, found map[string]bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var top map[string]json.RawMessage
	if json.Unmarshal(data, &top) != nil {
		return
	}
	dir := filepath.Dir(path)
	if cfg := top["jest"]; len(cfg) > 0 && cfg[0] == '{' && typeChecksText(dir, string(cfg)) {
		found[filepath.ToSlash(rel)] = true
	}
	var scripts map[string]string
	if json.Unmarshal(top["scripts"], &scripts) != nil {
		return
	}
	for _, script := range scripts {
		for _, seg := range scriptSep.Split(script, -1) {
			if !jestWord.MatchString(seg) {
				continue
			}
			for _, ref := range configArgs(strings.Fields(seg)) {
				addReferencedConfig(root, dir, ref, found)
			}
		}
	}
}

// configArgs returns the path operand of each "--config <p>", "--config=<p>"
// and "-c <p>" in a command's fields.
func configArgs(fields []string) []string {
	var out []string
	for i, f := range fields {
		switch {
		case (f == "--config" || f == "-c") && i+1 < len(fields):
			out = append(out, fields[i+1])
		case strings.HasPrefix(f, "--config="):
			out = append(out, strings.TrimPrefix(f, "--config="))
		}
	}
	return out
}

// addReferencedConfig records the config ref, resolved against dir (the dir of
// its package.json), when it is an existing JS/JSON file under root that
// type-checks by typeChecksText.
func addReferencedConfig(root, dir, ref string, found map[string]bool) {
	ref = strings.Trim(ref, `'"`)
	if ref == "" {
		return
	}
	if !filepath.IsAbs(ref) {
		ref = filepath.Join(dir, ref)
	}
	rel, err := filepath.Rel(root, ref)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return
	}
	switch filepath.Ext(ref) {
	case ".js", ".cjs", ".mjs", ".ts", ".json":
	default:
		return
	}
	if text, ok := readText(ref); ok && typeChecksText(filepath.Dir(ref), text) {
		found[filepath.ToSlash(rel)] = true
	}
}

// typeChecksText reports whether jest config text, with <rootDir> bound to dir,
// names ts-jest, never mentions isolatedModules, and names no tsconfig that
// sets "isolatedModules": true.
func typeChecksText(dir, text string) bool {
	return strings.Contains(text, "ts-jest") && !strings.Contains(text, "isolatedModules") && !tsconfigSetsIsolated(dir, text)
}

func readText(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return string(data), true
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
