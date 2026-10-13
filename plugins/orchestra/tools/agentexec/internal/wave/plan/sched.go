package plan

import (
	"sort"
	"strings"
)

// Status maps a package id to its status. A package that is absent counts as
// "pending".
type Status map[string]string

func (s Status) of(id string) string {
	if v, ok := s[id]; ok {
		return v
	}
	return "pending"
}

// Stem is the literal path prefix before the first glob wildcard segment.
func Stem(pattern string) string {
	pattern = strings.TrimPrefix(pattern, "./")
	pattern = strings.TrimSuffix(pattern, "/")
	var kept []string
	for _, part := range strings.Split(pattern, "/") {
		if strings.ContainsAny(part, "*?[") {
			break
		}
		kept = append(kept, part)
	}
	return strings.Join(kept, "/")
}

func stemOverlap(a, b string) bool {
	if a == "" || b == "" || a == b {
		return true
	}
	shorter, longer := a, b
	if len(a) >= len(b) {
		shorter, longer = b, a
	}
	return longer == shorter || strings.HasPrefix(longer, shorter+"/")
}

// Overlaps reports whether any pattern in a overlaps any pattern in b.
func Overlaps(a, b []string) bool {
	for _, x := range a {
		sx := Stem(x)
		for _, y := range b {
			if stemOverlap(sx, Stem(y)) {
				return true
			}
		}
	}
	return false
}

// Blocked maps every package that depends (transitively) on a failed, needs
// or blocked package to the dependency that blocks it. A dependency listed
// in external_done never blocks.
func Blocked(p *Plan, st Status) map[string]string {
	deps := make(map[string][]string, len(p.Packages))
	for _, pkg := range p.Packages {
		deps[pkg.ID] = pkg.DependsOn
	}
	var blocker func(id string, seen map[string]bool) (string, bool)
	blocker = func(id string, seen map[string]bool) (string, bool) {
		if seen[id] {
			return "", false // cycle guard; Load already rejects real cycles
		}
		next := make(map[string]bool, len(seen)+1)
		for k := range seen {
			next[k] = true
		}
		next[id] = true
		for _, dep := range deps[id] {
			if _, isPkg := deps[dep]; !isPkg {
				continue
			}
			if blockingStat[st.of(dep)] {
				return dep, true
			}
			if _, ok := blocker(dep, next); ok {
				return dep, true
			}
		}
		return "", false
	}
	result := map[string]string{}
	for _, pkg := range p.Packages {
		if dep, ok := blocker(pkg.ID, map[string]bool{}); ok {
			result[pkg.ID] = dep
		}
	}
	return result
}

// Select returns the ids ready to start now, most eligible first (highest
// prio, then plan order), capped by limit minus the packages in flight. A
// candidate whose files_owned overlap an in-flight or already selected
// package is held back.
func Select(p *Plan, st Status, limit int) []string {
	blocked := Blocked(p, st)
	inFlightCount := 0
	var inFlightFiles [][]string
	ids := make(map[string]bool, len(p.Packages))
	for _, pkg := range p.Packages {
		ids[pkg.ID] = true
		if inFlight[st.of(pkg.ID)] {
			inFlightCount++
			inFlightFiles = append(inFlightFiles, pkg.FilesOwned)
		}
	}
	slots := max(limit-inFlightCount, 0)
	if slots == 0 {
		return []string{}
	}

	var candidates []Package
	for _, pkg := range p.Packages {
		if _, isBlocked := blocked[pkg.ID]; isBlocked || st.of(pkg.ID) != "pending" {
			continue
		}
		ok := true
		for _, dep := range pkg.DependsOn {
			if ids[dep] && st.of(dep) != "integrated" {
				ok = false
				break
			}
		}
		if ok {
			candidates = append(candidates, pkg)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Prio > candidates[j].Prio })

	selected := []string{}
	var selectedFiles [][]string
	for _, pkg := range candidates {
		if len(selected) >= slots {
			break
		}
		if overlapsAny(pkg.FilesOwned, inFlightFiles) || overlapsAny(pkg.FilesOwned, selectedFiles) {
			continue
		}
		selected = append(selected, pkg.ID)
		selectedFiles = append(selectedFiles, pkg.FilesOwned)
	}
	return selected
}

func overlapsAny(files []string, others [][]string) bool {
	for _, o := range others {
		if Overlaps(files, o) {
			return true
		}
	}
	return false
}

// Summary counts packages per status (every enum value present, a blocked
// dependent counted as "blocked"), plus "total" and "runnable".
func Summary(p *Plan, st Status) map[string]int {
	counts := map[string]int{}
	for _, s := range Statuses {
		counts[s] = 0
	}
	blocked := Blocked(p, st)
	for _, pkg := range p.Packages {
		s := st.of(pkg.ID)
		if _, isBlocked := blocked[pkg.ID]; isBlocked && s != "failed" && s != "needs" {
			s = "blocked"
		}
		counts[s]++
	}
	counts["total"] = len(p.Packages)
	counts["runnable"] = len(Select(p, st, 1<<30))
	return counts
}

// TopoOrder is a deterministic topological order; ties break by plan order.
func TopoOrder(p *Plan) []string {
	index := make(map[string]int, len(p.Packages))
	deps := make(map[string][]string, len(p.Packages))
	for i, pkg := range p.Packages {
		index[pkg.ID] = i
		deps[pkg.ID] = pkg.DependsOn
	}
	var result []string
	visited := map[string]bool{}
	var visit func(id string)
	visit = func(id string) {
		if visited[id] {
			return
		}
		visited[id] = true
		ordered := append([]string(nil), deps[id]...)
		sort.SliceStable(ordered, func(i, j int) bool { return depIndex(index, ordered[i]) < depIndex(index, ordered[j]) })
		for _, dep := range ordered {
			if _, isPkg := index[dep]; isPkg {
				visit(dep)
			}
		}
		result = append(result, id)
	}
	for _, pkg := range p.Packages {
		visit(pkg.ID)
	}
	return result
}

func depIndex(index map[string]int, id string) int {
	if i, ok := index[id]; ok {
		return i
	}
	return -1
}
