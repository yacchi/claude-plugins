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

// countLines is len(text.splitlines()).
func countLines(text string) int {
	n := 0
	for i := 0; i < len(text); {
		r, w := utf8.DecodeRuneInString(text[i:])
		i += w
		switch r {
		case '\r':
			if i < len(text) && text[i] == '\n' {
				i++
			}
			n++
		case '\n', '\v', '\f', '\x1c', '\x1d', '\x1e', '\u0085', ' ', ' ':
			n++
		default:
			if i >= len(text) {
				n++
			}
		}
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
		owned := pkg.FilesOwned
		data, err := os.ReadFile(pkg.Spec)
		specText, haveSpec := string(data), err == nil && utf8.Valid(data)
		if len(owned) > maxOwnedFiles {
			warnings = append(warnings, Warning{pkgPtr(pkg.ID),
				"package " + pkg.ID + " owns " + strconv.Itoa(len(owned)) + " files; split into packages of 1-" +
					strconv.Itoa(maxOwnedFiles) + " files so no single package runs for tens of minutes"})
		}
		specLines := 0
		if haveSpec {
			specLines = countLines(specText)
		}
		if specLines > maxSpecLines {
			warnings = append(warnings, Warning{pkgPtr(pkg.ID),
				"package " + pkg.ID + " spec is " + strconv.Itoa(specLines) + " lines; split it into packages with specs of at most " +
					strconv.Itoa(maxSpecLines) + " lines so no single package runs for tens of minutes"})
		}
		if haveSpec && deletionWords.MatchString(specText) {
			literalOnly := true
			for _, f := range owned {
				if strings.ContainsAny(f, "*?[") {
					literalOnly = false
					break
				}
			}
			if literalOnly {
				warnings = append(warnings, Warning{pkgPtr(pkg.ID),
					"deletes code but owns only literal paths; leftover references (tests, other features) will be out of reach — consider a glob"})
			}
		}
		for _, rule := range p.Rules {
			if !Overlaps(owned, []string{rule.If}) {
				continue
			}
			var missing []string
			for _, req := range rule.Require {
				if !Overlaps(owned, []string{req}) {
					missing = append(missing, req)
				}
			}
			if len(missing) > 0 {
				warnings = append(warnings, Warning{pkgPtr(pkg.ID),
					"files_owned matching " + rule.If + " are missing required ownership: " + strings.Join(missing, ", ")})
			}
		}
	}
	return warnings
}
