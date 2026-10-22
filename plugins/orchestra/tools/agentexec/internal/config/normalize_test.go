package config

import (
	"slices"
	"testing"
)

// resolveLayers merges layers over the defaults the way Resolve does,
// without touching any config file.
func resolveLayers(layers ...map[string]any) map[string]any {
	values := Defaults()
	u := newUnion()
	for _, l := range layers {
		u.add(l)
		values = DeepMerge(values, l)
	}
	normalize(values, u)
	return values
}

func excludePaths(t *testing.T, values map[string]any) []string {
	t.Helper()
	var out []string
	for _, v := range ObjectAt(values, "isolation")["exclude_paths"].([]any) {
		out = append(out, v.(string))
	}
	return out
}

// A layer's exclude_paths adds to the defaults; it never replaces them.
func TestExcludePathsAreUnionedWithTheDefaults(t *testing.T) {
	got := excludePaths(t, resolveLayers(obj("isolation", obj("exclude_paths", strs("vendor/cache/")))))
	want := append(append([]string(nil), DefaultExcludePaths...), "vendor/cache/")
	if !slices.Equal(got, want) {
		t.Errorf("exclude_paths = %v, want %v", got, want)
	}
}

func TestKeepPathsTakeAnEntryBackOut(t *testing.T) {
	got := excludePaths(t, resolveLayers(obj("isolation", obj("keep_paths", strs("node_modules")))))
	if slices.Contains(got, "node_modules/") || !slices.Contains(got, ".pnpm-store/") {
		t.Errorf("exclude_paths = %v", got)
	}
}

func TestExcludePathEntriesAreNormalized(t *testing.T) {
	got := excludePaths(t, resolveLayers(obj("isolation", obj("exclude_paths", strs("./x", " /y/ ", "../z", "a/../b", "", "x/")))))
	tail := got[len(DefaultExcludePaths):]
	if !slices.Equal(tail, []string{"x/", "y/"}) {
		t.Errorf("added entries = %v, want [x/ y/]", tail)
	}
}
