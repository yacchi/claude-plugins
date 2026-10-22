package isolate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestCarryDirRelocatesVenvs(t *testing.T) {
	src := t.TempDir()
	dst := filepath.Join(t.TempDir(), "wt")
	srcRoot, dstRoot := src, dst
	venv := filepath.Join(src, ".venv")
	writeFile(t, filepath.Join(venv, "pyvenv.cfg"), "home = /usr/bin\n")
	writeFile(t, filepath.Join(venv, "bin", "pytest"),
		"#!/bin/sh\nexec '"+src+"/.venv/bin/python' \"$0\" \"$@\"\n")
	if err := os.Chmod(filepath.Join(venv, "bin", "pytest"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(venv, "bin", "activate"), "VIRTUAL_ENV='"+venv+"'\n")
	if err := os.Symlink("pytest", filepath.Join(venv, "bin", "runner")); err != nil {
		t.Fatal(err)
	}
	binary := "\x00\x01" + src + "/.venv"
	writeFile(t, filepath.Join(venv, "bin", "tool"), binary)
	sitePackages := filepath.Join(venv, "lib", "python3.12", "site-packages")
	writeFile(t, filepath.Join(sitePackages, "_demo.pth"), src+"/src\n")
	writeFile(t, filepath.Join(sitePackages, "demo-0.1.dist-info", "direct_url.json"),
		`{"url": "file://`+src+`"}`)
	writeFile(t, filepath.Join(sitePackages, "notes.txt"), src+"-other/x")
	tox := filepath.Join(src, ".tox", "py312")
	writeFile(t, filepath.Join(tox, "pyvenv.cfg"), "home = /usr/bin\n")
	writeFile(t, filepath.Join(tox, "bin", "pytest"), "exec "+tox+"/bin/python\n")

	if err := CarryDir(srcRoot, dstRoot, ".venv"); err != nil {
		t.Fatal(err)
	}
	if err := CarryDir(srcRoot, dstRoot, ".tox"); err != nil {
		t.Fatal(err)
	}

	dstVenv := filepath.Join(dst, ".venv")
	if got := readFile(t, filepath.Join(dstVenv, "bin", "pytest")); got != "#!/bin/sh\nexec '"+dst+"/.venv/bin/python' \"$0\" \"$@\"\n" {
		t.Fatalf("trampoline not relocated: %q", got)
	}
	if got := readFile(t, filepath.Join(dstVenv, "bin", "activate")); got != "VIRTUAL_ENV='"+dstVenv+"'\n" {
		t.Fatalf("activate not relocated: %q", got)
	}
	if got := readFile(t, filepath.Join(dst, ".venv", "lib", "python3.12", "site-packages", "_demo.pth")); got != dst+"/src\n" {
		t.Fatalf(".pth not relocated: %q", got)
	}
	dstSitePackages := filepath.Join(dstVenv, "lib", "python3.12", "site-packages")
	if got := readFile(t, filepath.Join(dstSitePackages, "demo-0.1.dist-info", "direct_url.json")); got != `{"url": "file://`+dst+`"}` {
		t.Fatalf("direct_url.json not relocated: %q", got)
	}
	if got := readFile(t, filepath.Join(dstSitePackages, "notes.txt")); got != src+"-other/x" {
		t.Fatalf("a sibling path was rewritten: %q", got)
	}
	if got := readFile(t, filepath.Join(dstVenv, "bin", "tool")); got != binary {
		t.Fatalf("a binary file was rewritten: %q", got)
	}
	if link, err := os.Readlink(filepath.Join(dstVenv, "bin", "runner")); err != nil || link != "pytest" {
		t.Fatalf("symlink not kept: %q %v", link, err)
	}
	if st, err := os.Stat(filepath.Join(dstVenv, "bin", "pytest")); err != nil || st.Mode().Perm() != 0o755 {
		t.Fatalf("mode not preserved: %v %v", st, err)
	}
	if got := readFile(t, filepath.Join(dst, ".tox", "py312", "bin", "pytest")); got != "exec "+filepath.Join(dst, ".tox", "py312")+"/bin/python\n" {
		t.Fatalf("depth-2 venv not relocated: %q", got)
	}
	if got := readFile(t, filepath.Join(src, ".venv", "bin", "pytest")); got != "#!/bin/sh\nexec '"+src+"/.venv/bin/python' \"$0\" \"$@\"\n" {
		t.Fatalf("source checkout was modified: %q", got)
	}
}
