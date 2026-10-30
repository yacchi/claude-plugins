package plan

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxOwnedFiles = 3
	maxSpecLines  = 150
	// maxJestConfigDepth caps how deep the repo walk for jest configs descends.
	maxJestConfigDepth = 6
	tsJestMessage      = "ts-jest without isolatedModules type-checks every test in every jest worker: specify isolatedModules: true (or @swc/jest) and type-check once with tsc --noEmit after integration"
)

var (
	deletionWords = regexp.MustCompile(`(?i)削除|撤去|取り除|外す|remove|delete|drop|deprecat`)
	tsJestWord    = regexp.MustCompile(`\bts-jest\b`)
	jestConfig    = regexp.MustCompile(`^jest\.config\.(js|cjs|mjs|ts)$`)
	// tsconfigRef matches a string literal ending in .json; exactly one group is set.
	tsconfigRef    = regexp.MustCompile(`"([^"\n]*\.json)"|'([^'\n]*\.json)'|` + "`([^`\\n]*\\.json)`")
	isolatedTrueJS = regexp.MustCompile(`"isolatedModules"\s*:\s*true`)
)

// Warning is one non-blocking lint finding; Pkg is nil for plan-level ones.
type Warning struct {
	Pkg     *string `json:"pkg"`
	Message string  `json:"message"`
}

func pkgPtr(id string) *string { return &id }

// countLines is the number of lines in text; a final line without a
// trailing newline counts.
func countLines(text string) int {
	n := strings.Count(text, "\n")
	if text != "" && !strings.HasSuffix(text, "\n") {
		n++
	}
	return n
}

// Lint returns the non-blocking ownership, sizing and ts-jest warnings. With a
// non-empty root it also warns about warm_read files missing from it (they
// may appear after integration, so this is never an error) and about jest
// configs under it that use ts-jest without isolatedModules.
func Lint(p *Plan, root string) []Warning {
	warnings := []Warning{}
	if root != "" {
		for _, rel := range p.WarmRead {
			if _, err := os.Stat(filepath.Join(root, rel)); err != nil {
				warnings = append(warnings, Warning{Message: "warm_read file does not exist in the repo: " + rel})
			}
		}
	}
	for _, pkg := range p.Packages {
		data, err := os.ReadFile(pkg.Spec)
		specText, haveSpec := string(data), err == nil && utf8.Valid(data)
		warnings = append(warnings, ownedWarnings(pkg)...)
		warnings = append(warnings, SpecWarnings(p, pkg)...)
		if haveSpec {
			warnings = append(warnings, deletionWarnings(pkg, specText)...)
			warnings = append(warnings, tsJestWarnings(pkgPtr(pkg.ID), specText)...)
		}
		warnings = append(warnings, ruleWarnings(p, pkg)...)
	}
	warnings = append(warnings, preambleWarnings(p)...)
	if root != "" {
		warnings = append(warnings, jestConfigWarnings(root)...)
	}
	return warnings
}

// LintSkeleton is Lint for a planning skeleton, which has no spec files yet:
// the deletion and ts-jest checks read the package's brief, and the spec checks
// are left to SpecWarnings once the specs exist. It also flags an interface no
// package names, which would otherwise never be pasted into any spec.
func LintSkeleton(p *Plan) []Warning {
	warnings := []Warning{}
	listed := map[string]bool{}
	for _, pkg := range p.Packages {
		warnings = append(warnings, ownedWarnings(pkg)...)
		warnings = append(warnings, deletionWarnings(pkg, pkg.Brief)...)
		warnings = append(warnings, tsJestWarnings(pkgPtr(pkg.ID), pkg.Brief)...)
		warnings = append(warnings, ruleWarnings(p, pkg)...)
		for _, id := range pkg.Interfaces {
			listed[id] = true
		}
	}
	warnings = append(warnings, preambleWarnings(p)...)
	for _, iface := range p.Interfaces {
		if !listed[iface.ID] {
			warnings = append(warnings, Warning{Message: "interface " + iface.ID +
				" is used by no package; list it in the packages that produce or consume it"})
		}
	}
	// Not in Lint: its warnings are emitted as events into every wave's log,
	// which would churn every wave-run golden.
	if strings.TrimSpace(p.CommitMessage) == "" {
		warnings = append(warnings, Warning{Message: "plan has no commit_message: a done run cannot land its result by itself (isolation.land: auto)"})
	}
	return warnings
}

// tsJestWarnings flags text that names ts-jest without ever mentioning
// isolatedModules: ts-jest then type-checks every test file in every worker.
func tsJestWarnings(pkg *string, text string) []Warning {
	if !tsJestWord.MatchString(text) || strings.Contains(text, "isolatedModules") {
		return nil
	}
	return []Warning{{pkg, tsJestMessage}}
}

// preambleWarnings applies tsJestWarnings to each distinct preamble file.
func preambleWarnings(p *Plan) []Warning {
	var warnings []Warning
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
		warnings = append(warnings, tsJestWarnings(nil, string(data))...)
	}
	return warnings
}

// jestConfigWarnings is the repo rule: every jest.config.{js,cjs,mjs,ts} under
// root that mentions ts-jest must mention isolatedModules, or name a tsconfig
// that sets "isolatedModules": true. node_modules, .git and dot directories are
// skipped, and the walk stops descending past maxJestConfigDepth.
func jestConfigWarnings(root string) []Warning {
	var warnings []Warning
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
		warnings = append(warnings, Warning{nil, filepath.ToSlash(rel) + ": " + tsJestMessage})
		return nil
	})
	return warnings
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

func ownedWarnings(pkg Package) []Warning {
	if len(pkg.FilesOwned) <= maxOwnedFiles {
		return nil
	}
	return []Warning{{pkgPtr(pkg.ID),
		"package " + pkg.ID + " owns " + strconv.Itoa(len(pkg.FilesOwned)) + " files; split into packages of 1-" +
			strconv.Itoa(maxOwnedFiles) + " files so no single package runs for tens of minutes"}}
}

// deletionWarnings flags a package whose text (spec or brief) says it deletes
// code while it owns only literal paths, which cannot reach the leftovers.
func deletionWarnings(pkg Package, text string) []Warning {
	if !deletionWords.MatchString(text) {
		return nil
	}
	for _, f := range pkg.FilesOwned {
		if strings.ContainsAny(f, "*?[") {
			return nil
		}
	}
	return []Warning{{pkgPtr(pkg.ID),
		"deletes code but owns only literal paths; leftover references (tests, other features) will be out of reach — consider a glob"}}
}

func ruleWarnings(p *Plan, pkg Package) []Warning {
	var warnings []Warning
	for _, rule := range p.Rules {
		if !Overlaps(pkg.FilesOwned, []string{rule.If}) {
			continue
		}
		var missing []string
		for _, req := range rule.Require {
			if !Overlaps(pkg.FilesOwned, []string{req}) {
				missing = append(missing, req)
			}
		}
		if len(missing) > 0 {
			warnings = append(warnings, Warning{pkgPtr(pkg.ID),
				"files_owned matching " + rule.If + " are missing required ownership: " + strings.Join(missing, ", ")})
		}
	}
	return warnings
}
