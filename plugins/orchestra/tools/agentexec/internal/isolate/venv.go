package isolate

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/trace"
)

// venvMaxDepth bounds the search for venvs in a carried directory, counted
// in path components: .venv, venv, .tox/<env> and a carry_extra venv are
// found without walking the rest of node_modules.
const venvMaxDepth = 2

// venvBinMaxSize skips large files in a venv's bin/ (compiled tools).
const venvBinMaxSize = 1 << 20

// venvBinSniff is how many leading bytes are checked for NUL to recognise
// a binary file.
const venvBinSniff = 8 << 10

// replacement is one old-to-new path pair; both are bytes so the rewrite
// works on raw file content.
type replacement struct {
	old, new []byte
}

// CarryDir copies srcRoot/rel to dstRoot/rel like the other dependency
// carries, then relocates every Python venv inside the copy to dstRoot: a
// venv embeds the absolute paths of the checkout it was built in, so its
// console scripts and editable installs would otherwise run the source
// checkout's code.
func CarryDir(srcRoot, dstRoot, rel string) error {
	if carryDir(srcRoot, dstRoot, rel) == carryFailed {
		return errors.New("could not carry " + rel)
	}
	return nil
}

// carryDir is CarryDir reporting the carry method. A venv that cannot be
// relocated fails the whole directory: left as it is, it would run the
// source checkout.
func carryDir(srcRoot, dstRoot, rel string) string {
	src, dst := filepath.Join(srcRoot, rel), filepath.Join(dstRoot, rel)
	method := copyTreeMethod(src, dst)
	if method == carryFailed {
		return carryFailed
	}
	if err := relocateVenvs(dst, venvReplacements(srcRoot, dstRoot)); err != nil {
		_ = os.RemoveAll(dst)
		return carryFailed
	}
	return method
}

// venvReplacements maps the source checkout to the destination, both as
// given and with symlinks resolved (macOS /var is /private/var), longest
// old path first.
func venvReplacements(srcRoot, dstRoot string) []replacement {
	pairs := []replacement{{[]byte(srcRoot), []byte(dstRoot)}}
	realSrc, errSrc := filepath.EvalSymlinks(srcRoot)
	realDst, errDst := filepath.EvalSymlinks(dstRoot)
	if errSrc == nil && errDst == nil && realSrc != srcRoot {
		pairs = append(pairs, replacement{[]byte(realSrc), []byte(realDst)})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return len(pairs[i].old) > len(pairs[j].old)
	})
	return pairs
}

// relocateVenvs relocates each venv (a directory holding pyvenv.cfg) found
// in dir within venvMaxDepth. Walk errors below dir are skipped.
func relocateVenvs(dir string, pairs []replacement) error {
	return filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if path == dir {
				return err
			}
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if d.IsDir() {
			if rel != "." && depthOf(rel) > venvMaxDepth {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "pyvenv.cfg" || !d.Type().IsRegular() {
			return nil
		}
		venv := filepath.Dir(path)
		end := trace.Begin(trace.KindFS, "relocate-venv", trace.Attrs{Detail: venv})
		_, err = relocateVenv(venv, pairs)
		end(exitOf(err))
		return err
	})
}

// depthOf counts the path components of a relative path.
func depthOf(rel string) int {
	n := 1
	for _, c := range rel {
		if c == filepath.Separator {
			n++
		}
	}
	return n
}

// relocateVenv rewrites the paths in one venv and returns how many files
// changed. Only the files that can embed the checkout's paths are touched:
// the regular files in bin/ and, in each site-packages, the .pth, editable
// finder, egg-link and direct_url.json metadata.
func relocateVenv(venv string, pairs []replacement) (int, error) {
	var binFiles, otherFiles []string
	entries, err := os.ReadDir(filepath.Join(venv, "bin"))
	if err == nil {
		for _, e := range entries {
			binFiles = append(binFiles, filepath.Join(venv, "bin", e.Name()))
		}
	}
	sitePackages, _ := filepath.Glob(filepath.Join(venv, "lib", "python*", "site-packages"))
	for _, sp := range sitePackages {
		for _, pattern := range []string{"*.pth", "__editable__*.py", "*.egg-link", "*.dist-info/direct_url.json"} {
			matches, _ := filepath.Glob(filepath.Join(sp, pattern))
			otherFiles = append(otherFiles, matches...)
		}
	}
	n := 0
	for _, f := range binFiles {
		changed, err := rewriteFile(f, pairs, true)
		if err != nil {
			return n, err
		}
		if changed {
			n++
		}
	}
	for _, f := range otherFiles {
		changed, err := rewriteFile(f, pairs, false)
		if err != nil {
			return n, err
		}
		if changed {
			n++
		}
	}
	return n, nil
}

// rewriteFile replaces the paths in a regular file, reporting whether it
// changed. Symlinks are never followed. A binary file (bin/ entries) is
// left alone when it is large or holds a NUL in its first bytes.
func rewriteFile(path string, pairs []replacement, binary bool) (bool, error) {
	st, err := os.Lstat(path)
	if err != nil || !st.Mode().IsRegular() {
		return false, nil
	}
	if binary && st.Size() > venvBinMaxSize {
		return false, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	if binary {
		sniff := data
		if len(sniff) > venvBinSniff {
			sniff = sniff[:venvBinSniff]
		}
		if bytes.IndexByte(sniff, 0) >= 0 {
			return false, nil
		}
	}
	out := replacePaths(data, pairs)
	if bytes.Equal(out, data) {
		return false, nil
	}
	if err := writeFileAtomic(path, out, st.Mode().Perm()); err != nil {
		return false, err
	}
	return true, nil
}

// replacePaths applies the replacements in one pass. A match counts only
// when the byte after it is not a name character, so /repo does not match
// /repo-other or /repo2.
func replacePaths(data []byte, pairs []replacement) []byte {
	out := make([]byte, 0, len(data))
	for i := 0; i < len(data); {
		matched := false
		for _, p := range pairs {
			end := i + len(p.old)
			if len(p.old) == 0 || !bytes.HasPrefix(data[i:], p.old) {
				continue
			}
			if end < len(data) && isPathNameByte(data[end]) {
				continue
			}
			out = append(out, p.new...)
			i = end
			matched = true
			break
		}
		if !matched {
			out = append(out, data[i])
			i++
		}
	}
	return out
}

func isPathNameByte(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
		c == '.' || c == '_' || c == '-'
}

// writeFileAtomic replaces path through a temp file in the same directory,
// so a failure never leaves a truncated file.
func writeFileAtomic(path string, data []byte, perm fs.FileMode) (err error) {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".relocate-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(name)
		}
	}()
	if _, err = tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err = tmp.Close(); err != nil {
		return err
	}
	if err = os.Chmod(name, perm); err != nil {
		return err
	}
	return os.Rename(name, path)
}
