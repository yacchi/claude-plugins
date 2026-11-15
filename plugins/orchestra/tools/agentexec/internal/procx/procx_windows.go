//go:build windows

package procx

import (
	"encoding/base64"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"github.com/yacchi/claude-plugins/plugins/orchestra/tools/agentexec/internal/proctree"
	"golang.org/x/sys/windows"
)

var shellOnce = sync.OnceValue(findShell)

// ShellPath is the POSIX shell commands run through: $AGENT_EXEC_SH, else
// sh.exe on PATH, else the sh.exe of the Git for Windows that git.exe on PATH
// belongs to. "sh" when none is found, so the start fails naming it.
func ShellPath() string { return shellOnce() }

func findShell() string {
	if p := os.Getenv("AGENT_EXEC_SH"); p != "" {
		return p
	}
	if p, err := exec.LookPath("sh"); err == nil {
		return p
	}
	if git, err := exec.LookPath("git"); err == nil {
		// <root>\cmd\git.exe, <root>\bin\git.exe or <root>\mingw64\bin\git.exe
		dir := filepath.Dir(git)
		for _, root := range []string{filepath.Dir(dir), filepath.Dir(filepath.Dir(dir))} {
			for _, rel := range []string{`bin\sh.exe`, `usr\bin\sh.exe`} {
				p := filepath.Join(root, rel)
				if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() {
					return p
				}
			}
		}
	}
	return "sh"
}

// shellScript is the -c argument that makes Git's sh run script unchanged.
// Go quotes an argument by the MSVC rules, but the MSYS runtime behind sh.exe
// unquotes by its own and drops a backslash of every doubled pair, so a
// script holding a backslash (a C:\ path) arrives altered. Such a script
// travels base64-encoded, which holds neither backslashes nor quotes, and
// is decoded and evaluated by the shell itself.
func shellScript(script string) string {
	// Go leaves an argument without blanks unquoted, and MSYS globs and
	// brace-expands an unquoted one (`ls*`, `a{b,c}`), so such a script
	// travels encoded too.
	unquoted := !strings.ContainsAny(script, " \t\n\"") && strings.ContainsAny(script, "*?[{~")
	if !strings.Contains(script, `\`) && !unquoted {
		return script
	}
	return `eval "$(printf '%s' ` + base64.StdEncoding.EncodeToString([]byte(script)) + ` | base64 -d)"`
}

// Detach starts cmd in a new process group without a console window and,
// when this process sits in a job object that allows it, out of that job:
// Claude Code's own job kills its children when it closes, and a detached
// worker must outlive the session that started it, as setsid does on Unix.
func Detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.CreationFlags |= windows.CREATE_NEW_PROCESS_GROUP | windows.CREATE_NO_WINDOW | breakaway()
}

var breakaway = sync.OnceValue(func() uint32 {
	// A NULL job handle queries this process's own job, and fails when the
	// process is in none: then there is nothing to break away from.
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	err := windows.QueryInformationJobObject(0, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info)), nil)
	if err != nil {
		return 0
	}
	if info.BasicLimitInformation.LimitFlags&windows.JOB_OBJECT_LIMIT_BREAKAWAY_OK != 0 {
		return windows.CREATE_BREAKAWAY_FROM_JOB
	}
	return 0
})

// KillGroup kills the tree under pid (a Detach'ed child) and pid itself.
// Windows has no process groups and no SIGTERM for console processes, so
// hard is ignored: the tree is always terminated.
func KillGroup(pid int, hard bool) error {
	_, err := proctree.KillTree(pid, true)
	if errors.Is(err, proctree.ErrNoRoot) {
		return syscall.ESRCH
	}
	if errors.Is(err, proctree.ErrUnsupported) {
		if p, ferr := os.FindProcess(pid); ferr == nil {
			return p.Kill()
		}
	}
	return err
}

// Detached reports whether Detach was applied to cmd.
func Detached(cmd *exec.Cmd) bool {
	return cmd.SysProcAttr != nil && cmd.SysProcAttr.CreationFlags&windows.CREATE_NEW_PROCESS_GROUP != 0
}
