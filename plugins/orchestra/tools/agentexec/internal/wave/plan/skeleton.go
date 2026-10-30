package plan

import (
	"math"
	"os"
	"strconv"
	"strings"
	"unicode/utf8"
)

// SpecWarnings returns the warnings a spec writer can fix in pkg's spec: its
// length, and each interface the package names that the spec does not paste
// verbatim. An unreadable or non-UTF-8 spec yields none.
func SpecWarnings(p *Plan, pkg Package) []Warning {
	data, err := os.ReadFile(pkg.Spec)
	if err != nil || !utf8.Valid(data) {
		return nil
	}
	var warnings []Warning
	if lines := countLines(string(data)); lines > maxSpecLines {
		warnings = append(warnings, Warning{pkgPtr(pkg.ID),
			"package " + pkg.ID + " spec is " + strconv.Itoa(lines) + " lines; split it into packages with specs of at most " +
				strconv.Itoa(maxSpecLines) + " lines so no single package runs for tens of minutes"})
	}
	spec := normalizeSpace(string(data))
	for _, id := range pkg.Interfaces {
		iface, ok := p.Interface(id)
		if !ok {
			continue
		}
		raw, err := os.ReadFile(iface.Path)
		if err != nil {
			continue
		}
		text := normalizeSpace(string(raw))
		if text == "" || strings.Contains(spec, text) {
			continue
		}
		warnings = append(warnings, Warning{pkgPtr(pkg.ID),
			"package " + pkg.ID + " spec does not contain interface " + id + " verbatim; paste " + iface.Path + " into it unchanged"})
	}
	return warnings
}

// normalizeSpace makes the verbatim check indifferent to indentation and line
// breaks, which a spec writer re-flows when pasting a contract into a fence.
func normalizeSpace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// Estimate returns the plan's total estimated seconds and the weight of its
// heaviest dependency chain, each rounded to whole seconds. Dependencies on
// packages outside the plan (external_done) weigh nothing.
func Estimate(p *Plan) (total, critical int) {
	byID := make(map[string]Package, len(p.Packages))
	for _, pkg := range p.Packages {
		byID[pkg.ID] = pkg
	}
	heaviest := map[string]float64{}
	var chain func(id string) float64
	chain = func(id string) float64 {
		if w, done := heaviest[id]; done {
			return w
		}
		pkg := byID[id]
		var deepest float64
		for _, dep := range pkg.DependsOn {
			if _, inPlan := byID[dep]; inPlan {
				deepest = math.Max(deepest, chain(dep))
			}
		}
		heaviest[id] = pkg.EstSeconds + deepest
		return heaviest[id]
	}
	var sum, longest float64
	for _, pkg := range p.Packages {
		sum += pkg.EstSeconds
		longest = math.Max(longest, chain(pkg.ID))
	}
	return int(math.Round(sum)), int(math.Round(longest))
}
