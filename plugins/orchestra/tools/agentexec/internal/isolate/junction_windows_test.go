package isolate

import (
	"os"
	"path/filepath"
	"testing"
)

// pnpm on Windows links node_modules/<pkg> into .pnpm with junctions: the
// copy keeps them as junctions, pointed inside the copy, not the source.
func TestCarryDirKeepsJunctionsInsideTheCopy(t *testing.T) {
	srcRoot := t.TempDir()
	nm := filepath.Join(srcRoot, "node_modules")
	real := filepath.Join(nm, ".pnpm", "dep@1.0.0", "node_modules", "dep")
	writeFile(t, filepath.Join(real, "index.js"), "module.exports = 1\n")
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "o.js"), "o\n")
	if err := makeJunction(filepath.Join(nm, "dep"), real); err != nil {
		t.Fatal(err)
	}
	if err := makeJunction(filepath.Join(nm, "ext"), outside); err != nil {
		t.Fatal(err)
	}
	if err := makeJunction(filepath.Join(nm, "gone"), filepath.Join(srcRoot, "missing")); err != nil {
		t.Fatal(err)
	}

	dstRoot := filepath.Join(t.TempDir(), "wt")
	if err := CarryDir(srcRoot, dstRoot, "node_modules"); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(dstRoot, "node_modules")
	link, err := os.Readlink(filepath.Join(dst, "dep"))
	want := filepath.Join(dst, ".pnpm", "dep@1.0.0", "node_modules", "dep")
	if err != nil || link != want {
		t.Fatalf("dep junction -> %q (%v), want %q", link, err, want)
	}
	if readFile(t, filepath.Join(dst, "dep", "index.js")) != "module.exports = 1\n" {
		t.Fatal("content not reachable through the copied junction")
	}
	if link, err := os.Readlink(filepath.Join(dst, "ext")); err != nil || link != outside {
		t.Fatalf("outside junction -> %q (%v), want %q", link, err, outside)
	}
	if _, err := os.Lstat(filepath.Join(dst, "gone")); !os.IsNotExist(err) {
		t.Fatalf("a dangling junction was carried: %v", err)
	}
	// Removing the copy never reaches through a junction.
	if err := os.RemoveAll(dstRoot); err != nil {
		t.Fatal(err)
	}
	if readFile(t, filepath.Join(real, "index.js")) == "" || readFile(t, filepath.Join(outside, "o.js")) == "" {
		t.Fatal("removing the copy deleted a junction's target")
	}
}
