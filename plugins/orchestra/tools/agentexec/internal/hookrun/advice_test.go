package hookrun

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSessionStartAdviceForInstructorInRepo injects the advice block after the
// router for an instructor model in a repo with a type-checking jest config,
// and nothing for a non-instructor model in the same repo.
func TestSessionStartAdviceForInstructorInRepo(t *testing.T) {
	repo := sessionStartRepo(t)
	if err := os.WriteFile(filepath.Join(repo, "jest.config.js"), []byte("module.exports = { preset: 'ts-jest' };\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := sessionStartContextOf(t, sessionStartInput(t, "advice-opus", "startup", "claude-opus-5", repo))
	if !strings.HasPrefix(got, sessionStartRouterOpen) {
		t.Errorf("context does not start with the router: %q", got)
	}
	for _, want := range []string{"<orchestra-advice>", "- jest.config.js: ts-jest type-checks every test file", "</orchestra-advice>"} {
		if !strings.Contains(got, want) {
			t.Errorf("context lacks %q: %q", want, got)
		}
	}
	if strings.Contains(got, "{{ADVICE}}") {
		t.Errorf("placeholder left in context: %q", got)
	}
	if other := sessionStartContextOf(t, sessionStartInput(t, "advice-sonnet", "startup", "claude-sonnet-5", repo)); other != "" {
		t.Errorf("non-instructor model got context %q", other)
	}
}

// TestSessionStartNoAdviceOutsideRepo keeps the router alone when the cwd is
// not in a repository.
func TestSessionStartNoAdviceOutsideRepo(t *testing.T) {
	stampsTempState(t)
	got := sessionStartContextOf(t, sessionStartInput(t, "advice-none", "startup", "claude-opus-5", t.TempDir()))
	if strings.Contains(got, "<orchestra-advice>") {
		t.Errorf("advice injected outside a repository: %q", got)
	}
}
