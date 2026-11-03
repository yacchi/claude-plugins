package plan

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ifaceLoadFile writes an interface file of the given line count and returns its path.
func ifaceLoadFile(t *testing.T, dir, name string, lines int) string {
	t.Helper()
	var b strings.Builder
	for i := 0; i < lines; i++ {
		b.WriteString("line\n")
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// ifaceLoadMsg returns the *Error message, or "" for nil; it fails on any other error type.
func ifaceLoadMsg(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("want *Error, got %T: %v", err, err)
	}
	return e.Msg
}

func TestIfaceLoadBoundary(t *testing.T) {
	dir := t.TempDir()
	p := &Plan{
		Interfaces: []Interface{
			{ID: "i50", Path: ifaceLoadFile(t, dir, "i50", 50)},
			{ID: "i40", Path: ifaceLoadFile(t, dir, "i40", 40)},
			{ID: "i41", Path: ifaceLoadFile(t, dir, "i41", 41)},
		},
		Packages: []Package{{ID: "a", Interfaces: []string{"i50", "i40"}}},
	}
	if err := CheckInterfaceLoad(p); err != nil {
		t.Fatalf("total 90 must pass, got %v", err)
	}
	p.Packages[0].Interfaces = []string{"i50", "i41"}
	want := "package a lists 91 interface lines (i50 50, i41 41); with a 60-line body its spec passes the 150-line limit: list only the interfaces the package produces, and quote in its brief the lines it consumes"
	if got := ifaceLoadMsg(t, CheckInterfaceLoad(p)); got != want {
		t.Fatalf("message\n got: %q\nwant: %q", got, want)
	}
}

func TestIfaceLoadUnterminatedLine(t *testing.T) {
	dir := t.TempDir()
	nonl := filepath.Join(dir, "nonl")
	if err := os.WriteFile(nonl, []byte(strings.Repeat("line\n", 89)+"last"), 0o644); err != nil {
		t.Fatal(err)
	}
	p := &Plan{
		Interfaces: []Interface{{ID: "nonl", Path: nonl}},
		Packages:   []Package{{ID: "a", Interfaces: []string{"nonl"}}},
	}
	if err := CheckInterfaceLoad(p); err != nil {
		t.Fatalf("90 counted lines must pass, got %v", err)
	}
	p.Interfaces[0].Path = ifaceLoadFile(t, dir, "big", 91)
	if got := ifaceLoadMsg(t, CheckInterfaceLoad(p)); !strings.HasPrefix(got, "package a lists 91 interface lines (nonl 91);") {
		t.Fatalf("unexpected message: %q", got)
	}
}

func TestIfaceLoadOrderAndJoin(t *testing.T) {
	dir := t.TempDir()
	p := &Plan{
		Interfaces: []Interface{
			{ID: "big", Path: ifaceLoadFile(t, dir, "big", 91)},
			{ID: "i40", Path: ifaceLoadFile(t, dir, "i40", 40)},
			{ID: "i50", Path: ifaceLoadFile(t, dir, "i50", 50)},
			{ID: "i41", Path: ifaceLoadFile(t, dir, "i41", 41)},
		},
		Packages: []Package{
			{ID: "b", Interfaces: []string{"big"}},
			{ID: "ok", Interfaces: []string{"i40"}},
			{ID: "a", Interfaces: []string{"i50", "i41"}},
		},
	}
	got := ifaceLoadMsg(t, CheckInterfaceLoad(p))
	bFinding := "package b lists 91 interface lines (big 91); with a 60-line body"
	aFinding := "package a lists 91 interface lines (i50 50, i41 41); with a 60-line body"
	if !strings.HasPrefix(got, bFinding) || !strings.Contains(got, "; "+aFinding) {
		t.Fatalf("want b then a joined by \"; \", got %q", got)
	}
	if strings.Contains(got, "package ok") {
		t.Fatalf("passing package reported: %q", got)
	}
	if strings.Count(got, " interface lines (") != 2 {
		t.Fatalf("want exactly two findings, got %q", got)
	}
}

func TestIfaceLoadMissingInterfaceCountsZero(t *testing.T) {
	dir := t.TempDir()
	p := &Plan{
		Interfaces: []Interface{
			{ID: "i50", Path: ifaceLoadFile(t, dir, "i50", 50)},
			{ID: "i41", Path: ifaceLoadFile(t, dir, "i41", 41)},
			{ID: "gone", Path: filepath.Join(dir, "missing")},
		},
		Packages: []Package{
			{ID: "a", Interfaces: []string{"i50", "i41", "ghost"}},
			{ID: "c", Interfaces: []string{"ghost", "gone"}},
		},
	}
	got := ifaceLoadMsg(t, CheckInterfaceLoad(p))
	want := "package a lists 91 interface lines (i50 50, i41 41, ghost 0); with a 60-line body its spec passes the 150-line limit: list only the interfaces the package produces, and quote in its brief the lines it consumes"
	if got != want {
		t.Fatalf("message\n got: %q\nwant: %q", got, want)
	}
}

func TestIfaceLoadPerPackageAllowance(t *testing.T) {
	dir := t.TempDir()
	p := &Plan{
		Interfaces: []Interface{
			{ID: "i50", Path: ifaceLoadFile(t, dir, "i50", 50)},
			{ID: "i40", Path: ifaceLoadFile(t, dir, "i40", 40)},
		},
		Packages: []Package{
			{ID: "a", Interfaces: []string{"i50", "i40"}},
			{ID: "b", Interfaces: []string{"i50", "i40"}},
		},
	}
	if err := CheckInterfaceLoad(p); err != nil {
		t.Fatalf("allowance is per package, got %v", err)
	}
}

func TestIfaceLoadEmpty(t *testing.T) {
	if err := CheckInterfaceLoad(&Plan{}); err != nil {
		t.Fatalf("empty plan: got %v", err)
	}
	p := &Plan{Packages: []Package{{ID: "a", Interfaces: nil}}}
	if err := CheckInterfaceLoad(p); err != nil {
		t.Fatalf("no interfaces: got %v", err)
	}
}
