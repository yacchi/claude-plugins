package plan

import (
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
)

var deletionWords = regexp.MustCompile(`(?i)削除|撤去|取り除|外す|remove|delete|drop|deprecat`)

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

// Lint returns the non-blocking ownership and sizing warnings. With a
// non-empty root it also warns about warm_read files missing from it (they
// may appear after integration, so this is never an error).
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
		}
		warnings = append(warnings, ruleWarnings(p, pkg)...)
	}
	return warnings
}

// LintSkeleton is Lint for a planning skeleton, which has no spec files yet:
// the deletion check reads the package's brief, and the spec checks are left
// to SpecWarnings once the specs exist. It also flags an interface no package
// names, which would otherwise never be pasted into any spec.
func LintSkeleton(p *Plan) []Warning {
	warnings := []Warning{}
	listed := map[string]bool{}
	for _, pkg := range p.Packages {
		warnings = append(warnings, ownedWarnings(pkg)...)
		warnings = append(warnings, deletionWarnings(pkg, pkg.Brief)...)
		warnings = append(warnings, ruleWarnings(p, pkg)...)
		for _, id := range pkg.Interfaces {
			listed[id] = true
		}
	}
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
