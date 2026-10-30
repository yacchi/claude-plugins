package advise

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRepoAdvisesTypeCheckingJestConfig(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "jest.config.js"), "module.exports = { preset: 'ts-jest' };\n")
	got := Repo(root)
	if len(got) != 1 {
		t.Fatalf("got %d advice, want 1: %+v", len(got), got)
	}
	if got[0].ID != "ts-jest-typecheck" || got[0].Path != "jest.config.js" {
		t.Errorf("advice = %+v, want ts-jest-typecheck for jest.config.js", got[0])
	}
}

func TestRepoIsEmptyNotNilWhenNoAdvice(t *testing.T) {
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "jest.config.js"), "module.exports = { preset: 'ts-jest', isolatedModules: true };\n")
	if got := Repo(root); got == nil || len(got) != 0 {
		t.Errorf("Repo = %#v, want a non-nil empty list", got)
	}
	if got := Repo(""); got == nil || len(got) != 0 {
		t.Errorf("Repo(\"\") = %#v, want a non-nil empty list", got)
	}
}
